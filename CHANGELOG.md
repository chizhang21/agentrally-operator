# Changelog

## Unreleased

- Provision an independent workspace PVC for each Sandbox, including warm spares,
  from `workspace.volumeClaimTemplate`. Keep data during suspension and reclaim
  the workspace when its Sandbox is replaced or removed.
- Run the optional Connector inside the elected Operator process, controlled by
  `connector.enabled`, without a separate executable or Deployment.
- Keep one generated copy of each CRD in `chart/templates/crds/`; Helm upgrades
  schemas with the Operator and retains CRDs on uninstall. Use `make generate`
  for DeepCopy, CRDs and controller RBAC.
- Exclude suspension from completion TTL and clear stale completion timestamps
  before expiring a resumed execution.
- Require current desired configuration for `Ready`; report current execution
  health separately as `Available`. Configuration changes require explicit restart.
- End the allocation budget at validated Sandbox assignment instead of Pod
  readiness. Enforce pending budgets and suspension before new dependency checks.
- Default new managed pools to zero spares, preserve existing replica settings,
  and leave node scheduling decisions to Kubernetes.
- Include the selected Agent container in configuration identity and account for
  Kubernetes probe, DownwardAPI and projected-token defaults when observing Pods.
- Add a real upstream kind lifecycle test for manual runs and version-tag release
  gates; PRs compile the E2E code without creating a cluster.
- Exclude local credentials from Chart archives and narrowly suppress a dependency
  index false positive in credential scanning.
- Bound allocation names for downstream Services, reject external pool DNS defaults
  that would change after allocation, and preserve recorded allocation names.
- Allow suspension during Group preparation and request execution cleanup before
  waiting for optional route finalizers.
- Clear stale positive readiness when the execution Pod is missing; accept
  ServiceAccount-injected pull credentials without masking explicit drift.
- Allow timeout-only updates on default Claims while retaining immutable pool references.
- Restrict CNI evidence to system or administrator-approved namespaces and ignore
  Node heartbeat-only discovery events.
- Validate Chart values and labels, select the published image architecture, and
  give initialization a separate startup-probe budget.
- Verify generated files in isolation, and run CRD lifecycle plus Connector
  election/readiness integration checks in CI.

CRD validation changes require an upgrade with the matching Chart. Rollback does
not migrate stored resources; review the [upgrade procedure](docs/installation.md).
Package versions are selected at build time; this file is not a second version source.
