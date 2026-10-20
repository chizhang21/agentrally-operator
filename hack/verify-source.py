"""Verify source formatting and local documentation links without modifying files."""
from pathlib import Path
import re
import subprocess

root = Path(__file__).resolve().parents[1]
files = {
    root / name
    for name in subprocess.check_output(
        ["git", "ls-files", "-co", "--exclude-standard", "-z"], cwd=root
    ).decode().split("\0")
    if name
}
errors = []
for source in sorted(files):
    if not source.is_file() or source.suffix != ".md":
        continue
    for reference in re.findall(r"\]\(([^\s)]+)(?:\s+\"[^\"]*\")?\)", source.read_text()):
        if reference.startswith("#") or re.match(r"[a-zA-Z][a-zA-Z0-9+.-]*:", reference):
            continue
        target = reference.split("#", 1)[0].strip("<>")
        if target and not (source.parent / target).exists():
            errors.append(f"{source.relative_to(root)}: missing link target {target}")

go_files = [str(source) for source in sorted(files) if source.is_file() and source.suffix == ".go"]
if go_files:
    unformatted = subprocess.check_output(["gofmt", "-l", *go_files], cwd=root).decode().splitlines()
    errors.extend(f"{Path(name).relative_to(root)}: run gofmt" for name in unformatted)
if errors:
    raise SystemExit("\n".join(errors))
print("Go formatting and local documentation links are current")
