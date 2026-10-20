# Installation

Use Helm 3.17 or later and an account that can create and update CRDs and cluster RBAC.
Install one Arally Operator release per cluster. The chart installs and upgrades
Arally's CRDs as part of that release.

Published images support Linux amd64 nodes, selected by default in the chart.
Custom images for other architectures require a matching node selector.

## Standard installation

### Install upstream agent-sandbox

Arally needs agent-sandbox Core and Extensions. Install both using the
[official combined manifest](https://github.com/kubernetes-sigs/agent-sandbox/tree/v1.0.5#installation):

```sh
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.5/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
```

This creates the `agent-sandbox-system` namespace, four CRDs, RBAC and one
controller with Extensions enabled. Arally's API adapter uses v1.0.5.

Helm does not install or check these dependencies. Missing `Sandbox` or
`SandboxTemplate` APIs prevent normal Operator startup; installation with `--wait`
then times out. Missing `SandboxClaim` or `SandboxWarmPool` APIs block allocation
with `PoolUnavailable`. If all CRDs exist but the upstream controller is
unavailable, Arally can start, but new AgentClaims wait for allocation and
eventually time out. The readiness probe does not verify upstream controller health.

### Install Arally

Install with the default values:

```sh
helm install arally-operator oci://ghcr.io/chizhang21/helm/arally-operator \
  -n arally-system --create-namespace --wait --timeout 5m
```

Add `-f myvalues.yaml` for custom values. If CRDs remain from a previous uninstall,
add `--take-ownership` to adopt them, using the same release name and namespace.
Use adoption only for resources belonging to your Arally installation.

`--wait --timeout 5m` waits up to five minutes for the release's workloads and
dependencies to be ready. The Operator creates `ClusterCapabilities/cluster`
automatically.

See [chart values](../chart/values.yaml) for available settings,
[Connector setup](outbound-connector.md#installation) for backend integration,
and [Agent SCC setup](usage.md#legacy-agent-images-on-openshift) for root or
fixed-UID Agent images on OpenShift. Continue with [deploying an agent](usage.md).

### Upgrade Arally

```sh
helm upgrade arally-operator oci://ghcr.io/chizhang21/helm/arally-operator \
  -n arally-system --reset-then-reuse-values --wait --timeout 5m
```

This updates the Operator and CRDs together. `--reset-then-reuse-values` uses the
new chart defaults and preserves previous explicit settings, including pull
Secrets. Pass `-f` or `--set` to change those settings. If existing Arally CRDs
are outside the release, add `--take-ownership` for the first upgrade.
No separate `kubectl apply` of Arally CRDs is needed.

Uninstall retains CRDs and Agent resources. Clean up Agents and Groups while the
Operator is still running; see [Lifecycle](usage.md#lifecycle).

## Local development and testing

Complete [Development: Package versions](development.md#package-versions), then
[install agent-sandbox](#install-upstream-agent-sandbox). From the repository root,
install the default dev archive:

```sh
CHART_PACKAGE="./dist/arally-operator-0.0.0-dev.tgz"
helm install arally-operator "$CHART_PACKAGE" \
  -n arally-system --create-namespace --wait --timeout 5m
```

If CRDs remain from a previous uninstall, add `--take-ownership` to the install
command. For an existing Helm release, use [Updating a dev build](#updating-a-dev-build).

If you built with an explicit `VERSION`, set `CHART_PACKAGE` to that version's archive.

The archive references the image built and pushed during development. Add
`-f myvalues.yaml` for custom values; the packaged image needs no override.

### Private images

If your development image requires authentication, create its pull Secret before
installing:

```sh
kubectl create namespace arally-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n arally-system create secret generic registry-credentials \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=/path/to/registry-config.json
```

Use a Docker config containing credentials for your registry. Pass the Secret
name directly; the packaged image settings need no overrides:

```sh
helm install arally-operator "$CHART_PACKAGE" \
  --set 'imagePullSecrets[0].name=registry-credentials' \
  -n arally-system --wait --timeout 5m
```

Keep the `--set` argument quoted so shells such as zsh do not expand the brackets.

The Secret applies to the Operator Pod, including its optional Connector worker.
Private Agent images need their own pull Secrets in the Group namespace,
configured in the Claim or SandboxTemplate.

### Updating a dev build

Rebuild, push and package using [Development](development.md#package-versions),
then upgrade from the new archive:

```sh
CHART_PACKAGE="./dist/arally-operator-0.0.0-dev.tgz"
helm upgrade arally-operator "$CHART_PACKAGE" \
  -n arally-system --reset-then-reuse-values --wait --timeout 5m
```

This uses the [same upgrade behavior](#upgrade-arally) as a published chart,
preserving explicit settings such as `imagePullSecrets`. Explicit image overrides
also carry over; update them with `--set` if the image reference changed.
For CRDs installed outside the release, add `--take-ownership` on the first upgrade.

The dev tag stays `v0.0.0-dev`. Its packaged pull policy is `Always`, but existing
Pods need a restart to use the replacement image:

```sh
kubectl -n arally-system rollout restart deployment \
  -l app.kubernetes.io/instance=arally-operator
kubectl -n arally-system rollout status deployment \
  -l app.kubernetes.io/instance=arally-operator --timeout=5m
```

Restarting the Operator also restarts its enabled Connector worker. If you used a
different Helm release name, replace `arally-operator` in the selector.

[Chart configuration](../chart/README.md) covers monitoring and scheduling.
