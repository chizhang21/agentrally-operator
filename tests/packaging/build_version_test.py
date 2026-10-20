"""Version injection must preserve sources and reject unusable package identities."""
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "hack"))
from build_version import stage_chart, validate_version


class BuildVersionTests(unittest.TestCase):
    def test_dev_chart_pulls_mutable_images_without_changing_sources(self):
        source = Path(__file__).resolve().parents[2] / "chart"
        original = (source / "values.yaml").read_bytes()
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "chart"
            stage_chart(source, target, "0.0.0-dev", "registry.example/operator:v0.0.0-dev")
            self.assertIn('  pullPolicy: "Always"', (target / "values.yaml").read_text())
        self.assertEqual(original, (source / "values.yaml").read_bytes())

    def test_invalid_versions_fail_before_staging(self):
        for version in ["", "v1.2.3", "01.2.3", "1.2.3-pr.01", "1.2.3+build", "1.2.3-rc-", "1.2.3;touch x"]:
            with self.subTest(version=version), tempfile.TemporaryDirectory() as directory:
                target = Path(directory) / "chart"
                with self.assertRaises(ValueError):
                    stage_chart(Path("chart"), target, version, "registry.example/operator:v1.2.3")
                self.assertFalse(target.exists())

    def test_digest_injection_keeps_source_defaults(self):
        root = Path(__file__).resolve().parents[2]
        source = root / "chart"
        metadata = (source / "Chart.yaml").read_bytes()
        values = (source / "values.yaml").read_bytes()
        digest = "sha256:" + "a" * 64
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "chart"
            stage_chart(source, target, "1.2.3", "registry.example:5000/operator@" + digest)
            staged = (target / "values.yaml").read_text()
            self.assertIn('  repository: "registry.example:5000/operator"', staged)
            self.assertIn('  digest: "' + digest + '"', staged)
            self.assertRegex(staged, r'(?m)^  tag: ""$')
            self.assertIn('version: "1.2.3"', (target / "Chart.yaml").read_text())
        self.assertEqual(metadata, (source / "Chart.yaml").read_bytes())
        self.assertEqual(values, (source / "values.yaml").read_bytes())


if __name__ == "__main__":
    unittest.main()
