# Installation

Use Helm 3.17 or later and an account that can create and update CRDs and cluster RBAC.
Install one AgentRally Operator release per cluster.
The chart installs and upgrades AgentRally's CRDs as part of that release.

CI images and the default chart target Linux amd64 nodes.
Custom images for other architectures require a matching node selector.

For the current source checkout, use the
[local installation workflow](#local-development-and-testing).

Version-tag releases publish the Operator image, Connector image and OCI Chart to GHCR as soon as
packaging succeeds.
Checks and E2E run independently of OCI publication.
GitHub Release publication additionally waits for anonymous artifact retrieval and
the public-example smoke test.
PR and main-branch CI retain build artifacts without publishing them.
See the [release workflow](../.github/workflows/release.yml) or [manual uploads](development.md#upload-images-and-chart).
The OCI commands below require an accessible, published Chart version.

## Compatibility

The current source uses agent-sandbox 1.0.11 for new installations.
For published Operator 0.0.6, use agent-sandbox 1.0.5 as documented in its tagged installation guide.
For an existing installation, [upgrade AgentRally before agent-sandbox](#upgrade-agent-sandbox).

These are representative Kind configurations for this source revision, not a claim
that every Kubernetes, CNI, CSI or RuntimeClass combination has been tested.
The exact node images and manifest checksums are pinned in
[versions.env](../tests/e2e/versions.env).

| Scenario | Kubernetes | agent-sandbox | Result |
| --- | --- | --- | --- |
| Baseline lifecycle and public examples | 1.34.11 | 1.0.11 | Passed locally |
| Newer Kubernetes sample | 1.37.0 | 1.0.11 | Passed locally |
| Operator 0.0.6 → this candidate, Connector off/on | 1.34.11 | 1.0.5 | Passed locally |
| Upstream upgrade with this candidate fixed | 1.34.11 | 1.0.5 → 1.0.11 | Passed locally |
| Packaged Chart defaults and public examples | 1.34.11 | 1.0.11 | Passed locally |

These scenarios use runc, kindnet and local-path storage.
They do not establish Kubernetes cluster upgrade compatibility, or validate OKD,
Kata, gVisor, EFS or another storage backend.
The upgrade source is the latest published Operator version selected for this candidate;
older sources require their own verification.
Anonymous retrieval of the candidate artifacts is checked by the tag workflow after publication.

## Standard installation

### Install upstream agent-sandbox

AgentRally needs agent-sandbox Core and Extensions.
Install both using the
[official combined manifest](https://github.com/kubernetes-sigs/agent-sandbox/tree/v1.0.11#installation):

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.11/sandbox-with-extensions.yaml && kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
```

This creates the `agent-sandbox-system` namespace, four CRDs, RBAC and one controller
with Extensions enabled.
AgentRally uses the upstream `agents.x-k8s.io/v1beta1` APIs.

Helm does not install or check these dependencies.
Missing `Sandbox` or `SandboxTemplate` APIs prevent normal Operator startup;
installation with `--wait` then times out.
Missing `SandboxClaim` or `SandboxWarmPool` APIs block allocation with
`PoolUnavailable`.

If all CRDs exist but the upstream controller is unavailable, AgentRally can start, but new
AgentInstances wait for allocation and eventually time out.

The readiness probe does not verify upstream controller health.

### Install AgentRally

Replace `<version>` with the [latest release](https://github.com/chizhang21/agentrally-operator/releases/latest) version, without the leading `v`.
Use examples from the matching source tag.

```sh
CHART_VERSION='<version>'
helm install agentrally-operator oci://ghcr.io/chizhang21/helm/agentrally-operator \
  --version "$CHART_VERSION" \
  -n agentrally-system --create-namespace --wait --timeout 5m
```

Add `-f myvalues.yaml` for custom values.

`--wait --timeout 5m` waits up to five minutes for the release's workloads and
dependencies to be ready.

The Operator creates `ClusterCapabilities/cluster` automatically.

Snapshot-initialized workspaces additionally need the VolumeSnapshot CRDs, snapshot
controller and a compatible CSI driver.
AgentRally reads this API only for instances using snapshot sources; it is not a startup
dependency.
See [workspace initialization](usage.md#initialize-from-a-volumesnapshot).

See [chart values](../chart/values.yaml) for available settings.
Optional backend integration has an [Orchestrator deployment example](../examples/values/orchestrator.yaml)
and [credential setup](outbound-connector.md#installation).
Continue with [deploying an agent](usage.md).

### Release configuration

The [default values](../chart/values.yaml) configure direct Kubernetes management.
Values examples provide overrides for the deployment modes:

| Configuration | Values file |
| --- | --- |
| Direct management through kubectl or GitOps | [standalone.yaml](../examples/values/standalone.yaml) |
| Orchestrator management through Connector | [orchestrator.yaml](../examples/values/orchestrator.yaml) |

From a local checkout, add `-f examples/values/standalone.yaml` or
`-f examples/values/orchestrator.yaml` to the install or upgrade command.
These files are Helm inputs; do not apply them with kubectl.

Orchestrator mode requires the [Connector credentials](outbound-connector.md#installation).

Monitoring is enabled by default, without values switches.
When the monitoring APIs exist during Helm installation or upgrade, the Chart creates
a ServiceMonitor and PrometheusRule.
The metrics ingress policy allows Prometheus Pods identified by
`app.kubernetes.io/name: prometheus` from any namespace.
Clusters without those APIs still expose the Operator's metrics Service.

Your Prometheus discovery policy must select the Operator namespace and its monitoring resources.
Resource measurements use existing kubelet/cAdvisor and CSI volume metrics.
The Operator does not install a monitoring stack or request access to kubelet proxies.
See [resource metrics](usage.md#resource-metrics) for attribution and data availability.
Offline rendering can supply `--api-versions monitoring.coreos.com/v1/ServiceMonitor`
and `--api-versions monitoring.coreos.com/v1/PrometheusRule`.

Helm checks merged values against [values.schema.json](../chart/values.schema.json).
This validates input structure and required settings, rather than workload readiness.
Scheduling, resources and security settings remain in the default values file.

### Upgrade AgentRally

For an OCI installation, replace `<version>` with the [latest release](https://github.com/chizhang21/agentrally-operator/releases/latest) version, without the leading `v`.
For a local archive, use [Updating a dev build](#updating-a-dev-build).

```sh
CHART_VERSION='<version>'
helm upgrade agentrally-operator oci://ghcr.io/chizhang21/helm/agentrally-operator \
  --version "$CHART_VERSION" \
  -n agentrally-system --reset-then-reuse-values --wait --timeout 5m
```

This updates the Operator and CRDs together.
`--reset-then-reuse-values` uses the new chart defaults and preserves previous explicit
settings, including pull Secrets.
Pass `-f` or `--set` to change those settings.
Update explicitly pinned `image.tag` or `image.digest` values for the new release, including those under `connector.image`.
No separate `kubectl apply` of AgentRally CRDs is needed.

Uninstall retains CRDs and Agent resources.
Clean up Agents and Groups while the Operator is still running; see
[Lifecycle](usage.md#lifecycle).

### Upgrade agent-sandbox

Upgrade AgentRally to a build containing this compatibility fix **before** upgrading
agent-sandbox from 1.0.5 to 1.0.11.
Operator 0.0.6 does not support the new template default added by agent-sandbox 1.0.11;
it can reject existing revisions with `DefinitionRevisionConflict`.

After upgrading AgentRally, apply the target manifest in place:

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.11/sandbox-with-extensions.yaml && kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
```

The manifest updates CRDs, RBAC and the Core/Extensions controller together.
Do not uninstall the dependency or delete its CRDs as an upgrade step.

The upgrade test checks the CRD update and controller rollout separately.
It verifies the original Instance, Claim, Sandbox and PVC UIDs and accepted revision.
Running Agents remain usable; suspended Agents stay stopped until explicitly resumed.
Both private and external PVC files must remain readable and writable after resume.
A controller's readiness alone does not verify this contract.

## Local development and testing

Complete [Development: Package versions](development.md#package-versions), then
[install agent-sandbox](#install-upstream-agent-sandbox).
From the repository root, install the default dev archive:

```sh
CHART_PACKAGE="./dist/agentrally-operator-0.0.0-dev.tgz"
helm install agentrally-operator "$CHART_PACKAGE" \
  -n agentrally-system --create-namespace --wait --timeout 5m
```

For an existing Helm release, use [Updating a dev build](#updating-a-dev-build).

If you built with an explicit `VERSION`, set `CHART_PACKAGE` to that version's archive.

The archive references the image built and pushed during development.
Add `-f myvalues.yaml` for custom values; the packaged image needs no override.

### Private images

If your development image requires authentication, create its pull Secret before
installing:

```sh
kubectl create namespace agentrally-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agentrally-system create secret generic registry-credentials \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=/path/to/registry-config.json
```

Use a Docker config containing credentials for your registry.
Pass the Secret name directly; the packaged image settings need no overrides:

```sh
helm install agentrally-operator "$CHART_PACKAGE" \
  --set 'imagePullSecrets[0].name=registry-credentials' \
  -n agentrally-system --wait --timeout 5m
```

Keep the `--set` argument quoted so shells such as zsh do not expand the brackets.

The pull Secret applies to both Operator and Connector Pods.
Private Agent images need their own pull Secrets in the Group namespace, configured in
AgentDefinition.container or SandboxTemplate.

### Updating a dev build

Rebuild, push and package using [Development](development.md#package-versions), then
upgrade from the new archive:

```sh
CHART_PACKAGE="./dist/agentrally-operator-0.0.0-dev.tgz"
helm upgrade agentrally-operator "$CHART_PACKAGE" \
  -n agentrally-system --reset-then-reuse-values --wait --timeout 5m
```

This uses the [same upgrade behavior](#upgrade-agentrally) as a published chart, preserving
explicit settings such as `imagePullSecrets`.
Explicit image overrides also carry over; update them with `--set` if the image
reference changed.

The dev tag stays `v0.0.0-dev`.
Its packaged pull policy is `Always`, but existing Pods need a restart to use the
replacement image:

```sh
kubectl -n agentrally-system rollout restart deployment \
  -l app.kubernetes.io/instance=agentrally-operator
kubectl -n agentrally-system rollout status deployment \
  -l app.kubernetes.io/instance=agentrally-operator --timeout=5m
```

This selector restarts both Deployments when Connector is enabled.
Restarting only the Operator Deployment leaves Connector running.
If you used a different Helm release name, replace `agentrally-operator` in the selector.
