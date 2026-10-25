"""Package identity follows the checked commit and full Git ref."""
from pathlib import Path
import runpy
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "hack"))
metadata = runpy.run_path(
    str(Path(__file__).resolve().parents[2] / "hack/ci-metadata.py")
)["metadata"]


class MetadataTests(unittest.TestCase):
    def test_pr_tags_identify_checked_commit(self):
        result = metadata("pull_request", "Owner/Arally-Operator", "a" * 40,
                          "refs/pull/123/merge", {"number": 123})
        self.assertEqual(result, {"image": "ghcr.io/owner/arally-operator",
                                 "tag": "v0.0.0-pr.123.gaaaaaaaaaaaa",
                                 "version": "0.0.0-pr.123.gaaaaaaaaaaaa",
                                 "chart_repository": "oci://ghcr.io/owner/helm"})

    def test_numeric_sha_is_a_valid_prerelease_identifier(self):
        result = metadata("pull_request", "owner/repo", "012345678901" + "0" * 28,
                          "refs/pull/1/merge", {"number": 1})
        self.assertEqual(result["version"], "0.0.0-pr.1.g012345678901")

    def test_release_and_prerelease_tags(self):
        for version in ["1.2.3", "1.0.0-rc.1"]:
            with self.subTest(version=version):
                result = metadata("push", "owner/arally-operator", "b" * 40,
                                  "refs/tags/v" + version, {})
                self.assertEqual(result["tag"], "v" + version)
                self.assertEqual(result["version"], version)

    def test_invalid_release_refs(self):
        for ref in ["main", "v1.2.3", "refs/heads/", "refs/tags/1.2.3", "refs/tags/v01.2.3",
                    "refs/tags/v1.0.0-rc.01", "refs/tags/v1.0.0+build",
                    "refs/tags/v1.0.0-" + "x" * 64]:
            with self.subTest(ref=ref), self.assertRaises(ValueError):
                metadata("push", "owner/repo", "b" * 40, ref, {})

    def test_branch_and_manual_builds_are_not_release_versions(self):
        for event_name, ref in [("push", "refs/heads/main"),
                                ("push", "refs/heads/v1.2.3"),
                                ("workflow_dispatch", "refs/heads/main")]:
            with self.subTest(event_name=event_name, ref=ref):
                result = metadata(event_name, "owner/repo", "b" * 40, ref, {})
                self.assertEqual(result["version"], "0.0.0-dev.gbbbbbbbbbbbb")
                self.assertEqual(result["tag"], "v0.0.0-dev.gbbbbbbbbbbbb")

    def test_deleted_refs_cannot_be_packaged(self):
        for ref in ["refs/heads/main", "refs/tags/v1.2.3"]:
            with self.subTest(ref=ref), self.assertRaises(ValueError):
                metadata("push", "owner/repo", "b" * 40, ref, {"deleted": True})

    def test_unusable_identity_is_rejected(self):
        for repository, sha, event in [("owner/repo\n", "a" * 40, {"number": 1}),
                                       ("owner/repo", "abc", {"number": 1}),
                                       ("owner/repo", "a" * 40, {"number": "1"})]:
            with self.subTest(repository=repository, sha=sha), self.assertRaises(ValueError):
                metadata("pull_request", repository, sha, "refs/pull/1/merge", event)


if __name__ == "__main__":
    unittest.main()
