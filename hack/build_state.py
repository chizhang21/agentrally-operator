"""Select and save the version and repositories shared by make commands."""
import argparse
import json
import os
from pathlib import Path
import tempfile
from urllib.parse import urlsplit

from build_version import local_version, parse_image, validate_version


STATE = Path(".cache/build.json")
DEFAULT_CHART_OCI_REPO = "oci://ghcr.io/chizhang21/helm"


def validate_chart_repo(repository):
    parsed = urlsplit(repository)
    if (parsed.scheme != "oci" or not parsed.hostname or parsed.username is not None
            or parsed.password is not None or parsed.query or parsed.fragment
            or any(char.isspace() for char in repository)
            or ":" in parsed.path or "@" in parsed.path):
        raise ValueError("CHART_OCI_REPO must be oci://REGISTRY[/NAMESPACE], without credentials or a tag")
    return repository


def select_identity(version=None, image=None, image_base=None, dev=False, chart_repo=None):
    # Injected version/image do not depend on a saved build. Keep the independent
    # Chart destination when readable; a corrupt record must not block CI inputs.
    injected = version is not None and image is not None
    try:
        saved = json.loads(STATE.read_text()) if STATE.exists() else {}
    except ValueError:
        if not injected:
            raise
        saved = {}
    if saved and not injected:
        validate_version(saved["version"])
        parse_image(saved["image"])
    selected_version = validate_version(
        version if version is not None else
        local_version() if dev or not saved else saved["version"])
    if image is not None:
        selected_image = image
    elif saved and version is None and image_base is None and not dev:
        selected_image = saved["image"]
    else:
        repository = image_base if image_base is not None else (
            parse_image(saved["image"])[0] if saved else "arally-operator")
        selected_image = f"{repository}:v{selected_version}"
    parse_image(selected_image)
    selected_chart = validate_chart_repo(
        chart_repo if chart_repo is not None else saved.get("chart_oci_repo", DEFAULT_CHART_OCI_REPO))
    return {"version": selected_version, "image": selected_image, "chart_oci_repo": selected_chart}


def save_identity(version, image, chart_repo):
    validate_version(version)
    parse_image(image)
    validate_chart_repo(chart_repo)
    STATE.parent.mkdir(parents=True, exist_ok=True)
    # Replace atomically so readers never see a partially written record.
    with tempfile.NamedTemporaryFile(mode="w", dir=STATE.parent, delete=False) as output:
        temporary = Path(output.name)
        json.dump({"version": version, "image": image, "chart_oci_repo": chart_repo}, output, indent=2)
        output.write("\n")
    try:
        os.replace(temporary, STATE)
    finally:
        temporary.unlink(missing_ok=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("resolve", "save"))
    parser.add_argument("--version")
    parser.add_argument("--image")
    parser.add_argument("--image-base")
    parser.add_argument("--chart-repo")
    parser.add_argument("--dev", action="store_true", help="select the default development version")
    args = parser.parse_args()
    try:
        if args.action == "resolve":
            identity = select_identity(args.version, args.image, args.image_base, args.dev, args.chart_repo)
            print(identity["version"], identity["image"], identity["chart_oci_repo"])
        else:
            if args.version is None or args.image is None:
                raise ValueError("saving requires --version and --image")
            chart_repo = args.chart_repo if args.chart_repo is not None else DEFAULT_CHART_OCI_REPO
            save_identity(args.version, args.image, chart_repo)
            print(args.version)
            print("Image:", args.image)
            print("Chart OCI:", chart_repo)
    except (ValueError, OSError, KeyError, TypeError) as error:
        parser.error(str(error))
