"""Validate workflow inputs and write the selected image identity to GITHUB_OUTPUT."""

import os
import re

registry = os.environ["REGISTRY"]
repository = os.environ["IMAGE_REPOSITORY"] or os.environ["OWNER"].lower() + "/codex-mobile"
tag = os.environ["IMAGE_TAG"]
component = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
checks = {
    "registry": (registry, r"[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]{1,5})?"),
    "image_repository": (repository, component + r"(?:/" + component + r")*"),
    "image_tag": (tag, r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}"),
}
version = r"(?:latest|[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?(?:\+[A-Za-z0-9.-]+)?)"
for name in ("CODEXAPP_VERSION", "CODEX_VERSION"):
    checks[name.lower()] = (os.environ[name], version)
for name, (value, pattern) in checks.items():
    if not re.fullmatch(pattern, value):
        raise SystemExit(f"Invalid {name}; use the format shown in the image build documentation")
if len(registry + "/" + repository) > 255:
    raise SystemExit("Image repository is too long")
with open(os.environ["GITHUB_OUTPUT"], "a") as output:
    output.write(f"registry={registry}\nimage={registry}/{repository}:{tag}\n")
