#!/usr/bin/env bash
# The candidate artifacts are already loaded by the reusable E2E workflow.
# Fetch the published bytes anonymously before executing the same examples.
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${RELEASE_VERSION:?Required release version}"
: "${CHART_REPOSITORY:?Required OCI chart parent}"
: "${E2E_IMAGE:?Required candidate Operator image}"
: "${E2E_CONNECTOR_IMAGE:?Required candidate Connector image}"
: "${E2E_CHART:?Required candidate Chart archive}"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
export DOCKER_CONFIG="$temporary/docker"
export HELM_REGISTRY_CONFIG="$temporary/helm-registry.json"
mkdir -p "$DOCKER_CONFIG" .cache/e2e/public-release
evidence=.cache/e2e/public-release/artifacts.txt
printf 'version=%s\nsource_commit=%s\n' "$RELEASE_VERSION" "$(git rev-parse HEAD)" > "$evidence"
for image in "$E2E_IMAGE" "$E2E_CONNECTOR_IMAGE"; do
  expected=$(docker image inspect --format '{{.Id}}' "$image")
  docker pull --platform=linux/amd64 "$image"
  actual=$(docker image inspect --format '{{.Id}}' "$image")
  if [[ "$expected" != "$actual" ]]; then
    echo "Published image differs from the tested candidate: $image" >&2; exit 1
  fi
  docker image inspect --format 'image={{.RepoTags}} config_digest={{.Id}} registry_digests={{.RepoDigests}}' "$image" >> "$evidence"
done
helm pull "$CHART_REPOSITORY/agentrally-operator" --version "$RELEASE_VERSION" --destination "$temporary"
published="$temporary/agentrally-operator-$RELEASE_VERSION.tgz"
if ! cmp -s "$E2E_CHART" "$published"; then
  echo 'Published Chart differs from the tested candidate.' >&2; exit 1
fi
sha256sum "$published" >> "$evidence"
E2E_PUBLIC_RELEASE=true E2E_CHART="$published" E2E_PROFILE=baseline E2E_TEST='^TestPublishedExamples$' make e2e
