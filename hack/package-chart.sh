#!/usr/bin/env bash
# Stamp and package a disposable chart copy; keep source defaults unchanged.
set -euo pipefail
image=$1
output=$2
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
mkdir -p .cache
stage=$(mktemp -d "$root/.cache/chart.XXXXXX")
trap 'rm -rf "$stage"' EXIT
version=${VERSION-$(python3 hack/build_version.py)}
python3 hack/build_version.py --version "$version" --image "$image" --output "$stage/chart"
mkdir -p "$output"
helm package "$stage/chart" --destination "$output"
