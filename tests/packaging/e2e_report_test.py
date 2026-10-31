"""E2E evidence must fail closed and keep raw object/output data out of CI artifacts."""

import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "hack"))
import e2e_report as report


def events(*tests, package="pass"):
    result = [{"Action": "start", "Package": "e2e"}]
    for name, action in tests:
        result.append({"Action": "run", "Test": name})
        if action:
            result.append({"Action": action, "Test": name, "Elapsed": 0.1})
    if package:
        result.append({"Action": package, "Package": "e2e"})
    return [json.dumps(event) for event in result]


class E2EEvidenceTests(unittest.TestCase):
    def finish(self, directory, lines, exit_code=0, cleanup="passed", phase="tests"):
        (directory / "tests.jsonl").write_text("\n".join(lines))
        with mock.patch.object(report, "command_json", return_value="sha256:fixture"), \
                mock.patch.object(report, "chart_identity", return_value={"name": "chart"}), \
                contextlib.redirect_stdout(io.StringIO()):
            result = report.finish(directory, phase, exit_code, "fixture:v1", "chart", cleanup)
        return result, json.loads((directory / "report.json").read_text())

    def test_empty_skipped_guard_and_incomplete_evidence_cannot_pass(self):
        cases = {
            "no matching tests": events(),
            "all skipped": events(("TestKataRuntimeLifecycle", "skip")),
            "only target guard": events(("TestE2ETargetGuard", "pass")),
            "all children skipped": events(("TestScenarioGroup", "pass"), ("TestScenarioGroup/skipped", "skip")),
            "no terminal result": events(("TestOperatorLifecycle", None)),
            "package interrupted": events(("TestOperatorLifecycle", "pass"), package=None),
            "package failed": events(("TestOperatorLifecycle", "pass"), package="fail"),
            "malformed stream": events(("TestOperatorLifecycle", "pass")) + ["invalid JSON"],
            "missing run": [line for line in events(("TestOperatorLifecycle", "pass")) if '"Action": "run"' not in line],
            "package failure cannot be overwritten": events(("TestOperatorLifecycle", "pass"), package="fail") +
                [json.dumps({"Action": "pass", "Package": "e2e"})],
        }
        for name, lines in cases.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as temporary:
                code, result = self.finish(Path(temporary), lines)
                self.assertNotEqual(code, 0)
                self.assertNotEqual(result["result"], "passed")

    def test_skipped_unselected_and_child_tests_are_reported_without_raw_output(self):
        lines = events(("TestOperatorLifecycle", "pass"), ("TestKataRuntimeLifecycle", "skip"),
                       ("TestConnectorOnlyTraffic", "pass"), ("TestConnectorOnlyTraffic/operator-identity", "pass"))
        lines.insert(1, json.dumps({"Action": "output", "Test": "TestKataRuntimeLifecycle",
                                   "Output": "credential-prompt-sse-canary"}))
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            (directory / "tests.txt").write_text("TestOperatorLifecycle\nTestKataRuntimeLifecycle\nTestConnectorOnlyTraffic\nTestSnapshotWorkspaceRestore\n")
            code, result = self.finish(directory, lines)
            self.assertEqual(code, 0)
            tests = {test["name"]: test["status"] for test in result["tests"]}
            self.assertEqual(tests["TestKataRuntimeLifecycle"], "skipped")
            self.assertEqual(tests["TestSnapshotWorkspaceRestore"], "not-run")
            self.assertEqual(tests["TestConnectorOnlyTraffic/operator-identity"], "passed")
            self.assertEqual(result["executedScenarios"], 2)
            for name in ("report.json", "summary.md"):
                self.assertNotIn("credential-prompt-sse-canary", (directory / name).read_text())

    def test_failure_codes_and_cleanup_failures_are_preserved(self):
        for original in (2, 17, 130, 143):
            with self.subTest(code=original), tempfile.TemporaryDirectory() as temporary:
                code, result = self.finish(Path(temporary), ["invalid JSON"], original, "failed")
                self.assertEqual(code, original)
                self.assertEqual(result["processExitCode"], original)
                self.assertEqual(result["result"], "failed")
        with tempfile.TemporaryDirectory() as temporary:
            code, result = self.finish(Path(temporary), events(("TestOperatorLifecycle", "pass")), cleanup="failed")
            self.assertNotEqual(code, 0)
            self.assertEqual(result["cleanup"], "failed")

    def test_inventory_publishes_whitelisted_fields_only(self):
        canary = "sensitive-resource-and-api-message-canary"
        def read(args, timeout=5):
            if "version" in args:
                return {"serverVersion": {"gitVersion": "v1.test"}, "apiServer": canary}
            item = {"metadata": {"name": "kindnet", "annotations": {"private": canary}},
                    "handler": "kata-test", "provisioner": "csi.example.com", "data": {"token": canary},
                    "spec": {"containers": [{"name": "agent", "image": "fixture:v1", "env": [{"value": canary}]}],
                             "template": {"spec": {"containers": [{"name": "agent", "image": "fixture:v1", "env": [{"value": canary}]}]}}},
                    "status": {"nodeInfo": {"architecture": "amd64", "containerRuntimeVersion": "containerd://test"},
                               "addresses": [canary], "containerStatuses": [{"name": "agent", "image": "docker.io/library/fixture:v1", "imageID": "sha256:fixture", "message": canary}]}}
            return {"items": [item]}
        with mock.patch.object(report, "command_json", side_effect=read):
            result = report.inventory()
        self.assertNotIn(canary, json.dumps(result))
        self.assertEqual(result["runtimeClasses"][0]["handler"], "kata-test")
        self.assertEqual(result["storageClasses"][0]["provisioner"], "csi.example.com")
        self.assertEqual(result["operator"]["runningImages"][0]["imageID"], "sha256:fixture")
        failure = subprocess.CalledProcessError(1, ["kubectl"], stderr=canary)
        with mock.patch.object(subprocess, "run", side_effect=failure):
            self.assertIsNone(report.command_json(["kubectl"]))

    def test_setup_retries_never_reuse_old_success_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / "tools"
            tools.mkdir()
            for name, code in (("docker", 17), ("kind", 0), ("kubectl", 0),
                               ("helm", 0), ("go", 0), ("curl", 0), ("timeout", 0)):
                tool = tools / name
                tool.write_text(f"#!/bin/sh\nexit {code}\n")
                tool.chmod(0o755)
            logs = root / "evidence"
            logs.mkdir()
            (logs / "report.json").write_text('{"result":"passed"}')
            (logs / "tests.jsonl").write_text("\n".join(events(("TestKataRuntimeLifecycle", "pass"))))
            env = {**os.environ, "PATH": str(tools) + os.pathsep + os.environ["PATH"],
                   "KIND": str(tools / "kind"), "E2E_IMAGE": "fixture:v1", "E2E_LOG_DIR": str(logs),
                   "E2E_CNI": "kindnet", "E2E_SNAPSHOTS": "false",
                   "E2E_CHART": str(ROOT / "chart")}
            for key in ("E2E_KIND_CONFIG", "E2E_RUNTIME_MANIFEST", "E2E_RUNTIME_CLASS"):
                env.pop(key, None)
            for _ in range(2):
                run = subprocess.run(["bash", "hack/e2e.sh"], cwd=ROOT, env=env, capture_output=True, timeout=15)
                self.assertEqual(run.returncode, 17, run.stderr.decode())
            reports = sorted(logs.glob("*/report.json"))
            self.assertEqual(len(reports), 2)
            for path in reports:
                result = json.loads(path.read_text())
                self.assertEqual(result["result"], "failed")
                self.assertEqual(result["phase"], "build")
                self.assertEqual(result["tests"], [])


if __name__ == "__main__":
    unittest.main()
