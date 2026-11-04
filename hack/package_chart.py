"""Stamp and package a temporary Helm chart without changing source files."""
import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def validate_version(version):
    match = re.fullmatch(
        r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
        r"(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?", version
    )
    if not match or len(version) > 63 or not version[-1].isalnum():
        raise ValueError("VERSION must be x.y.z[-prerelease], at most 63 characters, without v or +metadata")
    if match[4] and any(part.isdigit() and len(part) > 1 and part.startswith("0")
                        for part in match[4].split(".")):
        raise ValueError("numeric prerelease identifiers cannot have leading zeros")
    return version


def replace_field(text, field, value):
    text, count = re.subn(r"^" + re.escape(field) + r":.*$",
                         lambda _: field + ": " + json.dumps(value), text, flags=re.MULTILINE)
    if count != 1:
        raise ValueError(f"expected one {field.strip()} field in chart source")
    return text


def parse_image(image):
    """Return repository, tag and digest for an explicit image reference."""
    if not image or re.search(r"\s", image):
        raise ValueError("IMG must be an image reference without whitespace")
    if "@" in image:
        repository, digest = image.split("@", 1)
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise ValueError("IMG digest must be sha256 with 64 lowercase hex characters")
        tag = ""
    else:
        repository, separator, tag = image.rpartition(":")
        digest = ""
        if not separator or "/" in tag or not re.fullmatch(r"[\w][\w.-]{0,127}", tag, re.ASCII):
            raise ValueError("IMG must include an explicit tag or sha256 digest")
    if not repository:
        raise ValueError("IMG repository is empty")
    return repository, tag, digest


def stage_chart(source, output, version, image, connector_image):
    validate_version(version)
    metadata = (source / "Chart.yaml").read_text()
    metadata = replace_field(metadata, "version", version)
    metadata = replace_field(metadata, "appVersion", version)
    values = (source / "values.yaml").read_text()
    for reference, indent in [(image, "  "), (connector_image, "    ")]:
        repository, tag, digest = parse_image(reference)
        for field, value in [("repository", repository), ("tag", tag), ("digest", digest)]:
            values = replace_field(values, indent + field, value)
        if version.partition("-")[2] == "dev":
            values = replace_field(values, indent + "pullPolicy", "Always")
    shutil.copytree(source, output)
    (output / "Chart.yaml").write_text(metadata)
    (output / "values.yaml").write_text(values)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--connector-image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        root = Path(__file__).resolve().parents[1]
        (root / ".cache").mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="chart.", dir=root / ".cache") as directory:
            chart = Path(directory) / "chart"
            stage_chart(root / "chart", chart, args.version, args.image, args.connector_image)
            args.output.mkdir(parents=True, exist_ok=True)
            subprocess.run(["helm", "package", str(chart), "--destination", str(args.output)], check=True)
    except ValueError as error:
        parser.error(str(error))
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.returncode) from None
