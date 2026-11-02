# AgentRally Operator

English | [简体中文](README.zh-CN.md)

**AgentRally is a Kubernetes operator for isolated AI agent runtimes.**

Run your own agent images and manage their configuration and lifecycle through `kubectl`
or GitOps.
Choose a built-in environment or a custom `SandboxTemplate`, and reference existing
Secrets and ConfigMaps.

Organize agents into groups with namespace policies and resource quotas.
Prewarm capacity.

Suspend or resume execution.
Retain workspace data across upgrades with external PVCs.

Readiness status, Kubernetes Events and resource metrics support day-to-day operation.
Optional HTTP access exposes agent services.

Select runtime isolation through Kubernetes RuntimeClass, including Kata Containers VMs
and gVisor sandboxes.

AgentRally builds on
[agent-sandbox Core and Extensions](https://github.com/kubernetes-sigs/agent-sandbox)
for Sandbox allocation and Pod management.
Each agent keeps its own application logic and API.

Basic CR configuration and full field references are documented in
[examples](examples).
Complete Agent deployments live under `operator/` in the separate
[agentrally-examples](https://github.com/chizhang21/agentrally-examples) repository.

## Runtime isolation

The selected RuntimeClass determines how an agent's Pod is isolated:

| Runtime | Isolation mechanism |
| --- | --- |
| Node default | Determined by the node's runtime configuration. |
| [Kata Containers](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md) | Hardware virtualization with a guest kernel per Pod sandbox. |
| [gVisor](https://gvisor.dev/docs/) | A per-sandbox application kernel running in userspace. |

Kata provides lightweight VM isolation.
Its VM or microVM configuration and workload compatibility depend on the installed
[hypervisor/VMM](https://github.com/kata-containers/kata-containers/blob/main/docs/design/virtualization.md).

Choose an existing RuntimeClass in your `AgentDefinition`:

```yaml
spec:
  container:
    runtimeClassName: kata # Replace with your cluster's actual RuntimeClass name.
```

List available names with `kubectl get runtimeclasses`.
Omitting the field uses the node default.

Cluster administrators install the runtime, configure its node CRI handler and create
the RuntimeClass.
AgentRally carries this selection through agent-sandbox to the execution Pod.

See [runtime configuration](docs/agent-templates.md#container-runtime) for custom
templates and applying runtime changes to existing agents.

## Quick start

This walkthrough installs AgentRally `0.0.3`, starts a small HTTP Agent and exercises
suspend/resume before cleaning up.

### Prerequisites

- A Kubernetes cluster with a Linux amd64 node and permission to manage CRDs and cluster RBAC.
- kubectl, Helm 3.17 or later, Git and curl.

### 1. Install the controllers

Install agent-sandbox Core and Extensions, then the Operator from GHCR:

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.5/sandbox-with-extensions.yaml && kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
CHART_VERSION=0.0.3
helm install agentrally-operator oci://ghcr.io/chizhang21/helm/agentrally-operator \
  --version "$CHART_VERSION" \
  -n agentrally-system --create-namespace --wait --timeout 5m
```

The Chart uses `ghcr.io/chizhang21/agentrally-operator:v0.0.3`.
For custom values and private credentials, see [Installation](docs/installation.md).
To build from source, see [Development](docs/development.md#package-versions).

### 2. Start an Agent

Use examples from the matching source tag:

```sh
git clone --branch v0.0.3 --depth 1 https://github.com/chizhang21/agentrally-operator.git
cd agentrally-operator
kubectl apply -f examples/02-group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/03-definition.yaml -f examples/04-instance-basic.yaml && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=300s
```

The [Definition](examples/03-definition.yaml) uses the public
`ghcr.io/chizhang21/agentrally-demo-agent:v0.0.3` image.
It runs without model credentials, a Gateway, persistent storage or an additional RuntimeClass.
Optional configuration remains commented in the shared Group and Definition references.

If the Agent does not become ready, inspect its conditions and events:

```sh
kubectl -n agentrally-demo describe agentinstance demo-agent
```

See [diagnostics](docs/usage.md#status-and-diagnostics) for the reported reasons.

### 3. Call the Agent

Read its current Pod name and start a local port-forward:

```sh
POD=$(kubectl -n agentrally-demo get agentinstance demo-agent -o jsonpath='{.status.podName}')
kubectl -n agentrally-demo port-forward "pod/$POD" 18080:8080
```

Keep that terminal open.
In another terminal, check health and send a message:

```sh
curl -fsS http://127.0.0.1:18080/healthz
curl -fsS http://127.0.0.1:18080/chat -H 'Content-Type: application/json' --data '{"message":"Hello"}'
```

The chat response is:

```json
{"response":"AgentRally demo agent is ready.","mode":"demo"}
```

Demo mode uses Pydantic AI's `TestModel` for a fixed response and makes no model calls.
The [Agent project](https://github.com/chizhang21/agentrally-examples/tree/v0.0.3/operator/http-api-agent)
contains its source and instructions for connecting a real model with Secret-backed credentials.

### 4. Suspend and resume

```sh
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Suspended --timeout=120s
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":null}}' && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=300s
```

Suspension removes the Pod; resuming creates a new one.
Read the new Pod name and repeat the port-forward and calls above.
For persistent storage or a managed HTTPRoute, adapt [04-instance-full.yaml](examples/04-instance-full.yaml)
after preparing its StorageClass and Gateway dependencies.

### 5. Clean up

Delete the Instance, its Definition and the Group while the Operator is running:

```sh
kubectl delete -f examples/04-instance-basic.yaml --timeout=300s
kubectl delete -f examples/03-definition.yaml --timeout=120s
kubectl delete -f examples/02-group.yaml --timeout=300s
```

To uninstall the Operator afterwards:

```sh
helm uninstall agentrally-operator -n agentrally-system
```

Helm retains the CRDs.

## Lifecycle behavior

- **Suspend and resume:** the Pod is removed and recreated.
  PVC data remains; process memory and ephemeral files do not.
- **Configuration updates:** an allocated agent keeps running with its current Pod
  configuration until you set a new `restartToken`.
  A restart preserves its accepted definition revision.
  To upgrade, select the target `spec.revision` and set a new token.
- **Workspace retention:** private PVCs are replaced with the Sandbox;
  external workspace PVCs retain their data.
- **Expiry:** optional maximum lifetimes, shutdown deadlines and completion TTLs trigger execution cleanup.
  The shutdown policy controls whether to retain the AgentInstance.

See [Lifecycle](docs/usage.md#lifecycle) for commands and retention settings.

## Documentation

| I want to… | Read |
| --- | --- |
| Install or upgrade the Operator | [Installation](docs/installation.md) |
| Configure the Helm release | [Release configuration](docs/installation.md#release-configuration) · [Values examples](examples/values) |
| Pin revisions and manage workspace lifecycles | [Lifecycle](docs/usage.md#lifecycle) |
| Deploy, expose and operate agents | [Usage](docs/usage.md) |
| Inspect basic and full CR configuration | [Examples](examples) |
| Customize environments and pools | [Agent templates](docs/agent-templates.md) |
| Inspect available cluster configuration | [Cluster capabilities](docs/cluster-capabilities.md) |
| Understand resource ownership and reconciliation | [Architecture](docs/architecture.md) |
| Build, test or package changes | [Development](docs/development.md) |
| Inspect CI and release workflows | [GitHub Workflows](.github/workflows) |
| Enable optional backend integration | [Orchestrator values](examples/values/orchestrator.yaml) · [Connector protocol](docs/outbound-connector.md) |

## Project status

The API is **alpha**.

`ClusterCapabilities` provides read-only configuration reports for deployment planning.

Network isolation requires a configured NetworkPolicy and an enforcing CNI.

Application authentication belongs to the agent or ingress infrastructure.
See [Usage](docs/usage.md) for configuration details.

## License

Licensed under [Apache-2.0](LICENSE).
Third-party attribution is in [NOTICE](NOTICE).
