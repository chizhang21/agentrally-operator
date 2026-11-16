# Deploying and operating agents

This guide covers Agent configuration and day-to-day operations after
[installing the controllers](installation.md).
Build and test instructions are in [Development](development.md).

Use the source tag matching your installed Chart.
See [installation](installation.md#install-agentrally) for the latest release.
The shared [Group](../examples/02-group.yaml) and [Definition](../examples/03-definition.yaml)
start the public demo Agent without additional configuration.
Optional fields remain commented; prepare their dependencies before enabling them.
Create the Group, definition and instance in order.
Run the commands below from the repository root:

```sh
kubectl apply -f examples/02-group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/03-definition.yaml -f examples/04-instance-basic.yaml && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=300s
```

Verify the demo through its Pod:

```sh
POD=$(kubectl -n agentrally-demo get agentinstance demo-agent -o jsonpath='{.status.podName}')
kubectl -n agentrally-demo port-forward "pod/$POD" 8080:8080
```

In another terminal:

```sh
curl -fsS http://127.0.0.1:8080/chat -H 'Content-Type: application/json' -d '{"message":"Hello"}'
```

Expect `{"response":"AgentRally demo agent is ready.","mode":"demo"}`.
This validates the deployment without model credentials or a model-provider call.

For a persistent workspace, apply `examples/04-instance-full.yaml` after the same
Group and Definition.
It requests a 20 GiB filesystem PVC from the default StorageClass.
Set `storageClassName` or adjust capacity to match the cluster before applying.
The commented HTTPRoute requires Gateway API and the referenced Gateway only if enabled.
Use the full Instance's `status.podName` for access and repeat port-forward after any
operation that replaces its Pod.
Deleting the full Instance also deletes its template-created workspace PVC.
An `existingClaimRef` PVC is independently owned and remains after Instance deletion.

For a complete application walkthrough, choose an example from
[Start an Agent](../README.md#2-start-an-agent).
See [Lifecycle](#lifecycle) to suspend, resume and clean up the resources above.
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

Allocated instances can still restart or upgrade in place.
Retrying an unfinished allocation waits for unpause.


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

For storage retained after Instance deletion, use an existing PVC on the instance:

```yaml
workspace:
  mountPath: /workspace
  existingClaimRef:
    name: my-retained-state
```

The Operator records its UID without adoption or deletion and enforces zero spares.
Use one PVC per resident assistant; a retained volume is not a backup.
Application paths, probes and upgrade steps belong to each project in `agentrally-examples`.

### Private workspaces and shared content

Use `workspace` for each instance's main workspace and a custom
[SandboxTemplate](../examples/sandbox-template.yaml) for additional mounts.
The example mounts an existing `shared-content` PVC at `/shared`, read-only in both
`volumes[].persistentVolumeClaim` and `volumeMounts`.
It leaves `/workspace` for the instance configuration.

After creating the Group, prepare a shared filesystem PVC in its namespace:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-content
  namespace: agentrally-demo
spec:
  storageClassName: your-shared-filesystem-class
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
```

Choose a class that supports concurrent mounts on the target nodes, such as an
administrator-configured NFS or EFS class.
Publish files into stable directories such as `v1/skills.txt` and `v2/skills.txt`.
Prepare ownership and permissions for the Agent's non-root UID before deployment.
`fsGroup` is not a substitute for server-side permissions and may not apply to NFS.

To use this template:

1. In `03-definition.yaml`, remove `template` and `container`, then enable
   `templateRef.name: custom-agent`.
   Set `security.runAsUser` only when an explicit UID is required by the storage policy.
2. Apply `sandbox-template.yaml`, then the modified Definition.
3. Create two instances referencing that Definition, with different names.
   Each can use the full example's `workspace.volumeClaimTemplate` with the chosen
   StorageClass to obtain its own PVC.
   For externally retained private data, replace that block with
   `existingClaimRef.name` pointing to a separately prepared PVC for that instance.

The shared mount and each private workspace can use the same filesystem backend.
Each private PVC must map to a distinct, appropriately authorized data scope.
Native `volumeClaimTemplates` inside SandboxTemplate also create one PVC per Sandbox;
sharing a template does not share those volumes.

Inspect the mount in either running instance:

```sh
POD=$(kubectl -n agentrally-demo get agentinstance <instance-name> -o jsonpath='{.status.podName}')
kubectl -n agentrally-demo exec "$POD" -- sh -c 'cat "$SHARED_CONTENT_PATH/skills.txt"'
kubectl -n agentrally-demo exec "$POD" -- sh -c 'printf test >> "$SHARED_CONTENT_PATH/skills.txt"'
```

The read should return the published content; the write should fail as read-only.
`SHARED_CONTENT_PATH` is an application convention, not an Operator skills loader.
The demo's HTTP handler does not interpret these files.

To select new shared content, change the source template's `SHARED_CONTENT_PATH`
from `/shared/v1` to `/shared/v2` and wait for the Definition's new revision.
[Explicitly upgrade](#lifecycle) only the intended instance.
Other instances retain their accepted revision and content path.
Keep the PVC and mount layout unchanged; replacing storage is not an application upgrade.
Do not overwrite directories still used by old revisions.
Read-only mounts do not prevent the publisher from changing files or prevent Agents
from reading other public version directories on the same mount.

Access modes and different PVC names do not establish tenant authorization.
The Operator adds only the configured mounts; backend exports, POSIX permissions,
network access and any EFS Access Point/IAM restrictions remain administrator responsibilities.
Same-Group sharing does not provide protection from an administrator who can change templates.
PVC references are namespace-local; prepare separate claims for other Groups.

Missing PVCs appear in Instance conditions.
Mount failures also require inspecting the affected Pod's Events and the storage backend.
AgentRally does not substitute an empty directory or relax workload permissions.

Deleting an instance removes its template-created private PVC.
Its external private PVC and the shared PVC remain independently owned.
The Kind NFS test covers actual file access, permission rejection and these lifecycle
behaviors; it does not validate EFS-specific authorization or hostile NFS clients.

### Initialize from a VolumeSnapshot

Set the native PVC `dataSource` (or local `dataSourceRef`) to a ready
`snapshot.storage.k8s.io/VolumeSnapshot` in the instance namespace.
AgentRally checks that the snapshot exists, is not terminating, reports `readyToUse: true`,
and fits within the requested storage capacity when `restoreSize` is reported.
New allocations include the source UID in configuration identity, so an old warmed
workspace cannot be reused for a recreated source.

After allocation, suspend/resume, restart and revision updates reuse the private workspace,
even if its initialization snapshot is deleted or no longer ready.
Creating a new instance needs a ready source again.

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
rotated dependency values await an explicit restart.
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
Pod timings reset with a new Pod UID; in-place updates retain the original allocation timing record.

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
| `agentrally_agentinstances{state}` | Cached total, ready, available, suspended and pending counts |
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
| `PoolAllocationLost`, `WorkspaceIdentityChanged` | A pinned Claim, Sandbox or PVC is missing or has changed identity; inspect the original resources. Restart does not replace an established allocation. |
| `StorageChangeRejected` | Keep the allocated volume identities and mount layout; storage changes require a separate instance. |
| `RestartRequired` | Referenced dependency values changed; set a new restart token to apply them. The old instance may remain Available. |
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
Suspend, then remove the explicit mode to resume:

```sh
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Suspended --timeout=120s
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":null}}'
```

The current-generation `Suspended=True` condition confirms that the Pod is gone.
Core recreates it on resume, possibly with the same name but a new UID.
Use `status.podUID` to distinguish executions.
PVC data remains; process memory and emptyDir data do not.
This is stop/start, not checkpoint/restore.

To restart the process using the same Definition revision, set a new restart token:

```sh
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge \
  -p '{"spec":{"restartToken":"restart-1"}}'
```

Change the token for each restart; repeating it does nothing.
Definition edits publish a new revision and leave pinned instances running on their old
version.
Restart preserves the accepted revision and its invocation protocol declaration.
Upgrade by setting `spec.revision` to a published revision.
For example, select the Definition's current revision:

```sh
revision=$(kubectl -n agentrally-demo get agentdefinition demo-agent -o jsonpath='{.status.currentRevision}')
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge \
  -p "{\"spec\":{\"revision\":\"$revision\"}}"
```

A revision change starts the upgrade without a new restart token.
Repeating the same revision does not restart the process.
Rotating referenced environment credentials may report `RestartRequired`; use a new
restart token to apply their current values.

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

Restart and upgrade keep the same Instance, SandboxClaim, Sandbox and PVC UIDs.
The old Pod must terminate before the new configuration starts.
An upgrade interrupts running tasks and loses process memory and ephemeral files.
It does not migrate application data or undo writes made by a failed version.

Definition storage defaults apply to new instances.
Existing instances keep their allocated volumes; incompatible mount changes or explicit
Instance storage changes report `StorageChangeRejected` before stopping the old process.
Missing or replaced PVCs block the update rather than being accepted as restored data.

`status.provisioning` records the original allocation.
`status.configuration` identifies the applied Sandbox configuration source.
`status.update` records an accepted update in `Stopping` or `Starting`.
A suspended instance can prepare its target without starting a Pod.
Resume it explicitly to verify the new process.
Check current-generation `Ready` and `ConfigurationApplied`, and the new `status.podUID`,
before treating a running update as complete.

Deleting the Instance removes its Sandbox and Sandbox-owned PVCs.
An external `existingClaimRef` remains under its original ownership.

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

### Natural completion

A task's natural completion is reported through `Finished` and
`status.lifecycle.finishedAt` / `finishedReason`.
Successful and failed executions remain available for inspection until explicitly deleted.
Suspension does not count as task completion.

Users or Orchestrators decide when to suspend, resume or delete an Instance.
The Operator reconciles those explicit requests without a business lifetime or idle timer.

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
Startup failures or delays are reported without deleting the allocated execution.
Delete the Instance explicitly when the execution is no longer needed.

A timeout while still waiting for valid allocation removes the native Claim; change
restartToken to retry.

External pools must reference the matching AgentRally-generated configuration snapshot; the
instance continues to select its original source.
AgentRally does not modify the pool.
Configuration edits do not switch an active allocation.
See [resource ownership and cleanup](architecture.md#resources-and-ownership).
