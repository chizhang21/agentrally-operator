"""Derive exact GHCR tags and validate release intent without registry access."""
import json
import os
from pathlib import Path
import re

from build_version import validate_version


def metadata(event_name, repository, sha, ref, event):
    repository = repository.lower()
    if not re.fullmatch(r"[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*", repository):
        raise ValueError("invalid GitHub repository")
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", sha):
        raise ValueError("invalid checked commit SHA")
    if event_name == "push" and event.get("deleted"):
        raise ValueError("deleted Git refs cannot be packaged")
    if event_name == "pull_request":
        number = event.get("number")
        if type(number) is not int or number < 1:
            raise ValueError("invalid pull request number")
        version = validate_version(f"0.0.0-pr.{number}.g{sha[:12]}")
        tag = "v" + version
    elif event_name in ("push", "workflow_dispatch") and ref.startswith("refs/tags/"):
        ref_name = ref.removeprefix("refs/tags/")
        if not ref_name.startswith("v"):
            raise ValueError("release tags must start with v")
        version = validate_version(ref_name[1:])
        tag = ref_name
    elif event_name in ("push", "workflow_dispatch") and ref.startswith("refs/heads/") and ref != "refs/heads/":
        version = validate_version(f"0.0.0-dev.g{sha[:12]}")
        tag = "v" + version
    else:
        raise ValueError("expected a pull request, full branch ref or refs/tags/v<version>")
    if len(tag) > 128:
        raise ValueError("image tag exceeds the registry limit")
    owner = repository.split("/", 1)[0]
    return {"image": f"ghcr.io/{repository}", "tag": tag, "version": version,
            "chart_repository": f"oci://ghcr.io/{owner}/helm"}


if __name__ == "__main__":
    try:
        result = metadata(
            os.environ["GITHUB_EVENT_NAME"], os.environ["GITHUB_REPOSITORY"],
            os.environ["GITHUB_SHA"], os.environ["GITHUB_REF"],
            json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text()),
        )
    except (KeyError, ValueError) as error:
        raise SystemExit(str(error)) from error
    for key, value in result.items():
        print(f"{key}={value}")
