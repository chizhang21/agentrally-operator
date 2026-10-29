#!/usr/bin/env python3
"""Record E2E results without treating discovery or skipped tests as execution proof."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time


CHILD_CASES = {
    "TestPersistentWorkspaceFoundation": ("Foreground", "Background"),
    "TestTaskCompletionPolicies": ("successful", "failed-retained", "failed-expired"),
    "TestConnectorOnlyTraffic": ("operator-identity", "wrong-pod", "wrong-namespace"),
}
SKIP_REASONS = {
    "TestKataRuntimeLifecycle": "Requires E2E_RUNTIME_CLASS and an installed Kata runtime",
    "TestNetworkPolicyTraffic": "Requires E2E_CNI=cilium",
    "TestNativeEgressTraffic": "Requires E2E_CNI=cilium",
    "TestConnectorOnlyTraffic": "Requires E2E_CNI=cilium",
    "TestSnapshotWorkspaceRestore": "Requires E2E_SNAPSHOTS=true",
    "TestOpenClawLifecycle": "Requires the opt-in OpenClaw example profile",
}
PROFILES = {
    "kata": "TestKataRuntimeLifecycle",
    "networkPolicy": "TestNetworkPolicyTraffic",
    "nativeEgress": "TestNativeEgressTraffic",
    "connectorOnly": "TestConnectorOnlyTraffic",
    "snapshotRestore": "TestSnapshotWorkspaceRestore",
}


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def command_json(args, timeout=5):
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=timeout, check=True)
        return json.loads(result.stdout)
    except (OSError, ValueError, subprocess.SubprocessError):
        # Native API errors can quote object values and addresses.
        return None


def inventory():
    deadline = time.monotonic() + 20

    def read(*args):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return None
        return command_json(["kubectl", "--request-timeout=4s", *args, "-o", "json"], min(5, remaining))

    def items(resource, *args):
        result = read("get", resource, *args)
        return None if result is None else result.get("items", [])

    def named(resource, *fields):
        values = items(resource)
        if values is None:
            return None
        return [{"name": item.get("metadata", {}).get("name"),
                 **{field: item.get(field) for field in fields}} for item in values]

    version = read("version")
    nodes = items("nodes")
    environment = {
        "observedAt": utc_now(),
        "kubernetes": None if version is None else version.get("serverVersion", {}).get("gitVersion"),
        "nodes": None if nodes is None else [
            {key: node.get("status", {}).get("nodeInfo", {}).get(key) for key in
             ("operatingSystem", "osImage", "architecture", "kubeletVersion", "containerRuntimeVersion")}
            for node in nodes
        ],
        "runtimeClasses": named("runtimeclasses", "handler"),
        "storageClasses": named("storageclasses", "provisioner"),
        "csiDrivers": named("csidrivers"),
    }
    for role, namespace in (("operator", "agentrally-system"), ("agentSandbox", "agent-sandbox-system")):
        deployments = items("deployments", "-n", namespace)
        pods = items("pods", "-n", namespace)
        images = None if deployments is None else sorted({
            container["image"] for deployment in deployments
            for container in deployment.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
        })
        environment[role] = {
            "declaredImages": images,
            "runningImages": None if pods is None else [
                {"image": container.get("image"), "imageID": container.get("imageID")}
                for pod in pods for container in pod.get("status", {}).get("containerStatuses", [])
                if any(spec.get("name") == container.get("name") and spec.get("image") in (images or [])
                       for spec in pod.get("spec", {}).get("containers", []))
            ],
        }
    daemons = items("daemonsets", "-n", "kube-system")
    environment["cni"] = None if daemons is None else [
        {"name": daemon["metadata"]["name"], "images": [c.get("image") for c in daemon.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])]}
        for daemon in daemons if daemon.get("metadata", {}).get("name") in ("cilium", "kindnet")
    ]
    return environment


def stream(directory, source):
    valid = True
    with (directory / "tests.jsonl").open("w") as events, (directory / "lifecycle.log").open("w") as log:
        for line in source:
            events.write(line)
            try:
                event = json.loads(line)
                output = event.get("Output", "")
                if isinstance(output, str):
                    log.write(output)
                    print(output, end="", flush=True)
            except (ValueError, AttributeError):
                valid = False
    return 0 if valid else 1


def test_results(lines):
    tests, packages = {}, {}
    started = set()
    started_packages = set()
    valid = True
    for line in lines:
        try:
            event = json.loads(line)
            action, name = event.get("Action"), event.get("Test")
            if name:
                test = tests.setdefault(name, {"name": name, "status": "incomplete"})
                if action == "run":
                    valid = valid and name not in started
                    started.add(name)
                if action in ("pass", "fail", "skip"):
                    valid = valid and name in started and test["status"] == "incomplete"
                    test.update(status={"pass": "passed", "fail": "failed", "skip": "skipped"}[action], elapsedSeconds=event.get("Elapsed", 0))
            elif action in ("start", "pass", "fail", "skip") and event.get("Package"):
                if action == "start":
                    valid = valid and event["Package"] not in started_packages
                    started_packages.add(event["Package"])
                else:
                    valid = valid and event["Package"] in started_packages and packages.get(event["Package"]) == "start"
                packages[event["Package"]] = action
        except (ValueError, AttributeError):
            valid = False
    for name, test in tests.items():
        if test["status"] == "skipped":
            test["reason"] = SKIP_REASONS.get(name, "TestSkipped; inspect local logs")
    complete = valid and bool(packages) and all(value == "pass" for value in packages.values())
    complete = complete and all(test["status"] != "incomplete" for test in tests.values())
    executed = [test for name, test in tests.items() if "/" not in name and name != "TestE2ETargetGuard" and
                test["status"] == "passed" and (name not in CHILD_CASES or any(
                    tests.get(name + "/" + child, {}).get("status") == "passed" for child in CHILD_CASES[name]))]
    profiles = {}
    for profile, name in PROFILES.items():
        state = tests.get(name, {"status": "not-run"})["status"]
        cases = {child: tests.get(name + "/" + child, {"status": "not-run"})["status"] for child in CHILD_CASES.get(name, ())}
        if state == "passed" and cases and not all(status == "passed" for status in cases.values()):
            state = "incomplete"
        profiles[profile] = {"status": state, "test": name, "cases": cases,
                             "reason": SKIP_REASONS.get(name, "TestSkipped") if state == "skipped" else
                             "TestNotRun" if state == "not-run" else
                             "RequiredSubcasesNotVerified" if state == "incomplete" else state}
    return {"tests": list(tests.values()), "profiles": profiles,
            "complete": complete, "executedScenarios": len(executed)}


def chart_identity(chart):
    path = Path(chart)
    identity = {"name": path.name, "archiveSHA256": None, "metadata": {}}
    if path.is_file():
        identity["archiveSHA256"] = hashlib.sha256(path.read_bytes()).hexdigest()
    try:
        metadata = subprocess.run(["helm", "show", "chart", chart], capture_output=True,
                                  text=True, timeout=5, check=True).stdout
        # Only Helm's three scalar identity fields are retained, never arbitrary values.
        for line in metadata.splitlines():
            key, separator, value = line.partition(":")
            if separator and key in ("name", "version", "appVersion"):
                identity["metadata"][key] = value.strip().strip("\"'")
    except (OSError, subprocess.SubprocessError):
        pass
    return identity


def finish(directory, phase, exit_code, image, chart, cleanup):
    events = directory / "tests.jsonl"
    evidence = test_results(events.read_text().splitlines() if events.exists() else [])
    failed = any(test["status"] == "failed" for test in evidence["tests"])
    passed = exit_code == 0 and phase == "tests" and cleanup == "passed" and evidence["complete"] and evidence["executedScenarios"] > 0 and not failed
    result = "passed" if passed else "failed" if exit_code != 0 or failed or cleanup == "failed" else "incomplete"
    code = exit_code if exit_code else 0 if passed else 1
    try:
        source = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=True).stdout.strip()
        dirty = bool(subprocess.run(["git", "status", "--porcelain"], capture_output=True, text=True, check=True).stdout)
    except (OSError, subprocess.SubprocessError):
        source, dirty = None, None
    environment_path = directory / "environment.json"
    environment = json.loads(environment_path.read_text()) if environment_path.exists() else {"unavailable": True}
    report = {"schemaVersion": "e2e-report/v1", "runID": directory.name, "startedAt": os.environ.get("E2E_STARTED_AT"), "finishedAt": utc_now(),
              "result": result, "phase": phase, "processExitCode": exit_code, "exitCode": code, "cleanup": cleanup,
              "sourceCommit": source, "uncommittedChanges": dirty, "image": image,
              "imageSource": os.environ.get("E2E_IMAGE_SOURCE", "provided-image"),
              "imageID": command_json(["docker", "image", "inspect", "--format", "{{json .Id}}", image]),
              "chart": chart_identity(chart), "selection": os.environ.get("E2E_TEST", "."),
              "requestedProfile": {"cni": os.environ.get("E2E_CNI"), "runtimeClass": os.environ.get("E2E_RUNTIME_CLASS"),
                                   "storageClass": os.environ.get("E2E_KATA_STORAGE_CLASS"),
                                   "snapshots": os.environ.get("E2E_SNAPSHOTS") == "true"},
              "environment": environment, **evidence}
    write_json(directory / "report.json", report)
    rows = ["# AgentRally Operator E2E", "", f"Result: {result}. Phase: {phase}. Exit: {code}.",
            f"Finished: {report['finishedAt']}. Cleanup: {cleanup}.", "",
            "| Test | Result | Seconds | Reason |", "| --- | --- | --- | --- |"]
    rows += [f"| {test['name']} | {test['status']} | {test.get('elapsedSeconds', '')} | {test.get('reason', '')} |" for test in evidence["tests"]]
    rows += ["", "| Optional check | Result | Reason |", "| --- | --- | --- |"]
    rows += [f"| {name} | {value['status']} | {value['reason']} |" for name, value in evidence["profiles"].items()]
    rows += ["", "Skipped or absent checks are not verification. Environment discovery is configuration evidence.",
             "The source commit identifies the test checkout; provided images have independent provenance.",
             "Real Orchestrator integration, fencing and the joint resource budget remain separate gates."]
    (directory / "summary.md").write_text("\n".join(rows) + "\n")
    print(f"E2E evidence: {directory / 'report.json'} ({result})")
    return code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("inventory", "stream", "finish"))
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--phase", default="setup")
    parser.add_argument("--exit-code", type=int, default=1)
    parser.add_argument("--image", default="")
    parser.add_argument("--chart", default="chart")
    parser.add_argument("--cleanup", default="not-created")
    args = parser.parse_args()
    if args.mode == "inventory":
        write_json(args.directory / "environment.json", inventory())
        return 0
    if args.mode == "stream":
        return stream(args.directory, sys.stdin)
    return finish(args.directory, args.phase, args.exit_code, args.image, args.chart, args.cleanup)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, TypeError):
        print("Cannot produce complete E2E evidence; inspect local diagnostics.", file=sys.stderr)
        sys.exit(1)
