#!/usr/bin/env bash
# Use only a new cluster and a private kubeconfig; never the caller's current context.
set -euo pipefail
cd "$(dirname "$0")/../.."

KIND=${KIND:-kind}
for tool in "$KIND" docker kubectl helm go curl timeout tee awk sha256sum git rg; do
  command -v "$tool" >/dev/null || { echo "Required E2E tool is missing: $tool" >&2; exit 1; }
done
source tests/e2e/versions.env
export E2E_PROFILE=${E2E_PROFILE:-baseline}
explicit_test=${E2E_TEST:+true}
node=$KIND_NODE_BASELINE
upstream_version=$SANDBOX_VERSION
upstream_sha=$SANDBOX_SHA256
required='TestPublishedExamples TestStableRevisionUpdates TestRetainedWorkspaceUpgrade TestOperatorLifecycle TestRuntimePolicyAndExplicitLifecycle TestRuntimeSelectionAndFailures TestGroupSecurityBoundaries TestConnectorManagement'
case "$E2E_PROFILE" in
  baseline) ;;
  newer-kubernetes)
    node=$KIND_NODE_NEWER
    required='TestPublishedExamples TestStableRevisionUpdates TestRetainedWorkspaceUpgrade TestRuntimePolicyAndExplicitLifecycle TestRuntimeSelectionAndFailures TestGroupSecurityBoundaries'
    export E2E_TEST=${E2E_TEST:-'^(TestPublishedExamples|TestStableRevisionUpdates|TestRetainedWorkspaceUpgrade|TestRuntimePolicyAndExplicitLifecycle|TestRuntimeSelectionAndFailures|TestGroupSecurityBoundaries)$'}
    ;;
  operator-upgrade|connector-upgrade)
    export E2E_UPGRADE_FROM=${E2E_UPGRADE_FROM:-$OPERATOR_PREVIOUS_VERSION}
    export E2E_UPGRADE_CONNECTOR=false
    if [[ "$E2E_PROFILE" == connector-upgrade ]]; then export E2E_UPGRADE_CONNECTOR=true; fi
    ;;
  sandbox-upgrade)
    required=TestSandboxUpgrade
    export E2E_TEST=${E2E_TEST:-'^TestSandboxUpgrade$'}
    upstream_version=$SANDBOX_PREVIOUS_VERSION
    upstream_sha=$SANDBOX_PREVIOUS_SHA256
    ;;
  shared-storage)
    required=TestSharedStorage
    export E2E_TEST=${E2E_TEST:-'^TestSharedStorage$'}
    ;;
  *) echo 'Unknown E2E_PROFILE.' >&2; exit 1 ;;
esac
if [[ -n ${E2E_UPGRADE_FROM:-} ]]; then
  if [[ "$E2E_PROFILE" != baseline && "$E2E_PROFILE" != operator-upgrade && "$E2E_PROFILE" != connector-upgrade ]]; then
    echo 'Operator upgrade cannot be combined with another E2E profile.' >&2; exit 1
  fi
  upstream_version=$SANDBOX_PREVIOUS_VERSION
  upstream_sha=$SANDBOX_PREVIOUS_SHA256
  required=TestChartUpgrade
  export E2E_TEST=${E2E_TEST:-'^TestChartUpgrade$'}
fi
if [[ "$explicit_test" == true ]]; then required=''; fi
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
connector_image=${E2E_CONNECTOR_IMAGE:-agentrally-connector:e2e}
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
      timeout 10s kubectl --request-timeout=5s -n agentrally-system logs deployment/agentrally-operator-connector --pod-running-timeout=5s --all-containers > "$logs/connector.log" 2>&1 || true
      timeout 10s kubectl --request-timeout=5s -n agent-sandbox-system logs deployment/agent-sandbox-controller --pod-running-timeout=5s --all-containers > "$logs/upstream.log" 2>&1 || true
      if [[ "$E2E_PROFILE" == shared-storage ]]; then
        timeout 10s kubectl --request-timeout=5s -n agentrally-test-storage logs statefulset/nfs-nfs-server-provisioner --pod-running-timeout=5s > "$logs/nfs.log" 2>&1 || true
        timeout 10s kubectl --request-timeout=5s -n agentrally-test-storage exec nfs-nfs-server-provisioner-0 -- cat /export/ganesha.log > "$logs/nfs-server.log" 2>&1 || true
      fi
      timeout 30s "$KIND" export logs "$logs/kind" --name "$cluster" || true
    fi
    cleanup_egress_fixtures
    fixtures_result=$?
    if [[ "$E2E_PROFILE" == shared-storage ]]; then
      # Unmount clients in the example namespace while the test NFS server is alive.
      timeout 100s kubectl --request-timeout=5s delete namespace agentrally-demo --ignore-not-found --wait --timeout=90s || fixtures_result=1
    fi
    timeout 120s "$KIND" delete cluster --name "$cluster"
    cluster_result=$?
    if (( fixtures_result != 0 || cluster_result != 0 )); then
      echo 'E2E cleanup failed.' >&2
      if (( result == 0 )); then result=1; fi
    fi
  fi
  printf 'exit_code=%s\n' "$result" >> "$logs/inputs.txt"
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
  docker build -f Containerfile --target operator --tag "$image" .
else
  docker image inspect "$image" >/dev/null
fi

if [[ "$connector_image" == *@* || ${connector_image##*/} != *:* ]]; then
  echo 'E2E_CONNECTOR_IMAGE must name a locally built image with an explicit tag.' >&2
  exit 1
fi
if [[ -z ${E2E_CONNECTOR_IMAGE:-} ]]; then
  docker build -f Containerfile --target connector --tag "$connector_image" .
else
  docker image inspect "$connector_image" >/dev/null
fi

{
  printf 'profile=%s\nselected_tests=%s\nrequired_tests=%s\n' "$E2E_PROFILE" "${E2E_TEST:-.}" "$required"
  printf 'public_release=%s\nupgrade_connector=%s\n' "${E2E_PUBLIC_RELEASE:-false}" "${E2E_UPGRADE_CONNECTOR:-false}"
  printf 'source_commit=%s\n' "$(git rev-parse HEAD)"
  git status --porcelain --untracked-files=normal
  printf 'tracked_diff_sha256='; git diff --binary --no-ext-diff HEAD | sha256sum
  printf 'untracked_files_sha256='; git ls-files --others --exclude-standard -z | sort -z | xargs -0 -r sha256sum | sha256sum
  printf 'examples_sha256='; rg --files -0 examples | sort -z | xargs -0 -r sha256sum | sha256sum
  if [[ -f "$chart" ]]; then sha256sum "$chart"; else find "$chart" -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum; fi
  helm show chart "$chart"
  # Image ID is the Docker configuration digest, not the registry manifest digest.
  docker image inspect --format 'image={{.RepoTags}} config_digest={{.Id}} registry_digests={{.RepoDigests}}' "$image" "$connector_image"
  printf 'node=%s\nsandbox=%s sha256=%s\noperator_upgrade_from=%s\n' "$node" "$upstream_version" "$upstream_sha" "${E2E_UPGRADE_FROM:-none}"
  "$KIND" version
} > "$logs/inputs.txt"
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
else
  kind_options=()
  if [[ -n ${E2E_KIND_CONFIG:-} ]]; then kind_options+=(--config "$E2E_KIND_CONFIG"); fi
  "$KIND" create cluster --name "$cluster" --image "$node" --kubeconfig "$KUBECONFIG" --wait 180s "${kind_options[@]}"
fi
# Linux CI and local runs use the CLI from the same pinned image as the API server.
# Other hosts retain their native kubectl binary.
if [[ "$(go env GOHOSTOS)" == linux ]]; then
  mkdir -p "$temporary/bin"
  docker cp -L "$cluster-control-plane:/usr/bin/kubectl" "$temporary/bin/kubectl"
  export PATH="$temporary/bin:$PATH"
fi
kubectl version --client -o yaml >> "$logs/inputs.txt"
if [[ "$E2E_CNI" == cilium ]]; then
  # Nodes become Ready only after the replacement CNI is installed.
  helm install cilium oci://quay.io/cilium/charts/cilium --version 1.20.3 -n kube-system \
    --set ipam.mode=kubernetes --set operator.replicas=1 \
    --set kubeProxyReplacement=false --set socketLB.enabled=false --wait --timeout=360s
  kubectl -n kube-system rollout status daemonset/cilium --timeout=240s
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
load_image "$connector_image"
docker pull --platform="$platform" docker.io/library/busybox:1.37.0
export E2E_DIGEST_IMAGE
E2E_DIGEST_IMAGE=$(docker image inspect --format '{{index .RepoDigests 0}}' docker.io/library/busybox:1.37.0)
load_image docker.io/library/busybox:1.37.0
if [[ "$E2E_PROFILE" == shared-storage ]]; then
  source tests/e2e/nfs.sh
  install_nfs_fixture "$temporary" "$platform" "$logs/inputs.txt"
fi
upstream="$temporary/upstream.yaml"
download_upstream() {
  curl -fsSL --retry 3 "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$1/sandbox-with-extensions.yaml" -o "$3"
  echo "$2  $3" | sha256sum --check
}
download_upstream "$upstream_version" "$upstream_sha" "$upstream"
kubectl apply --server-side -f "$upstream"
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=240s
if [[ "$E2E_PROFILE" == sandbox-upgrade ]]; then
  export E2E_SANDBOX_UPGRADE_MANIFEST="$temporary/upstream-target.yaml"
  download_upstream "$SANDBOX_VERSION" "$SANDBOX_SHA256" "$E2E_SANDBOX_UPGRADE_MANIFEST"
  printf 'sandbox_upgrade_to=%s sha256=%s\n' "$SANDBOX_VERSION" "$SANDBOX_SHA256" >> "$logs/inputs.txt"
fi

if [[ -n ${E2E_UPGRADE_FROM:-} ]]; then
  # Start with the published release, including its CRDs and image defaults.
  # The Go scenario upgrades it to the candidate chart and images loaded above.
  helm pull oci://ghcr.io/chizhang21/helm/agentrally-operator --version "$E2E_UPGRADE_FROM" --destination "$temporary"
  export E2E_PREVIOUS_CHART="$temporary/agentrally-operator-$E2E_UPGRADE_FROM.tgz"
  previous_image="ghcr.io/chizhang21/agentrally-operator:v$E2E_UPGRADE_FROM"
  docker pull --platform="$platform" "$previous_image"
  load_image "$previous_image"
  if [[ ${E2E_UPGRADE_CONNECTOR:-false} == true ]]; then
    previous_connector="ghcr.io/chizhang21/agentrally-connector:v$E2E_UPGRADE_FROM"
    docker pull --platform="$platform" "$previous_connector"
    load_image "$previous_connector"
    docker image inspect --format 'previous_connector={{.RepoTags}} config_digest={{.Id}} registry_digests={{.RepoDigests}}' "$previous_connector" >> "$logs/inputs.txt"
  fi
  sha256sum "$E2E_PREVIOUS_CHART" >> "$logs/inputs.txt"
  docker image inspect --format 'previous_image={{.RepoTags}} config_digest={{.Id}} registry_digests={{.RepoDigests}}' "$previous_image" >> "$logs/inputs.txt"
  helm install agentrally-operator "$E2E_PREVIOUS_CHART" -n agentrally-system --create-namespace \
    --set image.pullPolicy=IfNotPresent --wait --timeout=240s
  export E2E_TEST=${E2E_TEST:-^TestChartUpgrade$}
elif [[ ${E2E_PUBLIC_RELEASE:-false} == true ]]; then
  helm install agentrally-operator "$chart" -n agentrally-system --create-namespace --wait --timeout=240s
  actual_image=$(kubectl -n agentrally-system get deployment agentrally-operator -o jsonpath='{.spec.template.spec.containers[0].image}')
  if [[ "$actual_image" != "$image" ]]; then
    echo "Published Chart selects an unexpected Operator image: $actual_image" >&2; exit 1
  fi
else
  helm install agentrally-operator "$chart" -n agentrally-system --create-namespace \
    --set-string image.repository="${image%:*}" --set-string image.tag="${image##*:}" \
    --set-string connector.image.repository="${connector_image%:*}" \
    --set-string connector.image.tag="${connector_image##*:}" \
    --set connector.image.pullPolicy=IfNotPresent --set image.pullPolicy=IfNotPresent --wait --timeout=240s
fi
export E2E_IMAGE="$image" E2E_CONNECTOR_IMAGE="$connector_image"
set +e
AGENTRALLY_E2E_KUBECONFIG="$KUBECONFIG" GOWORK=off \
  go test -p=1 -tags=e2e ./cmd/... ./tests/e2e -run "${E2E_TEST:-.}" -count=1 -timeout=50m -v 2>&1 \
  | tee "$logs/lifecycle.log"
test_results=("${PIPESTATUS[@]}")
set -e
if (( test_results[0] != 0 )); then exit "${test_results[0]}"; fi
if (( test_results[1] != 0 )); then exit "${test_results[1]}"; fi
# Explicit selections cannot succeed through a skip. Optional platform cases in
# the default suite are visible in the log but do not count as compatibility proof.
if [[ -n ${E2E_TEST:-} ]] && grep -Eq '^[[:space:]]*--- SKIP:' "$logs/lifecycle.log"; then
  echo 'An explicitly selected E2E scenario skipped.' >&2; exit 1
fi
for name in $required; do
  if ! grep -Eq "^--- PASS: $name " "$logs/lifecycle.log" || grep -Eq "^[[:space:]]*--- SKIP: $name(/| )" "$logs/lifecycle.log"; then
    echo "Required E2E scenario did not fully pass: $name" >&2; exit 1
  fi
done
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
