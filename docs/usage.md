# Deploying and operating agents

This guide covers Agent configuration and day-to-day operations after
[installing the controllers](installation.md).
Build and test instructions are in [Development](development.md).

From a local Operator checkout, replace the image in
[definition.yaml](../examples/base/definition.yaml), then create the Group, definition
and instance in order.
Run the commands below from the repository root:

```sh
kubectl apply -f examples/base/group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/definition.yaml -f examples/base/agent.yaml
```

## Users and permissions

Administrators install controllers, create Groups, set policies and grant access.
Agent deployers manage definitions and instances in their assigned namespace using the
[editor role and RoleBinding](../examples/rbac/README.md).
This role can read status but cannot write it.

Templates, Secrets, PVCs and Pod inspection need separate permissions.

The Operator has its own installation-managed RBAC.

## Agent templates

On AgentDefinition, choose `spec.template: generic`, `assistant` or `coding` and set
`container.image`.
For sidecars or other Pod settings, use `templateRef.name` to select a SandboxTemplate
in the same namespace, without `template` or `container`.
See [environment settings](agent-templates.md) for defaults and template constraints.

Set `AgentInstance.spec.definitionRef.name` to that definition.
Both resources must reference the same Group.

Definitions publish immutable configuration; instances pin it at allocation.
See [version selection and workspace lifetime](#lifecycle).

Use [full-definition.yaml](../examples/full-definition.yaml) and
[full.yaml](../examples/full.yaml) as the configuration and instance field references,
or start from a focused [example](../examples/README.md).
To inspect the installed schema:

```sh
kubectl explain agentdefinition.spec --recursive
kubectl explain agentinstance.spec --recursive
```

## Group namespace and security

AgentGroup creates `agentrally-<group-name>`; read `status.namespace` for the resolved name.
An explicit `spec.namespace` must start with `agentrally-` and cannot change.
Definitions, instances, Secrets, ConfigMaps, PVCs and source templates belong in that
namespace.
AgentRally does not adopt an unrelated existing namespace.

Separate namespaces do not block traffic.
For same-Group ingress only, add to `AgentGroup.spec`:

```yaml
networkPolicy:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
  - from:
    - podSelector: {}
```

This leaves egress open.
Add ingress rules for the Gateway and monitoring where needed.
NetworkPolicy requires an enforcing CNI.
Templates delegate network policy to the Group.
Pod Security defaults to Restricted.

Use `spec.executionPolicy.allowedRuntimeClasses` to require an explicitly selected runtime from an administrator's list.
Use `spec.executionPolicy.requireImageDigest: true` to require digest-pinned regular and init containers.
Both constraints apply to the final resolved Pod, including custom templates.
An omitted policy preserves existing behavior.
RuntimeClass selection is configuration evidence; it does not attest the node runtime.
Policy checks gate new AgentRally allocations and resumes.
Already submitted allocations and running executions may continue after a policy change.
Existing native pool snapshots are not rewritten by this policy.
Use Kubernetes admission controls when independently submitted native workloads must obey the same constraints.

`spec.limitRange` accepts a native LimitRange spec for container/Pod/PVC defaults and
limits.
AgentRally owns `agentrally-limits` in the Group namespace.
Removing the field removes that policy; changes apply to newly admitted resources.

Admission-added resource defaults do not cause `RestartRequired`; explicit requested
quantities remain part of configuration checks.
See [Group policy](../examples/group-policy.yaml).

To stop new allocations, set `spec.allocationPaused: true`.
Waiting instances report `AllocationPaused` without starting a new allocation budget.

Already submitted allocations may finish; existing agents can continue, suspend, resume
or be deleted.
Unpause the Group to allow new allocations.
This does not suspend allocated agents.
For Definitions with an explicit prewarming policy, it also drains desired spare capacity
to zero; upstream work already submitted can finish.
Legacy pools without this policy retain their existing spare behavior.

Restart cleanup can finish while paused; the replacement waits for unpause.

<a id="legacy-agent-images-on-openshift"></a>

### Agent process identity

Built-in environments use non-root containers.
Set `AgentDefinition.spec.security.runAsUser` for an explicit UID, or omit it to use the
image or admission default.

`security.serviceAccountName` selects an existing namespaced ServiceAccount.

For images requiring UID 0, an administrator prepares a Group with
`spec.podSecurity: Baseline` and a dedicated ServiceAccount.
The definition can then select `security.profile: AnyUID` and the required UID.
Namespace policy and ServiceAccount authorization remain administrator-managed.

## Interaction endpoint

Declare the application's named TCP `http` port, or its sole TCP port, in the definition
container or native template.
Then add to `AgentInstance.spec`:

```yaml
interaction:
  type: HTTPRoute
```

Omit interaction for no managed endpoint; removing it deletes the HTTPRoute and access
Service.

For HTTPRoute, shared settings belong in `AgentGroup.spec`:

```yaml
interactionDefaults:
  httpRoute:
    gatewayRef:
      name: shared
      namespace: gateway-system
      sectionName: https
    domain: agents.example.com
```

The Gateway must allow HTTPRoutes from the Group namespace.
Configure DNS and TLS for the domain; AgentRally creates neither.

If omitted, AgentRally discovers an eligible Gateway/listener and uses its wildcard
hostname.
A unique eligible default Gateway takes precedence; multiple eligible Gateways/listeners
require an explicit choice.
An IP alone cannot supply a domain.
HTTPS is preferred within the selected Gateway.

instances can override port, hostname, scheme and Gateway; see
[full.yaml](../examples/full.yaml).
An omitted Gateway namespace means the instance's namespace.

The backend serves HTTP; the Gateway handles external TLS termination.
The access Service uses port 80.
Allow its traffic through Group NetworkPolicy and provide application authentication in
the agent or ingress infrastructure.

Read the endpoint URL once `InteractionReady=True`:

```sh
kubectl -n agentrally-demo get agentinstance demo-agent \
  -o jsonpath='{.status.interaction.url}{"\n"}'
```

InteractionReady checks route acceptance and workload readiness, not end-to-end
connectivity or authentication.
Suspension retains the URL but makes it unavailable.

## Environment variables and Secrets

Create Secrets in the Group namespace.
Under `AgentDefinition.spec.container`:

```yaml
env:
- name: LOG_LEVEL
  value: info
- name: API_KEY
  valueFrom:
    secretKeyRef:
      name: agent-credentials
      key: api-key
```

Custom templates use the native container's env fields.
`envFrom` is also supported.
AgentRally checks references without publishing values and reads Secrets without listing,
watching or caching them.
Periodic reconciliation detects version changes; use a new restart token to apply them
to the process.
The separate `agentrally-examples/operator/http-api-agent` directory contains a complete
application deployment.

## Model and workspace

`AgentDefinition.spec.modelProvider` in
[full-definition.yaml](../examples/full-definition.yaml) injects `OPENAI_BASE_URL`,
`AGENTRALLY_MODEL_NAME` and Secret-backed `OPENAI_API_KEY`.
The application must consume or map these variables.

`agentContainer` selects the target container; the default is the first container.

For a private workspace, declare a native PVC spec in the instance.
Core provisions one PVC per Sandbox, including each unassigned warm instance:

```yaml
workspace:
  mountPath: /workspace
  volumeClaimTemplate:
    accessModes: [ReadWriteOnce]
    resources:
      requests:
        storage: 20Gi
    # storageClassName: your-storage-class
```

This also sets `AGENTRALLY_WORKSPACE_PATH`.
Omit `storageClassName` to use the cluster default.
AgentRally carries the spec through the pool template; Core creates and owns the PVC.

`status.volumeClaims` maps logical volume names to their allocated PVCs.
Pending WaitForFirstConsumer claims can bind after Core creates the Pod.
See
[workspace permissions and defaults](agent-templates.md#persistent-state-and-credentials).

### Platform-managed persistent workspaces (preview)

New Groups can enable `spec.persistentWorkspaces: true` at creation.
This choice cannot change later and does not migrate an existing Group or its PVCs.

An Instance in that Group can request its own persistent workspace:

```yaml
workspace:
  mountPath: /workspace
  persistence:
    retentionSeconds: 86400
  volumeClaimTemplate:
    accessModes: [ReadWriteOnce]
    resources:
      requests:
        storage: 1Gi
```

The Operator creates an `AgentWorkspace` record and one PVC.
It pins their UIDs before permitting a consumer.
The record, PVC, namespace and published revision are protected from ordinary
Instance, Definition and Group deletion.
Group policies and quotas remain with the namespace.

Choose persistence when creating the Instance.
Its workspace configuration is immutable.
The initial PVC must be a new filesystem volume; existing PVCs, snapshots and
pre-bound volumes are not accepted in this mode.
Pending PVCs can proceed to scheduling with `WaitForFirstConsumer`.
Persistent workspaces do not create warm spares.

Suspension and resumption of the same Instance reuse its PVC.
The first Definition revision remains pinned; upgrading that revision is currently
unsupported (`PersistentRevisionPinned`).

**Deletion recovery and automatic cleanup are not available in this preview.**
`retentionSeconds` records the requested future policy; no retention countdown starts,
and even zero does not trigger deletion.
After the Instance is deleted, the record reports `ReleasePending`.
Absence of a Pod does not prove that an old execution can no longer write.
A recreated Instance cannot take over the retained data.

Deleting the workspace record reports `DeletionBlocked` and preserves its finalizer.
Group and Definition deletion remain blocked while any associated record exists.
There is no supported purge command in this version.
Use this preview only where retaining those resources indefinitely is acceptable.
Direct administrator deletion of the namespace or forced removal of finalizers is
outside this protection.

### Externally managed workspaces

For storage retained across execution upgrades, use an existing PVC on the instance:

```yaml
workspace:
  mountPath: /workspace
  existingClaimRef:
    name: my-retained-state
```

The Operator records its UID without adoption or deletion and enforces zero spares.
Use one PVC per resident assistant; a retained volume is not a backup.
See the [retained workspace CR example](../examples/retained-workspace.yaml).
Application paths, probes and upgrade steps belong to each project in `agentrally-examples`.

### Initialize from a VolumeSnapshot

Set the native PVC `dataSource` (or local `dataSourceRef`) to a ready
`snapshot.storage.k8s.io/VolumeSnapshot` in the instance namespace.
See [snapshot-workspace.yaml](../examples/snapshot-workspace.yaml).
AgentRally checks that the snapshot exists, is not terminating, reports `readyToUse: true`,
and fits within the requested storage capacity when `restoreSize` is reported.
New allocations include the source UID in configuration identity, so an old warmed
workspace cannot be reused for a recreated source.

After allocation, suspend/resume reuses that execution's private workspace even if its
initialization snapshot is deleted or no longer ready.
A replacement execution needs a ready source again.

The snapshot CRDs, snapshot controller and a compatible CSI driver must already exist.
The PVC StorageClass must use a driver that can restore that snapshot.
Preflight is read-only; CSI performs restoration.
AgentRally does not create snapshots, copy live workspace data or verify application
consistency.
Cross-namespace snapshot sources are unsupported.

Ordinary workspaces require none of these snapshot APIs.

## Status and diagnostics

```sh
kubectl -n agentrally-demo get agentinstances
kubectl -n agentrally-demo describe agentinstance demo-agent
```

Read Conditions, Events and `status.podName`.
`Ready=True` requires current-generation configuration to be applied and the execution
to be ready, including a requested interaction endpoint.

`Available` reports current Core/Pod health independently.
A healthy old instance can have `Available=True` and `Ready=False/RestartRequired` while
an update waits for explicit restart.
An unverified creation revision cannot produce `Ready=True`.

Neither condition proves application credentials work.

`AgentGroup.status.summary` reports instance counts, native pool capacity and all PVC
requests/provisioned capacity in its namespace.
Ready and Available counts overlap; capacity is not disk usage.
Pool readiness uses current-generation upstream status and excludes terminating/stuck
capacity.

Group `Ready` reports namespace and policy preparation.
`CapacityReady` independently reports managed pool reconciliation and current-generation spare readiness.
An external pool does not gate this condition.

`CleanupReady` on an Instance reports owned execution cleanup and diagnoses stalls without bypassing finalizers.

`status.provisioning.launchType` reports `warm` or `cold` when upstream supplies it;
missing provenance stays unknown.

`AgentInstance.status.timings` records observed Pod creation/first availability,
suspension submission/completion and cleanup submission/completion.
Pod timings reset with a new Pod UID; replacement starts a new execution timing record.

The existing metrics endpoint exposes the following business measurements:

| Metric | Observation |
| --- | --- |
| `agentrally_agentinstances{state}` | Cached total, ready, available, suspended, pending and expired counts |
| `agentrally_pool_spare_replicas{state}` / `agentrally_pools` | Ready/desired spare capacity and pool count from Group summaries |
| `agentrally_namespace_pvcs` / `agentrally_namespace_storage_bytes{state}` | Namespace PVC count and requested/provisioned capacity, including external PVCs |
| `agentrally_agent_allocation_duration_seconds{launch_type}` | Persisted allocation start to validated assignment |
| `agentrally_agent_startup_duration_seconds{launch_type}` | Pod creation to first observed availability |
| `agentrally_agent_suspension_duration_seconds` | Suspension submission to Core confirmation with no remaining Pod |
| `agentrally_agent_cleanup_duration_seconds` | Cleanup submission to owned allocation/access-resource removal |
| `agentrally_observation_success` | Whether cached instance and Group collection succeeded |

Duration metrics are histograms.
The allocation histogram's `_count` series provides observed allocation counts by
`launch_type` (`warm`, `cold`, `unknown`).

Individual instance names and credential values are never metric labels.
Histograms count transitions after successful status writes and reset when the Operator
process restarts; they are not a durable event ledger.

Group summaries are eventually consistent, and a failed cache read omits state gauges
instead of publishing false zeros.

Enable `metrics.serviceMonitor.enabled` when Prometheus Operator is installed.

| Ready reason | What to check |
| --- | --- |
| `RestartRequired` | Desired configuration has changed; set a new restart token when replacement is appropriate. The old instance may remain Available. |
| `AllocationPaused` | The Group has paused new allocations. Unpause it to begin a new request. |
| `PoolConfigurationUnverified` | An external pool must reference the matching Group-owned AgentRally snapshot. |
| `SnapshotAPIUnavailable`, `SnapshotNotFound`, `SnapshotNotReady`, `SnapshotCapacityInsufficient` | Snapshot API/CSI prerequisites, source readiness and PVC restore capacity. |
| `Unschedulable` | CPU/memory requests, node availability, taints, affinity and PVC binding. Recognized scheduler messages identify CPU, memory or taint constraints. |
| `ErrImagePull`, `ImagePullBackOff`, `InvalidImageName` | Image reference, registry access and imagePullSecrets. |
| `CreateContainerConfigError` | Secret/ConfigMap references and container security settings. |
| `CrashLoopBackOff`, `ContainerExited` | Application command, exit code, probes and container logs. |
| `OOMKilled` | Container memory limits and application memory use. |
| `VolumeMountFailed`, `VolumeAttachFailed` | Pod events, CSI, PVC/PV status and access from other nodes. |
| `PodSandboxCreationFailed` | RuntimeClass, node container runtime and CNI; this alone does not identify a runtime or network root cause. |
| `PodStartupStalled`, `ReadinessStalled` | Startup or container readiness has not completed for at least five minutes. |
| `Suspending`, `PodTerminating` | Core is stopping the Pod or waiting for its deletion. |
| `PodTerminationStalled` | Pod remains at least five minutes past its deletion timestamp; ask an administrator to inspect kubelet, runtime, volume teardown and finalizers. |
| `PodObservationBlocked` | Pod ownership or the Operator's Pod read permission. |

Five-minute stalls add diagnostics without forcing deletion or restart.
Inspect the named Pod and its events for original errors; AgentRally does not publish raw
container/Event messages or Secret values.
More detail is in
[reconciliation and status](architecture.md#reconciliation-and-status).

## Lifecycle

Instances run by default.
Suspend, wait for the Pod to disappear, then remove the explicit mode to resume:

```sh
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Suspended --timeout=120s
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":null}}'
```

Core removes and recreates the Pod.
PVC data remains; process memory and emptyDir data do not.
This is stop/start, not checkpoint/restore.

To replace execution using the same definition revision, set a new restart token:

```sh
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge \
  -p '{"spec":{"restartToken":"restart-1"}}'
```

Change the token for each restart; repeating it does nothing.
Definition edits publish a new revision and leave pinned instances running on their old
version.
Restart preserves the accepted revision and its invocation protocol declaration.
For instances without platform persistence, upgrade by setting `spec.revision` to the desired hash from the Definition's
`status.currentRevision`, and change `restartToken` in the same update.
Platform persistent workspaces currently reject revision upgrades; see the
[persistent workspace preview](#platform-managed-persistent-workspaces-preview).
Changing only the selected revision or rotating environment credentials can report
RestartRequired until restart.

An optional `AgentDefinition.spec.invocationProtocol: HTTPJSONSSEV1` declares an
application protocol and becomes part of its immutable revision.
It does not enable invocation transport.
Controlled testing additionally requires the default-off [invocation preview](invocation-protocol.md) and a ConnectorOnly Group.
The [Connector capability query](outbound-connector.md#management-capability-discovery)
reports which operations this Operator actually supports.

ConfigurationApplied checks the owned Pod's creation revision; it does not prove
application hot reload.

Secret volumes retain Kubernetes update behavior; AgentRally does not freeze or copy Secret
contents.
The pending-update condition describes creation evidence.

AgentRally replaces execution through its owning SandboxClaim.
**Default Sandbox-owned workspaces are replaced:** upstream garbage collection removes the old Sandbox
and its PVCs.
An external `existingClaimRef` or platform persistent workspace retains its PVC across replacement.
Suspend/resume and Pod recreation within the same Sandbox retain its PVCs.

For Groups without retained platform workspaces, delete AgentInstances, then
AgentDefinitions, before the Group.
Retained records block parent deletion; the persistent workspace preview has no purge path.
Wait for execution cleanup, then inspect remaining Sandboxes and PVCs before moving or
removing data:

```sh
kubectl -n agentrally-demo delete agentinstance demo-agent --wait=true --timeout=120s
kubectl -n agentrally-demo delete agentdefinition demo-agent --wait=true --timeout=120s
kubectl -n agentrally-demo get agentdefinitions,agentinstances,sandboxes,pvc
```

Deleting the Group's Namespace deletes remaining namespaced resources, including
Secrets.
Do not use Group deletion as a substitute for data retention planning.

### Execution expiry

The optional `lifecycle` block in [full.yaml](../examples/full.yaml) controls expiry:

| Field | Behavior |
| --- | --- |
| `shutdownTime` | Absolute deadline, including time waiting for dependencies or allocation. Also applies to failed and suspended agents. |
| `maxLifetimeSeconds` | Wall-clock execution limit from validated allocation, including suspension. |
| `ttlSecondsAfterFinished` | Starts when Core confirms that the current owned Pod completed naturally. Suspension does not start this TTL. |
| `retainFailed` | True by default; false allows completion TTL to remove failed executions. |
| `shutdownPolicy: Retain` | Removes execution and interaction resources, keeps the instance with `Expired=True`. |
| `shutdownPolicy: Delete` | Deletes the instance through its finalizer. |

The earliest applicable execution deadline wins.
The Operator applies explicit `operatingMode` changes; it does not infer idleness from traffic or Leases.
Observation-driven automatic suspension is currently unsupported.
For upgrades from the old Activity Lease contract, follow the
[migration procedure](activity-migration.md).

Sandbox-owned workspace PVCs are removed with execution; externally supplied Secrets and
externally managed workspace PVCs remain.
Platform persistent workspaces also remain; their release, recovery and GC are unavailable.
`Retain` preserves the AgentInstance and diagnostics; it does not retain Sandbox-owned
private PVCs.

To run an expired retained instance again with a replacement execution, change
`restartToken` and move any elapsed `shutdownTime` into the future or remove it.

Omit lifecycle for no configured expiry.

## Pool allocation

All instances use SandboxWarmPool and SandboxClaim.
AgentRally selects or creates a matching pool; Extensions allocates an instance and refills
capacity.
New managed pools default to zero spares.

Configure `AgentGroup.spec.prewarming` for declarative spares and idle retention; omit
it to preserve manual scaling.
To prepare capacity before the first instance, also enable
`AgentDefinition.spec.prewarming` on a safe stateless application with a readiness probe.
Check the Definition's `Prepared` condition and `status.prewarming` counts separately
from its publication `Ready` condition.
See [pool policy](agent-templates.md#allocation-and-policy).

The optional provisioning block selects a pool or changes the allocation budget:

```yaml
provisioning:
  claimTimeoutSeconds: 60
  # warmPoolRef:
  #   name: tool-environment
```

The default 60-second budget starts at persisted `status.provisioning.startedAt` and
covers capacity/instance creation until an owned, matching Sandbox is observed.

`completedAt` ends that budget independently of Core/Pod readiness.
Startup failures or delays are reported without deleting the allocated execution; use
`lifecycle.shutdownTime` for an overall cleanup deadline.

A timeout while still waiting for valid allocation removes the native Claim; change
restartToken to retry.

External pools must reference the matching AgentRally-generated configuration snapshot; the
instance continues to select its original source.
AgentRally does not modify the pool.
Configuration edits do not switch an active allocation.
See [resource ownership and cleanup](architecture.md#resources-and-ownership).
