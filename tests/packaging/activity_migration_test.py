import contextlib
import copy
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    "migrate_activity", Path(__file__).resolve().parents[2] / "hack/migrate_activity.py"
)
migration = importlib.util.module_from_spec(spec)
spec.loader.exec_module(migration)


class FakeKubectl:
    def __init__(self, mode="Running", suspended=False):
        self.object = {
            "metadata": {"namespace": "agents", "name": "agent", "uid": "instance-1", "resourceVersion": "1"},
            "spec": {"operatingMode": mode, "lifecycle": {"idleTimeoutSeconds": 60, "activityLeaseRef": {"name": "activity"}}, "unrelated": "preserved"},
            "status": {"activity": {"suspended": suspended}},
        }
        self.deployment = {
            "metadata": {"uid": "deployment-1", "generation": 1},
            "spec": {"replicas": 0, "selector": {"matchLabels": {"app": "operator"}}},
            "status": {"observedGeneration": 1},
        }
        self.pods = []
        self.writes = 0
        self.conflict = False

    def get(self, resource, name=None, namespace=None):
        if resource == "deployment":
            return copy.deepcopy(self.deployment)
        if resource == "replicasets":
            return {"items": []}
        if resource == "pods":
            return {"items": copy.deepcopy(self.pods)}
        if resource != "agentinstances.arally.io":
            raise AssertionError(f"Unexpected resource: {resource}")
        return copy.deepcopy(self.object) if name else {"items": [copy.deepcopy(self.object)]}

    def patch(self, namespace, name, operations):
        if self.conflict:
            self.object["metadata"]["resourceVersion"] = "2"
        current = copy.deepcopy(self.object)
        for operation in operations:
            keys = [key.replace("~1", "/").replace("~0", "~") for key in operation["path"].split("/")[1:]]
            parent = current
            for key in keys[:-1]:
                parent = parent[key]
            key = keys[-1]
            if operation["op"] == "test":
                if parent[key] != operation["value"]:
                    raise migration.MigrationError("UID/RV conflict")
            elif operation["op"] == "remove":
                del parent[key]
            else:
                parent[key] = copy.deepcopy(operation["value"])
        self.object = current
        self.object["metadata"]["resourceVersion"] = "3"
        self.writes += 1
        return copy.deepcopy(self.object)


class MemoryReceipts:
    def __init__(self, crash_after_write=False):
        self.entries = []
        self.crash_after_write = crash_after_write

    def append(self, phase, receipt):
        if phase == "applied" and self.crash_after_write:
            raise migration.MigrationError("simulated receipt write failure")
        self.entries.append((phase, copy.deepcopy(receipt)))


class ActivityMigrationTests(unittest.TestCase):
    def run_migration(self, kube, apply=True, receipts=None):
        with contextlib.redirect_stdout(io.StringIO()):
            return migration.migrate(kube, "system", "operator", apply, receipts)

    def test_preserves_effective_intent_and_unrelated_configuration(self):
        cases = [("Running", True, "Suspended"), ("Suspended", False, "Suspended"), ("Running", False, "Running"), ("Suspended", True, "Suspended"), (None, False, "Running")]
        for mode, suspended, expected in cases:
            with self.subTest(mode=mode, suspended=suspended):
                kube = FakeKubectl(mode, suspended)
                # A stale signal does not invalidate the already latched intent.
                kube.object["status"]["activity"]["validUntil"] = "2000-01-01T00:00:00Z"
                original = copy.deepcopy(kube.object)
                self.assertEqual(1, self.run_migration(kube, apply=False))
                self.assertEqual(original, kube.object)
                receipts = MemoryReceipts()
                self.assertEqual(1, self.run_migration(kube, receipts=receipts))
                self.assertEqual(expected, kube.object["spec"]["operatingMode"])
                self.assertEqual("preserved", kube.object["spec"]["unrelated"])
                self.assertEqual({}, kube.object["spec"]["lifecycle"])
                self.assertEqual(["planned", "applied"], [phase for phase, _ in receipts.entries])
                self.assertEqual(0, self.run_migration(kube, receipts=receipts))
                self.assertEqual(1, kube.writes)

    def test_write_before_receipt_crash_does_not_override_later_admin_intent(self):
        kube = FakeKubectl(suspended=True)
        with self.assertRaises(migration.MigrationError):
            self.run_migration(kube, receipts=MemoryReceipts(crash_after_write=True))
        self.assertEqual("true", kube.object["metadata"]["annotations"][migration.MARKER])
        kube.object["spec"]["operatingMode"] = "Running"
        self.assertEqual(0, self.run_migration(kube, receipts=MemoryReceipts()))
        self.assertEqual("Running", kube.object["spec"]["operatingMode"])
        self.assertEqual(1, kube.writes)

    def test_conflict_leaves_intent_and_receipt_recoverable(self):
        kube = FakeKubectl(suspended=True)
        kube.conflict = True
        receipts = MemoryReceipts()
        with self.assertRaises(migration.MigrationError):
            self.run_migration(kube, receipts=receipts)
        self.assertEqual(0, kube.writes)
        self.assertEqual("Running", kube.object["spec"]["operatingMode"])
        self.assertEqual(["planned"], [phase for phase, _ in receipts.entries])

    def test_running_or_terminating_operator_blocks_changes(self):
        for terminating in (False, True):
            kube = FakeKubectl()
            if terminating:
                kube.pods = [{"metadata": {"labels": {"app": "operator"}, "deletionTimestamp": "2026-10-28T00:00:00Z"}}]
            else:
                kube.deployment["spec"]["replicas"] = 1
            with self.assertRaises(migration.MigrationError):
                self.run_migration(kube, receipts=MemoryReceipts())
            self.assertEqual(0, kube.writes)

    def test_reintroduced_legacy_fields_require_review(self):
        kube = FakeKubectl()
        kube.object["metadata"]["annotations"] = {migration.MARKER: "true"}
        with self.assertRaises(migration.MigrationError):
            self.run_migration(kube, receipts=MemoryReceipts())
        self.assertEqual(0, kube.writes)

    def test_receipt_is_durable_private_and_contains_only_migration_data(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.jsonl"
            receipts = migration.Receipts(path)
            try:
                self.run_migration(FakeKubectl(), receipts=receipts)
            finally:
                receipts.close()
            self.assertEqual(0o600, path.stat().st_mode & 0o777)
            self.assertEqual(2, len(path.read_text().splitlines()))
            self.assertNotIn("unrelated", path.read_text())


if __name__ == "__main__":
    unittest.main()
