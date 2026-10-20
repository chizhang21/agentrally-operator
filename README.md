# Arally Operator

English | [简体中文](README.zh-CN.md)

**Arally · Agent Rally**

Deploy your own Agent images on Kubernetes and OpenShift. Arally prepares their
configuration and requests execution from
[agent-sandbox Core and Extensions](https://github.com/kubernetes-sigs/agent-sandbox).
An `AgentClaim` can reference existing Secrets and workspace PVCs. Your Agent
keeps its own application interface and execution logic.

## What it manages

The API group is `arally.io/v1alpha1`.

| Resource | Purpose |
| --- | --- |
| `AgentGroup` | A dedicated namespace, Pod Security settings, optional quota and network policy. |
| `AgentClaim` | Agent configuration, pool allocation, suspend/resume, restart, expiry and optional HTTP access. |
| `ClusterCapabilities` | Read-only runtime, storage and network configuration reports for deployment checks. |

Choose a built-in `generic`, `assistant` or `coding` environment, or reference
your own `SandboxTemplate`. Built-ins provide Pod defaults; supply the Agent
application in your image. Arally creates template snapshots, pools and allocation
Claims; agent-sandbox manages Sandbox allocation and Pods. Secrets and persistent
PVCs remain externally managed.

Changes to an allocated Agent's Pod configuration require an explicit
`restartToken` change. Suspend/resume removes and recreates the Pod: PVC data
remains, while process memory and ephemeral files do not.

## Quick start

Use kubectl, Helm 3.17 or later, Docker, Make and Python 3. Your Kubernetes account
needs CRD and cluster RBAC permissions. Start from a source checkout:

```sh
git clone https://github.com/chizhang21/arally-operator.git
cd arally-operator
```

Install agent-sandbox **Core and Extensions v1.0.5** using the
[installation guide](docs/installation.md#install-upstream-agent-sandbox).
Build and push an Operator image, then package its chart. Replace
`registry.example.com/your-team` with your registry path and log in to that
registry first. Cluster nodes must be able to pull the image. These commands
build Linux amd64 images, matching the chart's default node selector:

```sh
make configure VERSION=0.0.0-dev \
  IMAGE_TAG_BASE=registry.example.com/your-team/arally-operator
DOCKER_DEFAULT_PLATFORM=linux/amd64 make docker-build
make docker-push
make chart-package
```

For a private Operator image, prepare [pull credentials](docs/installation.md#private-images)
before installation and add the documented `imagePullSecrets` setting:

```sh
helm install arally-operator ./dist/arally-operator-0.0.0-dev.tgz \
  -n arally-system --create-namespace --wait --timeout 5m
```

Replace the image in [agent.yaml](examples/base/agent.yaml) with your Agent image,
compatible with the built-in environment's [non-root defaults](docs/agent-templates.md).
Create the Group before its AgentClaim:

```sh
kubectl apply -f examples/base/group.yaml
kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/agent.yaml
kubectl -n arally-demo wait agentclaim/demo-agent --for=condition=Ready --timeout=5m
```

For credentials, persistent storage and HTTP access, see the
[examples](examples/README.md). For pending or failed instances, inspect
`kubectl -n arally-demo describe agentclaim demo-agent` and the
[diagnostic reasons](docs/usage.md#status-and-diagnostics).

## Documentation

- [Installation and upgrades](docs/installation.md) · [Chart values](chart/README.md)
- [Deploying and operating agents](docs/usage.md) · [Examples](examples/README.md)
- [Environments and pools](docs/agent-templates.md) · [Cluster capabilities](docs/cluster-capabilities.md)
- [Architecture](docs/architecture.md) · [Development](docs/development.md) · [CI and publication](docs/ci.md)
- [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md)

The Operator works through kubectl or GitOps. An optional outbound
[Connector](docs/outbound-connector.md) runs inside the Operator process for
backend integration and is disabled by default.

The API is alpha. Network isolation requires a configured NetworkPolicy and an
enforcing CNI. Application authentication belongs to the agent or ingress
infrastructure. See [Deploying and operating agents](docs/usage.md) for access and lifecycle settings.

Licensed under [Apache-2.0](LICENSE). Third-party attribution is in [NOTICE](NOTICE).
