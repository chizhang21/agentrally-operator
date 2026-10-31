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

Optional HTTP access, readiness status and lifecycle metrics support day-to-day
operation.

Select runtime isolation through Kubernetes RuntimeClass, including Kata Containers VMs
and gVisor sandboxes.

AgentRally builds on
[agent-sandbox Core and Extensions](https://github.com/kubernetes-sigs/agent-sandbox)
for Sandbox allocation and Pod management.
Each agent keeps its own application logic and API.

Basic CR configuration and full field references are documented in
[examples](examples/README.md).
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

Install a published Helm chart, then create your first agent.

### Prerequisites

- A Kubernetes cluster and permission to manage CRDs and cluster RBAC.
- agent-sandbox **Core and Extensions v1.0.5**, installed using the
  [upstream installation steps](docs/installation.md#install-upstream-agent-sandbox).
- kubectl, Helm 3.17 or later, and Git for the examples below.

### 1. Install the Operator

Replace `x.y.z` with the published Chart version you want to install.

```sh
CHART_VERSION=x.y.z
helm install agentrally-operator oci://ghcr.io/chizhang21/helm/agentrally-operator \
  --version "$CHART_VERSION" \
  -n agentrally-system --create-namespace --wait --timeout 5m
```

For private registry credentials and custom values, see [Installation](docs/installation.md).
To build from source, see [Development](docs/development.md#package-versions).

### 2. Deploy an agent

```sh
git clone https://github.com/chizhang21/agentrally-operator.git
cd agentrally-operator
```

From a local Operator checkout, replace the placeholder image in
[definition.yaml](examples/basic/definition.yaml) with your agent image.
It must support the built-in environment's [non-root defaults](docs/agent-templates.md).
Run the following commands from the repository root.

Create the Group, wait for its namespace to be ready, then publish the definition and
create the AgentInstance:

```sh
kubectl apply -f examples/basic/group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/basic/definition.yaml -f examples/basic/instance.yaml
```

For credentials, persistent storage and HTTP access, follow the
[full CR references](examples/README.md).

If the agent does not become ready, inspect its conditions and events:

```sh
kubectl -n agentrally-demo describe agentinstance demo-agent
```

The [diagnostics guide](docs/usage.md#status-and-diagnostics) explains the reported
reasons.

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
| Configure the Helm release | [Chart configuration](chart/README.md) |
| Pin revisions and manage workspace lifecycles | [Lifecycle](docs/usage.md#lifecycle) |
| Deploy, expose and operate agents | [Usage](docs/usage.md) |
| Inspect basic and full CR configuration | [Examples](examples/README.md) |
| Customize environments and pools | [Agent templates](docs/agent-templates.md) |
| Inspect available cluster configuration | [Cluster capabilities](docs/cluster-capabilities.md) |
| Understand resource ownership and reconciliation | [Architecture](docs/architecture.md) |
| Build, test or package changes | [Development](docs/development.md) |
| Inspect CI and release workflows | [GitHub Workflows](.github/workflows) |
| Enable optional backend integration | [Helm values example](chart/connector-values.example.yaml) · [Connector protocol](docs/outbound-connector.md) |

## Project status

The API is **alpha**.

`ClusterCapabilities` provides read-only configuration reports for deployment planning.

Network isolation requires a configured NetworkPolicy and an enforcing CNI.

Application authentication belongs to the agent or ingress infrastructure.
See [Usage](docs/usage.md) for configuration details.

[Contributing](CONTRIBUTING.md) · [Security policy](SECURITY.md)

## License

Licensed under [Apache-2.0](LICENSE).
Third-party attribution is in [NOTICE](NOTICE).
