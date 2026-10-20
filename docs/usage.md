# Deploying and operating agents

This guide covers Agent configuration and day-to-day operations after
[installing the controllers](installation.md). Build and test instructions are in
[Development](development.md).

From a local checkout, replace the image in [agent.yaml](../examples/base/agent.yaml),
then create the Group before its AgentClaim:

```sh
kubectl apply -f examples/base/group.yaml
kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/agent.yaml
kubectl -n arally-demo wait agentclaim/demo-agent --for=condition=Ready --timeout=5m
```

## Users and permissions

Administrators install controllers, create Groups, set policies and grant access.
Agent deployers manage Claims in their assigned namespace using the
[editor role and RoleBinding](../examples/rbac/README.md). This role can read
status but cannot write it. Templates, Secrets, PVCs and Pod inspection need
separate permissions. The Operator has its own installation-managed RBAC.

## Agent templates

Choose `spec.template: generic`, `assistant` or `coding` and set `runtime.image`.
For sidecars or other Pod settings, use `templateRef.name` to select a
SandboxTemplate in the same namespace, without `template` or `runtime`.
See [environment settings](agent-templates.md) for defaults and template constraints.

Use [full.yaml](../examples/full.yaml) as the Claim field reference, or start
from a focused [example](../examples/README.md). To inspect the installed schema:

```sh
kubectl explain agentclaim.spec --recursive
```

## Group namespace and security

AgentGroup creates `arally-<group-name>`; read `status.namespace` for the resolved
name. An explicit `spec.namespace` must start with `arally-` and cannot change.
Claims, Secrets, ConfigMaps, PVCs and source templates belong in that namespace.
Arally does not adopt an unrelated existing namespace.

Separate namespaces do not block traffic. For same-Group ingress only, add to
`AgentGroup.spec`:

```yaml
networkPolicy:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
  - from:
    - podSelector: {}
```

This leaves egress open. Add ingress rules for the Gateway/router and monitoring
where needed. NetworkPolicy requires an enforcing CNI. Templates delegate network
policy to the Group. Pod Security defaults to Restricted.

### Legacy Agent images on OpenShift

For root or fixed-UID images, create a Group named `legacy` with
`spec.podSecurity: Baseline`.
After its namespace exists, an administrator prepares a dedicated account:

```sh
oc -n arally-legacy create serviceaccount legacy-agent
oc -n arally-legacy create role legacy-agent-anyuid \
  --verb=use --resource=securitycontextconstraints.security.openshift.io --resource-name=anyuid
oc -n arally-legacy create rolebinding legacy-agent-anyuid \
  --role=legacy-agent-anyuid --serviceaccount=arally-legacy:legacy-agent
oc auth can-i use scc/anyuid \
  --as=system:serviceaccount:arally-legacy:legacy-agent -n arally-legacy
```

The [AnyUID Claim](../examples/anyuid.yaml) selects this account and UID.
Arally does not grant SCC permissions. AnyUID permits UID selection, not privileged
execution or host access; seccomp is left to the cluster/runtime in this mode.
See [OKD SCC documentation](https://docs.okd.io/latest/authentication/managing-security-context-constraints.html).

## Interaction endpoint

Declare the application's named TCP `http` port, or its sole TCP port, in the
runtime or native template. Then add to `AgentClaim.spec`:

```yaml
interaction:
  type: HTTPRoute
```

For OpenShift/OKD, use `type: OpenShiftRoute`. Omit interaction for no managed
endpoint; removing it deletes the route and access Service.

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

The Gateway must allow HTTPRoutes from the Group namespace. Configure DNS and TLS
for the domain; Arally creates neither. If omitted, Arally discovers an eligible
Gateway/listener and uses its wildcard hostname. A unique eligible default Gateway
takes precedence; multiple eligible Gateways/listeners require an explicit choice.
An IP alone cannot supply a domain. HTTPS is preferred within the selected Gateway.

Claims can override port, hostname, scheme and Gateway; see
[full.yaml](../examples/full.yaml). An omitted Gateway namespace means the Claim's
namespace. OpenShiftRoute ignores these Group defaults and defaults to an assigned
hostname with HTTPS edge termination. The backend serves HTTP in either case.
The access Service uses port 80. Allow its traffic through Group NetworkPolicy
and provide application authentication in the agent or ingress infrastructure.

```sh
kubectl -n arally-demo wait agentclaim/demo-agent \
  --for=condition=InteractionReady --timeout=120s
kubectl -n arally-demo get agentclaim demo-agent \
  -o jsonpath='{.status.interaction.url}{"\n"}'
```

InteractionReady checks route acceptance and workload readiness, not end-to-end
connectivity or authentication. Suspension retains the URL but makes it unavailable.

## Environment variables and Secrets

Create Secrets in the Group namespace. Under `AgentClaim.spec.runtime`:

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

Custom templates use the native container's env fields. `envFrom` is also supported.
Arally checks references without publishing values and reads Secrets without
listing, watching or caching them. Periodic reconciliation detects version changes;
use a new restart token to apply them to the process. See the
[ordered HTTP Agent example](../examples/http-api-agent/README.md).

## Model and workspace

`modelProvider` in [full.yaml](../examples/full.yaml) injects `OPENAI_BASE_URL`,
`ARALLY_MODEL_NAME` and Secret-backed `OPENAI_API_KEY`. The application must consume
or map these variables. `agentContainer` selects the target container; the default
is the first container.

For a private workspace, declare a native PVC spec in the Claim. Core provisions
one PVC per Sandbox, including each unassigned warm instance:

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

This also sets `ARALLY_WORKSPACE_PATH`. Omit `storageClassName` to use the cluster
default. Arally carries the spec through the pool template; Core creates and owns
the PVC. `status.volumeClaims` maps logical volume names to their allocated PVCs.
Pending WaitForFirstConsumer claims can bind after Core creates the Pod.
See [workspace permissions and defaults](agent-templates.md#persistent-state-and-credentials).

## Status and diagnostics

```sh
kubectl -n arally-demo get agentclaims
kubectl -n arally-demo describe agentclaim demo-agent
```

Read Conditions, Events and `status.podName`. `Ready=True` requires current-generation
configuration to be applied and the execution to be ready, including a requested
interaction endpoint. `Available` reports current Core/Pod health independently.
A healthy old instance can have `Available=True` and `Ready=False/RestartRequired`
while an update waits for explicit restart. An unverified creation revision cannot
produce `Ready=True`. Neither condition proves application credentials work.

| Ready reason | What to check |
| --- | --- |
| `RestartRequired` | Desired configuration has changed; set a new restart token when replacement is appropriate. The old instance may remain Available. |
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

Five-minute stalls add diagnostics without forcing deletion or restart. Inspect
the named Pod and its events for original errors; Arally does not publish raw
container/Event messages or Secret values. More detail is in
[reconciliation and status](architecture.md#reconciliation-and-status).

## Lifecycle

Suspend, wait for the Pod to disappear, then resume:

```sh
kubectl -n arally-demo patch agentclaim demo-agent --type=merge \
  -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n arally-demo wait agentclaim/demo-agent \
  --for=condition=Suspended --timeout=120s
kubectl -n arally-demo patch agentclaim demo-agent --type=merge \
  -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n arally-demo wait agentclaim/demo-agent \
  --for=condition=Ready --timeout=5m
```

Core removes and recreates the Pod. PVC data remains; process memory and emptyDir
data do not. This is stop/start, not checkpoint/restore.

After changing configuration or rotating credentials, set a new restart token:

```sh
kubectl -n arally-demo patch agentclaim demo-agent --type=merge \
  -p '{"spec":{"restartToken":"restart-1"}}'
```

Change the token for each restart; repeating it does nothing. Configuration edits
alone report RestartRequired and leave the process running. ConfigurationApplied
checks the owned Pod's creation revision; it does not prove application hot reload.
Secret volumes retain Kubernetes update behavior; Arally does not freeze or copy
Secret contents. The pending-update condition describes creation evidence.
Arally replaces execution through its owning Claim. **Replacement gets a fresh
workspace:** the old Sandbox and its PVCs are removed by upstream garbage collection.
Suspend/resume and Pod recreation within the same Sandbox retain its PVCs.

Delete AgentClaims before the Group. Wait for execution cleanup, then inspect
remaining Sandboxes and PVCs before moving or removing data:

```sh
kubectl -n arally-demo delete agentclaim demo-agent --wait=true --timeout=120s
kubectl -n arally-demo get agentclaims,sandboxes,pvc
```

Deleting the Group's Namespace deletes remaining namespaced resources, including
Secrets. Do not use Group deletion as a substitute for data retention planning.

### Execution expiry

The optional `lifecycle` block in [full.yaml](../examples/full.yaml) controls expiry:

| Field | Behavior |
| --- | --- |
| `shutdownTime` | Absolute deadline, including time waiting for dependencies or allocation. Also applies to failed and suspended agents. |
| `ttlSecondsAfterFinished` | Starts when Core confirms that the current owned Pod completed naturally. Suspension does not start this TTL. |
| `retainFailed` | True by default; false allows completion TTL to remove failed executions. |
| `shutdownPolicy: Retain` | Removes execution and interaction resources, keeps the Claim with `Expired=True`. |
| `shutdownPolicy: Delete` | Deletes the Claim through its finalizer. |

Sandbox-owned workspace PVCs are removed with execution; externally supplied
Secrets remain. `Retain` retains the AgentClaim and diagnostics, not its workspace.
To run an expired retained Claim again with a fresh workspace, change
`restartToken` and move any elapsed `shutdownTime` into the future or remove it.
Omit lifecycle for no configured expiry.

## Pool allocation

All Claims use SandboxWarmPool and SandboxClaim. Arally selects or creates a
matching pool; Extensions allocates an instance and refills capacity. New managed
pools default to zero spares; prewarming is explicit. Existing pool replica settings
are preserved. See [pool policy](agent-templates.md#allocation-and-policy).

The optional provisioning block selects a pool or changes the allocation budget:

```yaml
provisioning:
  claimTimeoutSeconds: 60
  # warmPoolRef:
  #   name: tool-environment
```

The default 60-second budget starts at persisted `status.provisioning.startedAt`
and covers capacity/Claim creation until an owned, matching Sandbox is observed.
`completedAt` ends that budget independently of Core/Pod readiness. Startup failures
or delays are reported without deleting the allocated execution; use
`lifecycle.shutdownTime` for an overall cleanup deadline. A timeout while still
waiting for valid allocation removes the native Claim; change restartToken to retry.
Existing external pools must match the source template and configuration.
Configuration edits do not switch an active allocation. See
[resource ownership and cleanup](architecture.md#mandatory-upstream-pools).
