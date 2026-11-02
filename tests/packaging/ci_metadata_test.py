"""Release, PR and branch builds must not share a package identity."""
from pathlib import Path
import runpy
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "hack"))
metadata = runpy.run_path(
    str(Path(__file__).resolve().parents[2] / "hack/ci-metadata.py")
)["metadata"]


class PackageIdentityTests(unittest.TestCase):
    def test_pr_uses_checked_commit_and_valid_numeric_sha(self):
        result = metadata("pull_request", "Owner/AgentRally-Operator", "012345678901" + "0" * 28,
                          "refs/pull/123/merge", {"number": 123})
        self.assertEqual(result, {"image": "ghcr.io/owner/agentrally-operator",
                                 "tag": "v0.0.0-pr.123.g012345678901",
                                 "version": "0.0.0-pr.123.g012345678901",
                                 "chart_repository": "oci://ghcr.io/owner/helm"})

    def test_release_and_prerelease_tags(self):
        for version in ["1.2.3", "1.0.0-rc.1"]:
            with self.subTest(version=version):
                result = metadata("push", "owner/agentrally-operator", "b" * 40,
                                  "refs/tags/v" + version, {})
                self.assertEqual(result["tag"], "v" + version)
                self.assertEqual(result["version"], version)

    def test_branch_and_manual_builds_are_not_release_versions(self):
        for event_name, ref in [("push", "refs/heads/v1.2.3"),
                                ("workflow_dispatch", "refs/heads/main")]:
            with self.subTest(event_name=event_name, ref=ref):
                result = metadata(event_name, "owner/repo", "b" * 40, ref, {})
                self.assertEqual(result["version"], "0.0.0-dev.gbbbbbbbbbbbb")
                self.assertEqual(result["tag"], "v0.0.0-dev.gbbbbbbbbbbbb")


if __name__ == "__main__":
    unittest.main()
