# Agent environments and templates

Select `AgentDefinition.spec.template` and fill `spec.container` for a built-in
environment.
The Operator embeds three Pod blueprints, then creates a complete SandboxTemplate
revision during publication.
Instances select a revision, then create a resolved snapshot and pool during allocation.
See [revision selection and lifecycle](usage.md#lifecycle).
Built-in environments do not require a source template.

For other Pod settings, create a SandboxTemplate and use `spec.templateRef`.
`template` and `templateRef` are mutually exclusive.

| Setting | [generic](../internal/templates/generic.yaml) | [assistant](../internal/templates/assistant.yaml) | [coding](../internal/templates/coding.yaml) |
| --- | --- | --- | --- |
| `spec.template` | generic | assistant | coding |
| Agent purpose | Simple custom HTTP Agent | Resident personal assistant, e.g. Hermes/OpenClaw | Repository/terminal Agent, e.g. Pi/Codex |
| CPU request / limit | 250m / 1 | 500m / 2 | 500m / 2 |
| Memory request / limit | 512Mi / 1Gi | 1Gi / 2Gi | 1Gi / 4Gi |
| Environment | LOG_LEVEL=info | HOME=/workspace, TMPDIR=/tmp, LOG_LEVEL=info | HOME=/workspace, TMPDIR=/tmp, LOG_LEVEL=info |
| Working directory | Image default | /workspace | /workspace |
| Workspace | None by default; optional private PVC | Ephemeral /workspace, or private PVC via AgentInstance.spec.workspace | Ephemeral /workspace, or private PVC via AgentInstance.spec.workspace |
| Temporary storage | emptyDir /tmp | emptyDir /tmp | emptyDir /tmp |
| Browser preparation | None | 512Mi memory-backed /dev/shm | 512Mi memory-backed /dev/shm |
| Container runtime | Node default; optional native runtimeClassName | Node default; optional native runtimeClassName | Node default; optional native runtimeClassName |
| Initial spare replicas | 0; prewarming is opt-in | 0; prewarming is opt-in | 0; prewarming is opt-in |

`container.resources` replaces the whole requests/limits block.

All built-ins use Linux nodes, restart Always, a 30-second termination grace period, no
API token automount, non-root execution, dropped capabilities, no privilege escalation
and RuntimeDefault seccomp.
No UID is fixed; images and PVC permissions must support the UID assigned by the
cluster.

The image entrypoint starts the application; `container.command` and `container.args`
can override it.
Set ports and probes for your image.
`full.yaml` uses 8080 and `/healthz`, but the built-ins impose neither.
A messaging-only agent can omit interaction and probes.

Include agent applications, browsers and adapters in the image.

## Definition container settings

`container.image` is required.
`imagePullPolicy`, `command`, `args`, `env`, `envFrom`, `resources`, `runtimeClassName`,
`imagePullSecrets`, `ports` and probe fields use native Kubernetes types.

`env` overrides built-in variables by name; `envFrom` uses Kubernetes precedence.
Model/workspace fields apply afterwards and override their reserved variables.
See [full/definition.yaml](../examples/full/definition.yaml).

Secrets and registry credentials must be in the agent namespace.

## Persistent state and credentials

Set `workspace.volumeClaimTemplate` to a native PVC spec; on the instance or as a
definition default; see the [workspace example](usage.md#model-and-workspace).
Choose capacity, access modes and optionally a StorageClass.

By default, Core creates a private PVC for every Sandbox, including unassigned warm instances.
The PVC belongs to that Sandbox.

Suspend/resume and Pod recreation within the same Sandbox retain workspace data.
Explicit execution replacement (`restartToken`), deletion and expiry remove the Sandbox
and its private PVCs.
Replacement starts with a new private workspace.

For files that must survive replacement, use
`AgentInstance.spec.workspace.existingClaimRef` to mount a namespace-local, externally
managed PVC.
Its UID stays bound across restart.

Existing-PVC allocations enforce zero spares; see
[workspace lifetime](usage.md#lifecycle).

New Groups can separately opt into the [platform persistent workspace preview](usage.md#platform-managed-persistent-workspaces-preview).
That mode creates one Operator-managed PVC outside the Sandbox ownership chain.
It has no warm spares, deletion recovery, automatic GC or revision upgrades in this version.

`workspace` replaces assistant/coding's ephemeral `/workspace` with the PVC mount.
A different mountPath also changes the built-in working directory and HOME, unless
`container.env` overrides HOME.
The application must respect HOME or configure its state paths itself.

Without a PVC, workspace data is lost on Pod replacement.
The mount and PVC template are resolved before allocation; source templates remain
unchanged.

For built-in environments with a writable workspace PVC and an explicit
`security.runAsUser`, the Operator uses that UID as `fsGroup` with
`fsGroupChangePolicy: OnRootMismatch`.
Kubernetes or the CSI driver grants that group access to the volume.

When the UID is omitted, admission and storage configuration determine permissions.
The storage driver must support `fsGroup` volume permissions.

Read-only workspaces and custom SandboxTemplate security settings are unchanged.
Use a custom template to set a separate volume group.

`modelProvider` supplies `OPENAI_BASE_URL`, `AGENTRALLY_MODEL_NAME` and Secret-backed
`OPENAI_API_KEY`; the image must consume or map them.
See the [complete definition](../examples/full/definition.yaml).

Other credentials use `container.env[].valueFrom.secretKeyRef` or `container.envFrom`,
or native container fields in a custom template.

## Container runtime

Set `spec.container.runtimeClassName` to an existing RuntimeClass name.
AgentRally passes it through; it does not map Kata/gVisor aliases.
Omit it for the node default.

An administrator installs the runtime and RuntimeClass.
In a custom template, use `spec.podTemplate.spec.runtimeClassName`.
See
[Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/).

```yaml
spec:
  agentGroupRef:
    name: demo
  template: coding
  container:
    image: registry.example.com/my-coding-agent:v1
    runtimeClassName: your-runtime-class
```

Before allocation, AgentRally checks the RuntimeClass reference and direct selector
conflicts.
Kubernetes evaluates nodes, affinity, tolerations and capacity.
The setting flows into the snapshot, pool and Pod.

For a runtime change published by the Definition, select the new `spec.revision`
and set a new `restartToken` on the instance.
An ordinary restart preserves its accepted Definition revision.

## Browser environment

[assistant](../internal/templates/assistant.yaml) and
[coding](../internal/templates/coding.yaml) mount 512Mi of memory-backed `/dev/shm`.
Include the browser and its libraries in the image and configure the agent's browser
tools.
The mount does not install or expose a browser.
Its memory usage counts toward container memory.

Use a custom template for a different volume layout.
Built-ins cover resident agents.

## Allocation and policy

All instances use SandboxWarmPool and SandboxClaim.
AgentRally creates missing pools from a snapshot of the resolved Pod configuration.
Instances using the same definition revision and matching resolved settings share
capacity within a Group; credentials, workspace and security differences select separate
pools.

New managed pools default to zero spares regardless of storage or restart policy.
Without a Group prewarming policy, existing pools retain their configured replica count.

Spares are unassigned instances.
A zero-spare pool still creates an instance when a instance requests one.
Extensions handles allocation and refill.

A spare using `workspace.volumeClaimTemplate` starts with its own PVC.
Allocation transfers the existing Sandbox and its disk to the instance; refill creates a
new Sandbox with a different PVC.

Storage specifications participate in pool identity; instance-specific PVC names do not.
Matching agents can share a pool without sharing their workspace files.

Prewarming still starts the real application with the configured credentials.
Enable spare instances only when the application can start before allocation:

```yaml
# AgentGroup.spec
prewarming:
  replicas: 1
  maxTotalReplicas: 4
  idleTTLSeconds: 300
```

`replicas` is the default spare count for each resolved configuration, not each instance.
For Definitions with an explicit prewarming policy, it is the per-pool upper limit.
Optional `maxTotalReplicas` caps desired spares across managed configurations.
The Group shares this budget across requested configurations, caps each share at its
request and assigns equal-demand remainders in stable pool-name order.
It reduces old targets before increasing others.
Terminating pools retain their share until foreground cleanup removes them.
New pools start at zero when a total budget is configured.
External pools are excluded, and ResourceQuota remains the hard resource-admission boundary.
Without a Definition prewarming policy, AgentRally discovers configurations from actual
instances, so a first unseen configuration has no ready spares.
The policy owns only generated pool replica counts; an omitted policy preserves native
manual scaling, except existing-PVC configurations always enforce zero spares.
External pools remain unchanged and must declare zero spares when mounting an existing
workspace.

Each spare consumes compute and a separate disk.

### Prepare capacity before the first instance

With Group prewarming enabled, opt a Definition into proactive preparation:

```yaml
# AgentDefinition.spec
prewarming:
  replicas: 1
```

This starts the real application before any AgentInstance exists.
Only enable it after checking that shared Definition credentials and initialization are
safe without an assigned owner.
Applications that connect to a user's channels, consume business queues or run personal
scheduled tasks should keep this disabled unless they have a safe unassigned phase.

Proactive preparation requires a long-running application and a readiness probe on the
selected Agent container.
It permits ephemeral workspace and configuration volumes.
PVC templates, existing PVCs, direct data volumes and Kubernetes service account tokens
are excluded.
The [full Definition reference](../examples/full/definition.yaml) includes the optional prewarming fields.

`Ready=True` means the application configuration is published.
`Prepared=True` means the current, probe-checked spare capacity satisfies the request.
`status.prewarming` reports the requested count, the Group's desired count and the ready
count separately.
Budget limits or preparation failures do not invalidate a published configuration.
These observations are eventually consistent; they do not reserve a warm slot for a
particular caller.

The first matching Instance uses the same configuration snapshot and can adopt a spare.
An Instance workspace override selects a different pool and stays cold with zero spares.
Setting Definition replicas to zero or removing its policy stops proactive preparation.
Scaling the policy does not create an application Revision.

Application updates and Secret/ConfigMap rotation select new capacity.
After the changed inputs are observed, old proactive pools stop refilling and stop
counting toward Prepared.
Existing allocated executions retain their accepted configuration and dependencies.
Changing referenced Secret contents does not itself create an immutable application
Revision; the resolved capacity fingerprint tracks the reference version.

Group allocation pause stops new proactive preparation and drains its desired spares to
zero.
Already submitted upstream work can finish.
Pools that have never used the new Definition policy retain the existing pause behavior.
External pools are never scaled by this policy.

Removing a proactive policy does not turn its old pools back into automatically refilled
legacy capacity.
Unused resources follow the Group's idle retention policy.
An enabled, current Definition request also keeps its pool alive without Instance
references.

Full PVC warm handoff and deletion recovery remain unavailable.
Independent image pre-pulling is not enabled; a cached image is separate from an already
running, prepared Sandbox.

After the last live instance/native allocation reference disappears, the persisted idle
timer retains the pool for `idleTTLSeconds`.
Zero (the default) deletes it immediately.
Reuse clears the timer; Group deletion bypasses it.
Foreground cleanup removes spares and their PVCs before their configuration snapshot.
See the [full Group reference](../examples/full/group.yaml).

Custom SandboxTemplates can declare native `volumeClaimTemplates`; Core provisions each
instance's storage.
Do not set `spec.volumeName` to a fixed PV in a claim template.

External pools must match the complete Pod and storage blueprint.
AgentRally keeps PVC templates on SandboxTemplate and never sends per-Claim templates, which
would force cold allocation upstream.

Custom templates may also reference external PVCs directly.
Those are shared references, not private per-instance disks: their access modes and
application concurrency rules still apply.
Attaching a different PVC after allocation is not supported.

Source templates must set `networkPolicyManagement: Unmanaged`; Group handles
networking.

To use an external pool, first resolve the same configuration through a managed instance
and read its `status.provisioning.templateName`.
Point the external pool's `spec.sandboxTemplateRef.name` to that generated snapshot.
The new instance keeps its `definitionRef` and accepted revision, plus the external
pool's name in `provisioning.warmPoolRef`.
Its Pod, service, storage, credentials and Group must match.

Source or credential changes need a new matching snapshot; the external pool is never
rewritten automatically.

AgentRally verifies snapshot ownership, source, revision marker and full blueprint before
creating the SandboxClaim.
Ordinary unmarked user templates cannot prove creation inputs and are rejected as
`PoolConfigurationUnverified`.
Do not copy a revision marker into a user source template or stamp an already running
Pod.

Source templates and external pools stay unchanged.
Published definition revisions belong to AgentDefinition; resolved allocation snapshots
and pools belong to Group.
An external pool reference also protects its generated snapshot from unused-template
cleanup.
See [pool settings](usage.md#pool-allocation).

Core's headless Service and the optional HTTP access Service are separate.
[full/sandbox-template.yaml](../examples/full/sandbox-template.yaml) shows native Pod settings.
Process identity settings are described in [Usage](usage.md#agent-process-identity).
