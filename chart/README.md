# AgentRally Operator chart

See [Installation](../docs/installation.md) for public and local installation.
All settings are in [values.yaml](values.yaml).

## Chart sources

- `templates/crds/` is generated from `api/v1alpha1/*_types.go`, including Helm release metadata. Each CRD is stored once.
- `templates/roles.yaml` is generated from `internal/controller/rbac.go`; its RBAC rules are plain YAML.
- Other files in `templates/` define the Operator Deployment, Services and RBAC bindings.

Change Go types or RBAC markers, run `make generate`, and commit the
CRDs, RBAC and DeepCopy output. `make check` detects generation differences.
Do not edit generated YAML directly. Tests are in [tests/packaging](../tests/packaging).

Resource templates show selectors, probes and container security settings directly.
Only common release labels and image references use shared helpers. Values control
scheduling, resources and optional Connector/monitoring objects.

Build identities, local archives and OCI upload commands are documented in
[Development](../docs/development.md#package-versions). Install public charts,
local archives and private images through [Installation](../docs/installation.md).

## Scheduling and Connector

`nodeSelector`, `affinity` and `tolerations` apply to the Operator Pods.
Published images currently target `linux/amd64`, matching the default node selector.
For an image you build for another platform, override both the image and selector.
`startupProbe` allows up to five minutes for initialization before liveness checks
begin; adjust its timing for your API server's startup latency.
The Operator uses Lease leader election. Set `connector.enabled: true` to run
the outbound Connector inside the elected Operator process. Its backend
credentials are mounted into the Operator Pods, and additional CR write
permissions are granted to the Operator ServiceAccount only when enabled.
See [Connector configuration](../docs/outbound-connector.md#installation).
The top-level `resources` values cover both controllers and Connector.

The Operator supports OpenShift's restricted-v2 SCC. Its image runs non-root;
the chart leaves `runAsUser` and `fsGroup` unset for namespace-assigned values.
Agent image permissions are covered in [Usage](../docs/usage.md#process-permissions).

## Monitoring

Metrics use an internal ClusterIP Service on port 8080; health probes use 8081.
Set `metrics.serviceMonitor.enabled` for Prometheus Operator or OpenShift user
workload monitoring. The ServiceMonitor API must already exist.
Use `metrics.serviceMonitor.labels` to match the Prometheus selector.
Offline rendering needs `--api-versions monitoring.coreos.com/v1/ServiceMonitor`.
Business gauges and lifecycle histograms use this same endpoint; see
[metrics and status](../docs/usage.md#status-and-diagnostics).

The optional metrics NetworkPolicy selects Operator Pods and permits port 8080
from namespaces labeled `metrics=enabled`. Enable it with
`metrics.networkPolicy.enabled` and adjust `metrics.networkPolicy.namespaceSelector`
for your monitoring namespaces. The CNI must enforce NetworkPolicy.

## Upgrades

CRDs are part of the Helm release and update with the Operator. Review API schema
changes before upgrading. Commands for [public charts](../docs/installation.md#upgrade-agentrally),
[local dev archives](../docs/installation.md#updating-a-dev-build)
are in Installation. Those commands preserve explicit values, including private
image pull Secrets. No separate CRD apply is needed.

### Uninstall and rollback

The CRDs have `helm.sh/resource-policy: keep`. Uninstall removes the Operator
but keeps its CRDs and Agent resources. Clean up Agents and Groups while the
Operator is still running; see [Lifecycle](../docs/usage.md#lifecycle).

Rollback restores a previous chart's CRD definitions and Operator image.
Review schema differences before rolling back.
