# Arally Operator

English | [简体中文](README.zh-CN.md)

**Deploy and manage containerized agents on Kubernetes and OpenShift.**

Describe your agent's image, configuration and lifecycle in Kubernetes resources.
Manage it through `kubectl` or GitOps.

Arally prepares the configuration and requests execution through
[agent-sandbox Core and Extensions](https://github.com/kubernetes-sigs/agent-sandbox),
which handle Sandbox allocation and Pod management. Each agent keeps its own
application logic and API.

## What you can do

- **Run your own agent images.** Choose a `generic`, `assistant` or `coding`
  environment, or bring a custom `SandboxTemplate`. Built-in environments provide
  Pod defaults; you supply the agent application.
- **Connect credentials and storage.** Reference existing Secrets and ConfigMaps,
  and provision a private workspace PVC for each Sandbox, including warm instances.
- **Control the lifecycle.** Suspend, resume or explicitly restart an agent.
  Configure shutdown deadlines and cleanup after completion.
- **Organize agents into groups.** Give each group a dedicated namespace with
  Pod Security settings and optional resource quotas and network policies.
- **Expose and inspect agents.** Configure optional HTTP access through Gateway
  API or OpenShift Routes, and inspect readiness and diagnostic conditions.

## How it works

Arally exposes three resources in `arally.io/v1alpha1`:

| Resource | Scope | Responsibility |
| --- | --- | --- |
| `AgentGroup` | Cluster | Prepares a group's namespace and policies. |
| `AgentClaim` | Namespaced | Declares one agent's configuration, allocation and lifecycle. |
| `ClusterCapabilities` | Cluster | Reports runtime, storage and network configuration for deployment checks. |

Create an `AgentGroup` first, then create AgentClaims in its namespace. For
example, the [base Group](examples/base/group.yaml) prepares `arally-demo`, and
this [AgentClaim](examples/base/agent.yaml) requests an agent there:

```yaml
apiVersion: arally.io/v1alpha1
kind: AgentClaim
metadata:
  name: demo-agent
  namespace: arally-demo
spec:
  agentGroupRef:
    name: demo
  template: generic
  runtime:
    image: registry.example.com/my-agent:v1
```

Arally manages configuration, template snapshots, pools and allocation requests.
The required agent-sandbox controllers allocate Sandboxes and manage their Pods.
See [Architecture](docs/architecture.md) for the resource relationships.

## Quick start

This walkthrough builds the Operator from source and installs a local Helm chart.

### Prerequisites

- A Kubernetes or OpenShift cluster and permission to manage CRDs and cluster RBAC.
- agent-sandbox **Core and Extensions v1.0.5**, installed using the
  [upstream installation steps](docs/installation.md#install-upstream-agent-sandbox).
- kubectl, Helm 3.17 or later, Docker, Make and Python 3 on your workstation.
- A container registry you can push to and your cluster can pull from.

### 1. Build and package the Operator

Replace `registry.example.com/your-team` with your registry path and log in to
that registry before building. These commands target Linux amd64, matching the
chart's default node selector.

```sh
git clone https://github.com/chizhang21/arally-operator.git
cd arally-operator

make configure VERSION=0.0.0-dev \
  IMAGE_TAG_BASE=registry.example.com/your-team/arally-operator
DOCKER_DEFAULT_PLATFORM=linux/amd64 make docker-build
make docker-push
make chart-package
```

### 2. Install the Operator

For a private Operator image, prepare
[pull credentials](docs/installation.md#private-images) first and add the
documented `imagePullSecrets` option to this command.

```sh
helm install arally-operator ./dist/arally-operator-0.0.0-dev.tgz \
  -n arally-system --create-namespace --wait --timeout 5m
```

### 3. Deploy an agent

Replace the placeholder image in [agent.yaml](examples/base/agent.yaml) with your
agent image. It must support the built-in environment's
[non-root defaults](docs/agent-templates.md).

Create the Group, wait for its namespace to be ready, then create the AgentClaim:

```sh
kubectl apply -f examples/base/group.yaml
kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/agent.yaml
kubectl -n arally-demo wait agentclaim/demo-agent --for=condition=Ready --timeout=5m
```

For credentials, persistent storage and HTTP access, follow the
[focused examples](examples/README.md).

If the agent does not become ready, inspect its conditions and events:

```sh
kubectl -n arally-demo describe agentclaim demo-agent
```

The [diagnostics guide](docs/usage.md#status-and-diagnostics) explains the reported reasons.

## Lifecycle behavior

- **Suspend and resume:** the Pod is removed and recreated. PVC data remains;
  process memory and ephemeral files do not.
- **Configuration updates:** an allocated agent keeps running with its current
  Pod configuration until you set a new `restartToken`. Replacement starts with a
  fresh workspace and removes the previous Sandbox's PVCs.
- **Expiry:** optional shutdown deadlines and completion TTLs control when
  execution is cleaned up and whether the AgentClaim is retained.

See [Lifecycle](docs/usage.md#lifecycle) for commands and retention settings.

## Documentation

| I want to… | Read |
| --- | --- |
| Install or upgrade the Operator | [Installation](docs/installation.md) |
| Configure the Helm release | [Chart configuration](chart/README.md) |
| Deploy, expose and operate agents | [Usage](docs/usage.md) |
| Try credentials, storage and access configurations | [Examples](examples/README.md) |
| Customize environments and pools | [Agent templates](docs/agent-templates.md) |
| Inspect available cluster configuration | [Cluster capabilities](docs/cluster-capabilities.md) |
| Understand resource ownership and reconciliation | [Architecture](docs/architecture.md) |
| Build, test or package changes | [Development](docs/development.md) |
| Understand CI and image publication | [CI](docs/ci.md) |
| Enable optional backend integration | [Outbound Connector](docs/outbound-connector.md) |

## Project status

The API is **alpha**. `ClusterCapabilities` provides read-only configuration
reports for deployment planning.

Network isolation requires a configured NetworkPolicy and an enforcing CNI.
Application authentication belongs to the agent or ingress infrastructure.
See [Usage](docs/usage.md) for configuration details.

[Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security policy](SECURITY.md)

## License

Licensed under [Apache-2.0](LICENSE). Third-party attribution is in [NOTICE](NOTICE).
