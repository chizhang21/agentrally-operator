# Deploying and operating agents

This guide covers Agent configuration and day-to-day operations after
[installing the controllers](installation.md).
Build and test instructions are in [Development](development.md).

Use a `v0.0.3` Operator checkout with Chart `0.0.3`.
The shared [Group](../examples/02-group.yaml) and [Definition](../examples/03-definition.yaml)
start the public demo Agent without additional configuration.
Optional fields remain commented; prepare their dependencies before enabling them.
Create the Group, definition and instance in order.
Run the commands below from the repository root:

```sh
kubectl apply -f examples/02-group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/03-definition.yaml -f examples/04-instance-basic.yaml && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=300s
```

Follow the [README walkthrough](../README.md#3-call-the-agent) to call the Agent,
suspend/resume and clean up.
The demo returns a fixed response until model credentials are configured.

## Users and permissions

Administrators install controllers, create Groups, set policies and grant access.
Direct Kubernetes access uses the caller's existing RBAC permissions.

When using an Orchestrator, users authenticate through the backend.
The backend authorizes its users and sends scoped management requests through Connector.
Those users need no direct Kubernetes access.

The Chart installs runtime permissions for the Operator and its optional Connector.

## Agent templates

On AgentDefinition, choose `spec.template: generic`, `assistant` or `coding` and set
`container.image`.
For sidecars or other Pod settings, use `templateRef.name` to select a SandboxTemplate
in the same namespace, without `template` or `container`.
A native SandboxTemplate is optional.
If using one, refer to [sandbox-template.yaml](../examples/sandbox-template.yaml).
See [environment settings](agent-templates.md) for defaults and template constraints.

Set `AgentInstance.spec.definitionRef.name` to that definition.
Both resources must reference the same Group.

Definitions publish immutable configuration; instances pin it at allocation.
See [version selection and workspace lifetime](#lifecycle).

Use [03-definition.yaml](../examples/03-definition.yaml) and
[04-instance-full.yaml](../examples/04-instance-full.yaml) as the configuration and instance field references.
For the smallest Instance declaration, use [04-instance-basic.yaml](../examples/04-instance-basic.yaml).
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

### Restrict outbound traffic

Use `AgentGroup.spec.networkPolicy` for native Kubernetes egress rules.
The [Group reference](../examples/02-group.yaml) includes a commented policy for DNS,
a selected service and an external CIDR with an excluded range.
The basic deployment uses the cluster's existing network policy.
To enable the example policy, adapt and uncomment its selectors and destinations.
Include the model provider, Gateway and monitoring clients required by your application.

For default-deny egress, set `policyTypes: [Egress]` with no egress rules.
Omitting `policyTypes` follows Kubernetes defaults: Ingress, plus Egress only when
there are egress rules.
An empty `egress` list alone does not enable outbound isolation.
Omitting a port's protocol means TCP.

AgentRally adds no implicit DNS, same-Group or internet allowance.
Allow both UDP and TCP port 53 for the cluster's actual DNS path.
Adapt the example for NodeLocal DNS or a different DNS deployment.
Allowing DNS queries does not restrict subsequent connections by domain name.
Domain allowlists require a suitable CNI extension and are outside this native API.

Put namespace and Pod selectors in the same peer to require both to match.
Separate peers are alternatives.
Policies selecting a Pod are additive: another policy can permit a path that this
Group policy omits.
AgentRally manages only `agentrally-group-policy` and preserves other policies.
Control who can create policies and change destination labels when relying on this boundary.

Validate allow and deny paths on the target CNI.
Service address translation can change which addresses a policy sees.
A Pod egress policy does not establish node or hostNetwork isolation.
Storage/CSI traffic from the node needs separate controls.
`NetworkPolicySupported` records discovered configuration; it does not prove
packet filtering or change `enforcementVerified` to true.
See the [Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

### Execution constraints

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
See the [full Group reference](../examples/02-group.yaml).

To stop new allocations, set `spec.allocationPaused: true`.
Waiting instances report `AllocationPaused` without starting a new allocation budget.

Already submitted allocations may finish; existing agents can continue, suspend, resume
or be deleted.
Unpause the Group to allow new allocations.
This does not suspend allocated agents.
For Definitions with an explicit prewarming policy, it also drains desired spare capacity
to zero; upstream work already submitted can finish.
On-demand pools without a Definition preparation policy keep their configured spare behavior.

Restart cleanup can finish while paused; the replacement waits for unpause.


### Agent process identity

Built-in environments default to `security.profile: Restricted`, requiring non-root
execution and dropping all Linux capabilities.
Set `AgentDefinition.spec.security.runAsUser` for an explicit UID, or omit it to use the
image or admission default.

`security.serviceAccountName` selects an existing namespaced ServiceAccount.

For images requiring UID 0 or runtime-default capabilities, an administrator prepares an AgentGroup with
`spec.podSecurity: Baseline` and a dedicated ServiceAccount.
The AgentDefinition can then use a built-in environment with AnyUID:

```yaml
template: assistant
security:
  profile: AnyUID
  serviceAccountName: agent-runtime
  runAsUser: 0
```

AnyUID leaves built-in capabilities at the container runtime defaults.
Native SandboxTemplate capability settings remain as declared.
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
[04-instance-full.yaml](../examples/04-instance-full.yaml).
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
[03-definition.yaml](../examples/03-definition.yaml) injects `OPENAI_BASE_URL`,
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
Application paths, probes and upgrade steps belong to each project in `agentrally-examples`.

### Initialize from a VolumeSnapshot

Set the native PVC `dataSource` (or local `dataSourceRef`) to a ready
`snapshot.storage.k8s.io/VolumeSnapshot` in the instance namespace.
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

Lifecycle condition changes are published as Kubernetes Events after a successful status write.
The timing fields remain on the Instance; lifecycle histograms are not exported.
Events are short-lived operational records, rather than a durable audit log.

### Resource metrics

Monitoring is part of the default Chart installation.
ServiceMonitor and PrometheusRule resources are created when their APIs are available.
The default scrape keeps AgentRally measurements and excludes Operator process,
workqueue and reconciliation metrics.

The Operator exports cached state and resource attribution:

| Metric | Observation |
| --- | --- |
| `agentrally_agentinstances{state}` | Cached total, ready, available, suspended, pending and expired counts |
| `agentrally_pool_spare_replicas{state}` / `agentrally_pools` | Ready/desired spare capacity and pool count from Group summaries |
| `agentrally_namespace_pvcs` / `agentrally_namespace_storage_bytes{state}` | Namespace PVC count and requested/provisioned capacity, including external PVCs |
| `agentrally_instance_pod_info` | Instance and accepted Definition identity for the observed execution Pod |
| `agentrally_instance_volume_info` | Whole-PVC association, observed identity and whether the claim is resolved |
| `agentrally_observation_success` | Whether cached resource collection succeeded |

The default recording rules join this attribution with existing Prometheus samples:

| Recorded measurement | Source |
| --- | --- |
| `agentrally_instance_cpu_usage_cores` / `agentrally_definition_cpu_usage_cores` | Five-minute rate of `container_cpu_usage_seconds_total` |
| `agentrally_instance_memory_working_set_bytes` / `agentrally_definition_memory_working_set_bytes` | `container_memory_working_set_bytes` |
| `agentrally_instance_volume_used_bytes` | `kubelet_volume_stats_used_bytes` for each resolved PVC association |
| `agentrally_instance_storage_used_bytes` / `agentrally_definition_storage_used_bytes` | Whole-volume used bytes, aggregated only when all associated PVCs have samples |

CPU and memory cover the Instance's execution containers, including sidecars.
Instance measurements identify their namespace, Instance and accepted Definition using names and UIDs.
Definition CPU and memory totals sum their Instances.
Shared storage is displayed as whole-PVC usage for each Instance.
Attribution includes workspace claims, native volume claim templates and direct PVC references in SandboxTemplate.
The Definition storage total counts each associated PVC once, rather than summing duplicate Instance readings.
It cannot distinguish each writer's files or subdirectory usage.

The cluster monitoring stack must already collect kubelet/cAdvisor and volume statistics.
CSI drivers and volume modes differ in their support for volume usage reporting.
Unmounted volumes and unsupported backends may have no usage samples.
Missing usage and unresolved PVC identities do not become zero, nor a partial total presented as complete.
These are observed volume statistics, not storage-provider billing or physical allocation measurements.

Attribution comes from cached Kubernetes state and is eventually consistent.
Collection failure omits state gauges instead of publishing false zeros.
Credential values and application content are never metric labels.

### Readiness reasons

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
Upgrade by setting `spec.revision` to the desired hash from the Definition's
`status.currentRevision`, and change `restartToken` in the same update.
Changing only the selected revision or rotating environment credentials can report
RestartRequired until restart.

An optional `AgentDefinition.spec.invocationProtocol: HTTPJSONSSEV1` declares an
application protocol and becomes part of its immutable revision.
It does not enable invocation transport.
Controlled testing additionally requires the default-off [invocation preview](outbound-connector.md#invocation-preview) and a ConnectorOnly Group.
The [Connector capability query](outbound-connector.md#capability-discovery)
reports which operations this Operator actually supports.

ConfigurationApplied checks the owned Pod's creation revision; it does not prove
application hot reload.

Secret volumes retain Kubernetes update behavior; AgentRally does not freeze or copy Secret
contents.
The pending-update condition describes creation evidence.

AgentRally replaces execution through its owning SandboxClaim.
**Default Sandbox-owned workspaces are replaced:** upstream garbage collection removes the old Sandbox
and its PVCs.
An external `existingClaimRef` retains its PVC across replacement.
Suspend/resume and Pod recreation within the same Sandbox retain its PVCs.

Delete AgentInstances, then
AgentDefinitions, before the Group.
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

The optional `lifecycle` block in [04-instance-full.yaml](../examples/04-instance-full.yaml) controls expiry:

| Field | Behavior |
| --- | --- |
| `shutdownTime` | Absolute deadline, including time waiting for dependencies or allocation. Also applies to failed and suspended agents. |
| `maxLifetimeSeconds` | Wall-clock execution limit from validated allocation, including suspension. |
| `ttlSecondsAfterFinished` | Starts when Core confirms that the current owned Pod completed naturally. Suspension does not start this TTL. |
| `retainFailed` | True by default; false allows completion TTL to remove failed executions. |
| `shutdownPolicy: Retain` | Removes execution and interaction resources, keeps the instance with `Expired=True`. |
| `shutdownPolicy: Delete` | Deletes the instance through its finalizer. |

The earliest applicable execution deadline wins.
Users can update `AgentInstance.spec.operatingMode` directly.
When an Orchestrator manages agents, it owns activity tracking and the decision to
suspend or resume, and sends native CR updates through Connector.
The Operator reconciles the requested state without inferring idleness.

Sandbox-owned workspace PVCs are removed with execution; externally supplied Secrets and
externally managed workspace PVCs remain.
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
