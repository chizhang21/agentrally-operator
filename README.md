# AgentRally

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

This walkthrough builds the Operator from source and installs a local Helm chart.

### Prerequisites

- A Kubernetes cluster and permission to manage CRDs and cluster RBAC.
- agent-sandbox **Core and Extensions v1.0.5**, installed using the
  [upstream installation steps](docs/installation.md#install-upstream-agent-sandbox).
- kubectl, Helm 3.17 or later, Docker, Make and Python 3 on your workstation.
- A container registry you can push to and your cluster can pull from.

### 1. Build and package the Operator

Replace `registry.example.com/your-team` with your registry path and log in to that
registry before building.

These commands target Linux amd64, matching the chart's default node selector.

```sh
git clone https://github.com/chizhang21/agentrally-operator.git
cd agentrally-operator

make configure VERSION=0.0.0-dev \
  IMAGE_TAG_BASE=registry.example.com/your-team/agentrally-operator
export DOCKER_DEFAULT_PLATFORM=linux/amd64
make docker-build
make docker-push
make chart-package
```

### 2. Install the Operator

For a private Operator image, prepare
[pull credentials](docs/installation.md#private-images) first and add the documented
`imagePullSecrets` option to this command.

```sh
helm install agentrally-operator ./dist/agentrally-operator-0.0.0-dev.tgz \
  -n agentrally-system --create-namespace --wait --timeout 5m
```

### 3. Deploy an agent

Replace the placeholder image in [definition.yaml](examples/base/definition.yaml) with
your agent image.
It must support the built-in environment's [non-root defaults](docs/agent-templates.md).

Create the Group, wait for its namespace to be ready, then publish the definition and
create the AgentInstance:

```sh
kubectl apply -f examples/base/group.yaml
kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/definition.yaml
kubectl -n agentrally-demo wait agentdefinition/demo-agent --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/agent.yaml
kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=5m
```

For credentials, persistent storage and HTTP access, follow the
[focused examples](examples/README.md).

If the agent does not become ready, inspect its conditions and events:

```sh
kubectl -n agentrally-demo describe agentinstance demo-agent
```

The [diagnostics guide](docs/usage.md#status-and-diagnostics) explains the reported
reasons.

## Lifecycle behavior

- **Suspend and resume:** the Pod is removed and recreated.
  PVC data remains; process memory and ephemeral files do not.
- **Idle lifecycle:** an optional caller-managed [activity Lease](docs/activity.md) enables automatic suspension and wake-up.
- **Configuration updates:** an allocated agent keeps running with its current Pod
  configuration until you set a new `restartToken`.
  Definition edits alone do not move pinned instances.
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
| Reuse configuration, pin revisions and retain workspaces | [Definitions and instances](docs/agent-definitions.md) |
| Deploy, expose and operate agents | [Usage](docs/usage.md) |
| Try credentials, storage and access configurations | [Examples](examples/README.md) |
| Customize environments and pools | [Agent templates](docs/agent-templates.md) |
| Inspect available cluster configuration | [Cluster capabilities](docs/cluster-capabilities.md) |
| Understand resource ownership and reconciliation | [Architecture](docs/architecture.md) |
| Build, test or package changes | [Development](docs/development.md) |
| Understand CI and releases | [CI](docs/ci.md) |
| Enable optional backend integration | [Outbound Connector](docs/outbound-connector.md) |

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
