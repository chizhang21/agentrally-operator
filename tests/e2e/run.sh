#!/usr/bin/env bash
# Use only a new cluster and a private kubeconfig; never the caller's current context.
set -euo pipefail
cd "$(dirname "$0")/../.."

KIND=${KIND:-kind}
for tool in "$KIND" docker kubectl helm go curl timeout tee awk; do
  command -v "$tool" >/dev/null || { echo "Required E2E tool is missing: $tool" >&2; exit 1; }
done
export E2E_CNI=${E2E_CNI:-kindnet}
export E2E_SNAPSHOTS=${E2E_SNAPSHOTS:-false}
export E2E_RUNTIME_CLASS=${E2E_RUNTIME_CLASS:-}
if [[ -n ${E2E_RUNTIME_MANIFEST:-} && ! -f "$E2E_RUNTIME_MANIFEST" ]]; then
  echo 'E2E_RUNTIME_MANIFEST must name a local runtime installation manifest.' >&2
  exit 1
fi
if [[ -n ${E2E_KIND_CONFIG:-} && ! -f "$E2E_KIND_CONFIG" ]]; then
  echo 'E2E_KIND_CONFIG must name a local kind configuration.' >&2
  exit 1
fi
if [[ -n ${E2E_KIND_CONFIG:-} && "$E2E_CNI" == cilium ]]; then
  echo 'Custom kind configuration currently requires E2E_CNI=kindnet.' >&2
  exit 1
fi
case "$E2E_CNI" in kindnet|cilium) ;; *) echo 'E2E_CNI must be kindnet or cilium.' >&2; exit 1 ;; esac
case "$E2E_SNAPSHOTS" in true|false) ;; *) echo 'E2E_SNAPSHOTS must be true or false.' >&2; exit 1 ;; esac
chart=${E2E_CHART:-chart}
if [[ ! -f "$chart" && ! -f "$chart/Chart.yaml" ]]; then
  echo 'E2E_CHART must name a local Chart directory or archive.' >&2
  exit 1
fi

cluster="agentrally-e2e-$(date +%s)-$$"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/agentrally-e2e.XXXXXX")
export KUBECONFIG="$temporary/kubeconfig"
log_root="${E2E_LOG_DIR:-$PWD/.cache/e2e}"
mkdir -p "$log_root"
logs=$(mktemp -d "$log_root/$cluster.XXXXXX")
image=${E2E_IMAGE:-agentrally-operator:e2e}
created=false

cleanup_egress_fixtures() {
  local ids id owner failed=0
  ids=$(timeout 10s docker ps -aq --no-trunc --filter "label=io.agentrally.e2e.egress-cluster=$cluster") || return 1
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    if [[ ! "$id" =~ ^[a-f0-9]{64}$ ]]; then failed=1; continue; fi
    owner=$(timeout 5s docker inspect --format '{{ index .Config.Labels "io.agentrally.e2e.egress-cluster" }}' "$id") || { failed=1; continue; }
    if [[ "$owner" != "$cluster" ]]; then failed=1; continue; fi
    timeout 10s docker rm -f "$id" >/dev/null || failed=1
  done <<< "$ids"
  return "$failed"
}

finish() {
  result=$?
  trap - EXIT INT TERM
  # Diagnostic failures must not skip cleanup or replace the test failure.
  set +e
  if [[ "$created" == true ]]; then
    if (( result != 0 )); then
      kubectl --request-timeout=5s get pods,events -A -o yaml > "$logs/resources.yaml" 2>&1 || true
      kubectl --request-timeout=5s get agentdefinitions,agentinstances,agentgroups,clustercapabilities -A -o yaml > "$logs/agentrally-resources.yaml" 2>&1 || true
      timeout 10s kubectl --request-timeout=5s -n agentrally-system logs deployment/agentrally-operator --pod-running-timeout=5s --all-containers > "$logs/operator.log" 2>&1 || true
      timeout 10s kubectl --request-timeout=5s -n agent-sandbox-system logs deployment/agent-sandbox-controller --pod-running-timeout=5s --all-containers > "$logs/upstream.log" 2>&1 || true
      timeout 30s "$KIND" export logs "$logs/kind" --name "$cluster" || true
    fi
    cleanup_egress_fixtures
    fixtures_result=$?
    timeout 120s "$KIND" delete cluster --name "$cluster"
    cluster_result=$?
    if (( fixtures_result != 0 || cluster_result != 0 )); then
      echo 'E2E cleanup failed.' >&2
      if (( result == 0 )); then result=1; fi
    fi
  fi
  echo "E2E logs: $logs"
  rm -rf "$temporary"
  exit "$result"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Supplying an image tests an already built local release candidate. Otherwise
# build the production Containerfile from this checkout, without publishing it.
if [[ "$image" == *@* || ${image##*/} != *:* ]]; then
  echo 'E2E_IMAGE must name a locally built image with an explicit tag.' >&2
  exit 1
fi
if [[ -z ${E2E_IMAGE:-} ]]; then
  docker build -f Containerfile --tag "$image" .
else
  docker image inspect "$image" >/dev/null
fi

node='kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a'
if "$KIND" get clusters | grep -Fxq "$cluster"; then
  echo "Test cluster name already exists: $cluster" >&2
  exit 1
fi
created=true
if [[ "$E2E_CNI" == cilium ]]; then
  cat > "$temporary/kind.yaml" <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
YAML
  "$KIND" create cluster --name "$cluster" --image "$node" --kubeconfig "$KUBECONFIG" --config "$temporary/kind.yaml"
  # Nodes become Ready only after the replacement CNI is installed.
  helm install cilium oci://quay.io/cilium/charts/cilium --version 1.20.3 -n kube-system \
    --set ipam.mode=kubernetes --set operator.replicas=1 \
    --set kubeProxyReplacement=false --set socketLB.enabled=false --wait --timeout=360s
  kubectl -n kube-system rollout status daemonset/cilium --timeout=240s
else
  kind_options=()
  if [[ -n ${E2E_KIND_CONFIG:-} ]]; then kind_options+=(--config "$E2E_KIND_CONFIG"); fi
  "$KIND" create cluster --name "$cluster" --image "$node" --kubeconfig "$KUBECONFIG" --wait 180s "${kind_options[@]}"
fi
if [[ -n ${E2E_RUNTIME_MANIFEST:-} ]]; then
  kubectl apply --server-side -f "$E2E_RUNTIME_MANIFEST"
  kubectl wait --for=condition=Ready pod --all -A --timeout=360s
fi
if [[ -n "$E2E_RUNTIME_CLASS" ]]; then kubectl get runtimeclass "$E2E_RUNTIME_CLASS"; fi
# Docker's containerd image store can retain a multi-platform index without all
# its children. Export only the running platform so kind imports complete content.
platform="linux/$(go env GOARCH)"
load_image() {
  docker image save --platform="$platform" -o "$temporary/image.tar" "$1"
  "$KIND" load image-archive "$temporary/image.tar" --name "$cluster"
}
if [[ "$E2E_SNAPSHOTS" == true ]]; then
  # Preload pinned fixture images through Docker before starting their Pods.
  source tests/e2e/snapshots.sh
  install_snapshot_fixtures "$temporary" "$platform"
fi
load_image "$image"
docker pull --platform="$platform" docker.io/library/busybox:1.37.0
export E2E_DIGEST_IMAGE
E2E_DIGEST_IMAGE=$(docker image inspect --format '{{index .RepoDigests 0}}' docker.io/library/busybox:1.37.0)
load_image docker.io/library/busybox:1.37.0
upstream="$temporary/upstream.yaml"
curl -fsSL --retry 3 https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.5/sandbox-with-extensions.yaml -o "$upstream"
echo "b150cb058c577c59c42b060ff7f22e31b5311ca80430db98129f1280a0e85970  $upstream" | sha256sum --check
kubectl apply --server-side -f "$upstream"
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=240s

# Use a local tag loaded into kind. Keep image lookup and all other manifests
# identical to a real chart install; Connector remains disabled by default.
repository=${image%:*}
tag=${image##*:}
helm install agentrally-operator "$chart" -n agentrally-system --create-namespace \
  --set-string image.repository="$repository" --set-string image.tag="$tag" \
  --set image.pullPolicy=IfNotPresent --wait --timeout=240s
set +e
AGENTRALLY_E2E_KUBECONFIG="$KUBECONFIG" GOWORK=off \
  go test -p=1 -tags=e2e ./cmd ./tests/e2e -run "${E2E_TEST:-.}" -count=1 -timeout=35m -v 2>&1 \
  | tee "$logs/lifecycle.log"
test_results=("${PIPESTATUS[@]}")
set -e
if (( test_results[0] != 0 )); then exit "${test_results[0]}"; fi
if (( test_results[1] != 0 )); then exit "${test_results[1]}"; fi
# A passing parent whose children all skipped is not an executed scenario.
if ! awk '
  /^[[:space:]]*--- (PASS|FAIL|SKIP): Test/ {
    status[$3] = $2
    parent = $3
    while (sub(/\/[^/]+$/, "", parent)) children[parent] = 1
  }
  END {
    for (name in status)
      if (status[name] == "PASS:" && !children[name] && name !~ /^TestE2ETargetGuard(\/|$)/) exit 0
    exit 1
  }
' "$logs/lifecycle.log"; then
  echo 'E2E must pass at least one scenario; an empty, skipped-only or target-guard-only run is insufficient.' >&2
  exit 1
fi
