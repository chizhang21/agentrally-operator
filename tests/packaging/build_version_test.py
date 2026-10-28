"""Invalid package versions must fail before changing files."""
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "hack"))
from build_version import stage_chart


class BuildVersionTests(unittest.TestCase):
    def test_invalid_versions_fail_before_staging(self):
        for version in ["", "v1.2.3", "01.2.3", "1.2.3-pr.01", "1.2.3+build", "1.2.3-rc-", "1.2.3;touch x"]:
            with self.subTest(version=version), tempfile.TemporaryDirectory() as directory:
                target = Path(directory) / "chart"
                with self.assertRaises(ValueError):
                    stage_chart(Path("chart"), target, version, "registry.example/operator:v1.2.3")
                self.assertFalse(target.exists())


if __name__ == "__main__":
    unittest.main()
