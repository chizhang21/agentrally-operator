"""Separate make commands must share the selected package identity."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class BuildStateTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        source = Path(__file__).resolve().parents[2]
        shutil.copy(source / "Makefile", self.root)
        for folder in ("hack", "mk"):
            shutil.copytree(source / folder, self.root / folder)
        self.env = {key: value for key, value in os.environ.items()
                    if key not in {"VERSION", "IMG", "IMAGE_TAG_BASE", "CHART_OCI_REPO",
                                   "MAKEFLAGS", "MFLAGS", "MAKELEVEL"}}
        self.container = self.root / "container"
        self.container.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> calls\n')
        self.container.chmod(0o755)

    def make(self, *args, success=True):
        result = subprocess.run(
            ["make", "--no-print-directory", "-s", *args, f"CONTAINER_TOOL={self.container}"],
            cwd=self.root, env=self.env, text=True, capture_output=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result.stdout

    def state(self):
        return json.loads((self.root / ".cache/build.json").read_text())

    def test_separate_build_push_and_chart_reuse_identity(self):
        self.make("docker-build", "IMAGE_TAG_BASE=registry.example:5000/operator")
        selected = self.state()
        self.make("docker-push")
        calls = (self.root / "calls").read_text().splitlines()
        self.assertEqual(calls, [f'build -f Dockerfile -t {selected["image"]} .',
                                 f'push {selected["image"]}'])
        package = self.root / "hack/package-chart.sh"
        package.write_text('printf "%s %s\\n" "$VERSION" "$1" > packaged\n')
        self.make("chart-package")
        self.assertEqual((self.root / "packaged").read_text().strip(),
                         f'{selected["version"]} {selected["image"]}')
        self.assertEqual(self.state(), selected)

    def test_version_saves_fixed_dev_identity_and_keeps_repository(self):
        self.make("version", "IMAGE_TAG_BASE=registry.example/operator")
        old = self.state()
        self.make("docker-build")
        self.assertEqual(self.state(), old)
        self.make("version", "docker-build")
        new = self.state()
        self.assertEqual(old, new)
        self.assertEqual(new["version"], "0.0.0-dev")
        self.assertEqual(new["image"], "registry.example/operator:v" + new["version"])
        self.assertIn(new["image"], (self.root / "calls").read_text())
        self.make("docker-push")
        self.assertEqual(self.state(), new)
        self.assertEqual((self.root / "calls").read_text().splitlines()[-1], f'push {new["image"]}')

    def test_explicit_overrides_and_clean(self):
        self.make("version", "VERSION=1.2.3", "IMG=registry.example/operator:custom")
        self.assertEqual(self.state(), {"version": "1.2.3", "image": "registry.example/operator:custom",
                                       "chart_oci_repo": "oci://ghcr.io/chizhang21/helm"})
        self.make("version", "VERSION=2.0.0")
        self.assertEqual(self.state()["image"], "registry.example/operator:v2.0.0")
        self.make("docker-build", "IMAGE_TAG_BASE=other.example/operator")
        self.assertEqual(self.state()["image"], "other.example/operator:v2.0.0")
        self.make("clean")
        self.make("version")
        self.assertEqual(self.state()["version"], "0.0.0-dev")
        self.assertTrue(self.state()["image"].startswith("agentrally-operator:"))

    def test_invalid_override_preserves_saved_identity(self):
        self.make("version", "VERSION=1.2.3")
        selected = self.state()
        self.make("docker-push", "VERSION=bad", success=False)
        self.make("docker-push", "IMG=untagged", success=False)
        self.assertEqual(self.state(), selected)
        self.assertFalse((self.root / "calls").exists())

    def test_ci_overrides_ignore_local_record(self):
        self.make("version")
        (self.root / ".cache/build.json").write_text("invalid json")
        self.make("version", "VERSION=0.0.0-pr.12.gaaaaaaaaaaaa",
                  "IMG=ghcr.io/example/operator:v0.0.0-pr.12.gaaaaaaaaaaaa")
        self.assertEqual(self.state()["image"], "ghcr.io/example/operator:v0.0.0-pr.12.gaaaaaaaaaaaa")

    def stub_chart_upload(self):
        tools = self.root / "tools"
        tools.mkdir()
        helm = tools / "helm"
        helm.write_text(
            '#!/usr/bin/env python3\nimport json, sys\n'
            'with open("chart-calls", "a") as output:\n'
            '    output.write(json.dumps(sys.argv[1:]) + "\\n")\n')
        helm.chmod(0o755)
        self.env["PATH"] = str(tools) + os.pathsep + self.env["PATH"]
        package = self.root / "hack/package-chart.sh"
        package.write_text(
            'printf "%s %s\\n" "$VERSION" "$1" > packaged\n'
            'mkdir -p "$2"\n'
            'touch "$2/agentrally-operator-${VERSION}.tgz"\n')

    def test_chart_upload_reuses_version_and_does_not_push_operator_image(self):
        self.stub_chart_upload()
        self.make("version", "VERSION=0.0.0-pr.12.gaaaaaaaaaaaa",
                  "IMG=registry.example:5000/operator:custom")
        selected = self.state()
        destination = "oci://registry.example:5000/team/helm"
        self.make("chart-push", "CHART_OCI_REPO=" + destination)
        selected["chart_oci_repo"] = destination
        self.assertEqual(self.state(), selected)
        self.assertEqual((self.root / "packaged").read_text().strip(),
                         f'{selected["version"]} {selected["image"]}')
        self.assertEqual(json.loads((self.root / "chart-calls").read_text()),
                         ["push", str(self.root / "dist" /
                                      f'agentrally-operator-{selected["version"]}.tgz'), destination])
        self.assertFalse((self.root / "calls").exists())
        self.make("docker-push")
        self.assertEqual((self.root / "calls").read_text().strip(), f'push {selected["image"]}')
        self.assertEqual(len((self.root / "chart-calls").read_text().splitlines()), 1)

    def test_chart_upload_defaults_to_dev_and_ghcr(self):
        self.stub_chart_upload()
        self.make("chart-push")
        self.assertEqual(self.state()["version"], "0.0.0-dev")
        self.assertEqual(json.loads((self.root / "chart-calls").read_text()),
                         ["push", str(self.root / "dist/agentrally-operator-0.0.0-dev.tgz"),
                          "oci://ghcr.io/chizhang21/helm"])

    def test_chart_upload_stops_when_packaging_fails(self):
        self.stub_chart_upload()
        (self.root / "hack/package-chart.sh").write_text('exit 12\n')
        self.make("chart-push", success=False)
        self.assertFalse((self.root / "chart-calls").exists())
        self.assertFalse((self.root / "calls").exists())

    def test_configure_saves_repositories_independently_without_uploading(self):
        self.stub_chart_upload()
        self.make("version", "VERSION=1.2.3", "IMG=registry.example/operator:custom")
        destination = "oci://charts.example:5000/team/helm"
        self.make("configure", "CHART_OCI_REPO=" + destination)
        self.assertEqual(self.state(), {"version": "1.2.3",
                                       "image": "registry.example/operator:custom",
                                       "chart_oci_repo": destination})
        self.make("configure", "IMAGE_TAG_BASE=images.example/team/operator")
        self.assertEqual(self.state()["image"], "images.example/team/operator:v1.2.3")
        self.assertEqual(self.state()["chart_oci_repo"], destination)
        self.assertFalse((self.root / "calls").exists())
        self.assertFalse((self.root / "chart-calls").exists())
        self.make("docker-push")
        self.make("chart-push")
        self.assertEqual(json.loads((self.root / "chart-calls").read_text())[-1], destination)
        self.assertEqual((self.root / "calls").read_text().strip(),
                         "push images.example/team/operator:v1.2.3")
        self.make("version")
        self.assertEqual(self.state(), {"version": "0.0.0-dev",
                                       "image": "images.example/team/operator:v0.0.0-dev",
                                       "chart_oci_repo": destination})

    def test_old_record_without_chart_repository_remains_usable(self):
        (self.root / ".cache").mkdir()
        (self.root / ".cache/build.json").write_text(json.dumps({
            "version": "1.2.3", "image": "registry.example/operator:v1.2.3"}))
        self.make("configure")
        self.assertEqual(self.state()["version"], "1.2.3")
        self.assertEqual(self.state()["chart_oci_repo"], "oci://ghcr.io/chizhang21/helm")

    def test_complete_image_override_preserves_chart_destination(self):
        destination = "oci://charts.example/team/helm"
        self.make("configure", "CHART_OCI_REPO=" + destination)
        self.make("configure", "VERSION=1.2.3", "IMG=images.example/operator:custom")
        self.assertEqual(self.state()["chart_oci_repo"], destination)

    def test_invalid_chart_destination_preserves_saved_settings(self):
        self.make("version")
        selected = self.state()
        for destination in ["https://charts.example/helm", "oci://charts.example/helm:1.2.3",
                            "oci://charts.example/team helm"]:
            self.make("configure", "CHART_OCI_REPO=" + destination, success=False)
            self.assertEqual(self.state(), selected)
        self.assertFalse((self.root / "calls").exists())

    def test_help_does_not_create_build_state(self):
        self.make("help")
        self.assertFalse((self.root / ".cache").exists())


if __name__ == "__main__":
    unittest.main()
