#!/usr/bin/env python3
"""Preserve explicit instance intent before upgrading past Activity Lease support."""

import argparse
import json
import os
from pathlib import Path
import stat
import subprocess
import sys


MARKER = "arally.io/activity-migrated"
LEGACY_FIELDS = ("idleTimeoutSeconds", "activityLeaseRef")


class MigrationError(Exception):
    pass


class Kubectl:
    def __init__(self, context):
        self.context = context

    def run(self, *args):
        result = subprocess.run(
            ["kubectl", "--context", self.context, *args],
            text=True, capture_output=True, check=False,
        )
        if result.returncode:
            # Native errors can quote object content. Report only the operation.
            raise MigrationError(f"kubectl {args[0]} failed; inspect the resource separately")
        return json.loads(result.stdout)

    def get(self, resource, name=None, namespace=None):
        args = ["get", resource]
        if name:
            args.append(name)
        args += ["-n", namespace] if namespace else ["-A"]
        return self.run(*args, "-o", "json")

    def patch(self, namespace, name, operations):
        return self.run(
            "patch", "agentinstances.arally.io", name, "-n", namespace,
            "--type=json", "-p", json.dumps(operations), "-o", "json",
        )


def owned_by(obj, kind, uid):
    return any(
        ref.get("controller") is True and ref.get("kind") == kind and ref.get("uid") == uid
        for ref in obj.get("metadata", {}).get("ownerReferences", [])
    )


def selector_matches(labels, selector):
    if not selector.get("matchLabels") and not selector.get("matchExpressions"):
        raise MigrationError("Operator Deployment must have a nonempty Pod selector")
    if any(labels.get(key) != value for key, value in selector.get("matchLabels", {}).items()):
        return False
    for expression in selector.get("matchExpressions", []):
        key, operator = expression["key"], expression["operator"]
        values = expression.get("values", [])
        matches = {
            "In": key in labels and labels[key] in values,
            "NotIn": key not in labels or labels[key] not in values,
            "Exists": key in labels,
            "DoesNotExist": key not in labels,
        }
        if operator not in matches:
            raise MigrationError("Unsupported Operator Pod selector")
        if not matches[operator]:
            return False
    return True


def require_stopped(kube, namespace, deployment, expected_uid=None):
    obj = kube.get("deployment", deployment, namespace)
    metadata, spec = obj["metadata"], obj["spec"]
    uid = metadata.get("uid")
    if not uid or expected_uid and uid != expected_uid:
        raise MigrationError("Operator Deployment identity changed")
    if spec.get("replicas", 1) != 0 or obj.get("status", {}).get("observedGeneration", 0) < metadata.get("generation", 1):
        raise MigrationError("Scale the old Operator to zero and wait for the Deployment to observe it")
    selector = spec.get("selector") or {}
    if not selector.get("matchLabels") and not selector.get("matchExpressions"):
        raise MigrationError("Operator Deployment must have a nonempty Pod selector")
    sets = kube.get("replicasets", namespace=namespace).get("items", [])
    owned = [item for item in sets if owned_by(item, "Deployment", uid)]
    if any(item.get("spec", {}).get("replicas", 1) != 0 for item in owned):
        raise MigrationError("An old Operator ReplicaSet still requests Pods")
    set_uids = {item["metadata"]["uid"] for item in owned}
    for pod in kube.get("pods", namespace=namespace).get("items", []):
        if selector_matches(pod["metadata"].get("labels", {}), selector) or any(owned_by(pod, "ReplicaSet", item) for item in set_uids):
            raise MigrationError("Wait for every old Operator Pod, including terminating Pods, to disappear")
    return uid


def migration_plan(obj):
    metadata, spec = obj["metadata"], obj["spec"]
    annotations = metadata.get("annotations") or {}
    lifecycle = spec.get("lifecycle") or {}
    legacy = {key: lifecycle[key] for key in LEGACY_FIELDS if key in lifecycle}
    if annotations.get(MARKER) == "true":
        if legacy:
            raise MigrationError("Activity fields reappeared on an already migrated instance")
        return None
    if not legacy and obj.get("status", {}).get("activity") is None:
        return None
    mode = spec.get("operatingMode") or "Running"
    if mode not in ("Running", "Suspended"):
        raise MigrationError("Instance has an unsupported operatingMode")
    suspended = (obj.get("status", {}).get("activity") or {}).get("suspended") is True
    desired = "Suspended" if mode == "Suspended" or suspended else "Running"
    if not metadata.get("uid") or not metadata.get("resourceVersion"):
        raise MigrationError("Instance UID and resourceVersion are required")
    patch = [
        {"op": "test", "path": "/metadata/uid", "value": metadata["uid"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": metadata["resourceVersion"]},
        {"op": "add", "path": "/spec/operatingMode", "value": desired},
    ]
    patch += [{"op": "remove", "path": f"/spec/lifecycle/{key}"} for key in legacy]
    if not metadata.get("annotations"):
        patch.append({"op": "add", "path": "/metadata/annotations", "value": {MARKER: "true"}})
    else:
        patch.append({"op": "add", "path": "/metadata/annotations/arally.io~1activity-migrated", "value": "true"})
    receipt = {
        "namespace": metadata["namespace"], "name": metadata["name"], "uid": metadata["uid"],
        "resourceVersion": metadata["resourceVersion"], "oldMode": mode, "newMode": desired,
        "legacy": legacy, "marker": MARKER,
    }
    return patch, receipt


class Receipts:
    def __init__(self, path):
        path = Path(path)
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            os.close(fd)
            raise MigrationError("Receipt must be a regular file with mode 0600")
        # Persist the directory entry as well as each record before any patch.
        parent_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(parent_fd)
        except OSError:
            os.close(fd)
            raise
        finally:
            os.close(parent_fd)
        self.file = os.fdopen(fd, "a", encoding="utf-8")

    def append(self, phase, receipt):
        self.file.write(json.dumps({"phase": phase, **receipt}, sort_keys=True) + "\n")
        self.file.flush()
        os.fsync(self.file.fileno())

    def close(self):
        self.file.close()


def migrate(kube, namespace, deployment, apply=False, receipts=None):
    if apply and receipts is None:
        raise MigrationError("Applying a migration requires a durable receipt file")
    deployment_uid = require_stopped(kube, namespace, deployment) if apply else None
    items = kube.get("agentinstances.arally.io").get("items", [])
    migrated = 0
    for listed in items:
        name, ns = listed["metadata"]["name"], listed["metadata"]["namespace"]
        obj = kube.get("agentinstances.arally.io", name, ns)
        if obj["metadata"].get("uid") != listed["metadata"].get("uid"):
            raise MigrationError("Instance identity changed after inventory; repeat the dry run")
        planned = migration_plan(obj)
        if planned is None:
            continue
        operations, receipt = planned
        if apply:
            require_stopped(kube, namespace, deployment, deployment_uid)
            receipts.append("planned", receipt)
            result = kube.patch(ns, name, operations)
            if result["metadata"].get("uid") != receipt["uid"] or result["metadata"].get("annotations", {}).get(MARKER) != "true":
                raise MigrationError("Migration result could not be confirmed; inspect the receipt and current object")
            receipts.append("applied", receipt)
        print(f"{'migrated' if apply else 'planned'} {ns}/{name}: {receipt['newMode']}")
        migrated += 1
    return migrated


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--namespace", required=True, help="Operator Deployment namespace")
    parser.add_argument("--deployment", required=True)
    parser.add_argument("--apply", action="store_true", help="Apply after pausing old Operator and external writers")
    parser.add_argument("--receipt", help="Required with --apply; append-only local receipt file")
    args = parser.parse_args()
    if args.apply and not args.receipt:
        parser.error("--apply requires --receipt")
    receipts = None
    try:
        if args.apply:
            receipts = Receipts(args.receipt)
        migrate(Kubectl(args.context), args.namespace, args.deployment, args.apply, receipts)
    except (MigrationError, OSError, ValueError, KeyError, TypeError) as error:
        # Do not print subprocess output or the resource body on failure.
        print(str(error) if isinstance(error, MigrationError) else "Migration failed; inspect the receipt and resource identities", file=sys.stderr)
        return 1
    finally:
        if receipts is not None:
            receipts.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
