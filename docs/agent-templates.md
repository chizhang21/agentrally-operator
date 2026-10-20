# Agent environments and templates

Select `AgentClaim.spec.template` and fill `spec.runtime` for a built-in environment.
The Operator embeds three Pod blueprints, then creates a complete SandboxTemplate
snapshot and pool during allocation. You do not need to create a source template.
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
| Workspace | None by default | Ephemeral /workspace, or existing PVC via AgentClaim.workspace | Ephemeral /workspace, or existing PVC via AgentClaim.workspace |
| Temporary storage | emptyDir /tmp | emptyDir /tmp | emptyDir /tmp |
| Browser preparation | None | 512Mi memory-backed /dev/shm | 512Mi memory-backed /dev/shm |
| Container runtime | Node default; optional native runtimeClassName | Node default; optional native runtimeClassName | Node default; optional native runtimeClassName |
| Initial spare replicas | 0; prewarming is opt-in | 0; prewarming is opt-in | 0; prewarming is opt-in |

`runtime.resources` replaces the whole requests/limits block. All built-ins use
Linux nodes, restart Always, a 30-second termination grace period, no API token
automount, non-root execution, dropped capabilities, no privilege escalation and
RuntimeDefault seccomp. No UID is fixed; images and PVC permissions must support
the UID assigned by the cluster.

The image entrypoint starts the application; `runtime.command` and `runtime.args`
can override it. Set ports and probes for your image. `full.yaml` uses 8080 and
`/healthz`, but the built-ins impose neither. A messaging-only agent can omit
interaction and probes. Include agent applications, browsers and adapters in the image.

## Instance settings

`runtime.image` is required. `imagePullPolicy`, `command`, `args`, `env`, `envFrom`,
`resources`, `runtimeClassName`, `imagePullSecrets`, `ports` and probe fields use
native Kubernetes types. `env` overrides built-in variables by name; `envFrom`
uses Kubernetes precedence. Model/workspace fields apply afterwards and override
their reserved variables. See [full.yaml](../examples/full.yaml).
Secrets and registry credentials must be in the agent namespace.

## Persistent state and credentials

Create a workspace PVC after Group is ready; see the
[PVC example](usage.md#model-and-workspace). Choose a StorageClass, capacity and
access mode for your application. Use a PVC per personal agent or coding session.
Deleting the agent retains these external PVCs.

`workspace` replaces assistant/coding's ephemeral `/workspace` with the PVC mount.
A different mountPath also changes the built-in working directory and HOME, unless
`runtime.env` overrides HOME. The application must respect HOME or configure its
state paths itself. Without a PVC, workspace data is lost on Pod replacement.
The mount is added to the template before allocation, without changing the blueprint.

For built-in environments with a writable workspace PVC, the Operator sets
`fsGroup: 1000` and `fsGroupChangePolicy: OnRootMismatch`. Kubernetes or the CSI
driver grants the supplemental group access to the volume; the image's process UID
is unchanged. The storage driver must support `fsGroup` volume permissions.
Namespaces with OpenShift SCC UID/group allocations leave these fields to admission.
Read-only workspaces and custom SandboxTemplate security settings are unchanged.
Use a custom template for other volume permission requirements.

`modelProvider` supplies `OPENAI_BASE_URL`, `ARALLY_MODEL_NAME` and Secret-backed
`OPENAI_API_KEY`; the image must consume or map them. See the
[complete Claim](../examples/full.yaml). Other credentials use `runtime.env[].valueFrom.secretKeyRef`
or `runtime.envFrom`, or native container fields in a custom template.

## Container runtime

Set `spec.runtime.runtimeClassName` to an existing RuntimeClass name. Arally passes
it through; it does not map Kata/gVisor aliases. Omit it for the node default.
An administrator installs the runtime and RuntimeClass. In a custom template, use
`spec.podTemplate.spec.runtimeClassName`. See
[Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/).

```yaml
spec:
  agentGroupRef:
    name: demo
  template: coding
  runtime:
    image: registry.example.com/my-coding-agent:v1
    runtimeClassName: your-runtime-class
```

Before allocation, Arally checks the RuntimeClass reference and direct selector
conflicts. Kubernetes evaluates nodes, affinity, tolerations and capacity. The setting
flows into the snapshot, pool and Pod. Use a new `restartToken` to apply a runtime
change to an allocated agent.

## Browser environment

[assistant](../internal/templates/assistant.yaml) and [coding](../internal/templates/coding.yaml)
mount 512Mi of memory-backed `/dev/shm`. Include the browser and its libraries in
the image and configure the agent's browser tools. The mount does not install or
expose a browser. Its memory usage counts toward container memory.
Use a custom template for a different volume layout. Built-ins cover resident agents.

## Allocation and policy

All Claims use SandboxWarmPool and SandboxClaim. Arally creates missing pools
from a snapshot of the resolved Pod configuration. Matching configurations share
capacity within a Group; credentials, workspace and security differences select
separate pools. New managed pools default to zero spares regardless of storage or
restart policy. Existing pools retain their configured replica count. Spares are
unassigned instances. A zero-spare pool still creates an
instance when a Claim requests one. Extensions handles allocation and refill.

A spare starts the application with the same PVC and credentials. Refill starts
another copy. Storage access mode alone does not make this safe:

| PVC mode | Constraint |
| --- | --- |
| ReadWriteOnce | Multiple Pods can mount on one node, not across nodes. |
| ReadWriteOncePod | Only one Pod can mount. |
| ReadWriteMany | Shared mounts are allowed, but browser profiles/session databases may still conflict. |

See [Kubernetes access modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes).
For a personal agent, keep zero spares and retain its PVC. For stateless agents,
use emptyDir and external state to allow prewarming. For read-only or otherwise
concurrency-safe data, verify the application before increasing spare replicas:

```sh
kubectl -n arally-demo get agentclaim coding-demo -o jsonpath='{.status.provisioning.poolName}{"\n"}'
# Replace POOL_NAME with the returned name after confirming safe concurrent use.
kubectl -n arally-demo patch sandboxwarmpool POOL_NAME --type merge -p '{"spec":{"replicas":1}}'
```

Arally preserves the pool's replica setting. Changing it does not remove storage
restrictions or prevent concurrent application execution.

Attaching a private PVC only after allocation is not implemented. The current pool
has the full Pod configuration, including PVC references; an existing running Pod
cannot acquire another volume through AgentClaim. Native per-Sandbox
`volumeClaimTemplates` are not enabled.

Source templates and external pool templates must set
`networkPolicyManagement: Unmanaged`; Group handles networking. External pools
must match source identity and resolved configuration. Arally validates effective
Pod settings before allocation; implicit upstream DNS changes are rejected before
creating the SandboxClaim.
Source templates and external pools stay unchanged; generated snapshots and pools
belong to Group and are deleted when unused. See [pool settings](usage.md#pool-allocation).

Core's headless Service and the optional HTTP access Service are separate.
[custom-template.yaml](../examples/custom-template.yaml) shows native Pod settings.
[AnyUID](../examples/anyuid.yaml) uses a built-in environment and needs administrator
authorization.
