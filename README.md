# Arally Operator

English | [简体中文](README.zh-CN.md)

**Arally · Agent Rally**

Run your agents on Kubernetes and OpenShift. Supply an image, configuration and
workspace through an `AgentClaim`; Arally prepares the environment and requests
an instance from [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox).
Your agent keeps its own application interface and execution logic.

## What it manages

The API group is `arally.io/v1alpha1`.

| Resource | Purpose |
| --- | --- |
| `AgentGroup` | A dedicated namespace, Pod Security settings, optional quota and network policy. |
| `AgentClaim` | Agent configuration, pool allocation, suspend/resume, restart, expiry and optional HTTP access. |
| `ClusterCapabilities` | Runtime, storage and network discovery for deployment checks. |

Choose a built-in `generic`, `assistant` or `coding` environment, or reference
your own `SandboxTemplate`. Agent images, Secrets and persistent PVCs are supplied
separately. Arally creates template snapshots, pools and allocation Claims;
agent-sandbox manages Sandbox allocation and Pods.

## Quick start

Install agent-sandbox **Core and Extensions** following the
[installation guide](docs/installation.md#install-upstream-agent-sandbox).
Then install Arally with Helm 3.17 or later, using an account with CRD and cluster
RBAC permissions:

```sh
helm install arally-operator oci://ghcr.io/chizhang21/helm/arally-operator \
  -n arally-system --create-namespace --wait --timeout 5m
```

From a local checkout, replace the image in [agent.yaml](examples/base/agent.yaml),
then apply the example:

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
- [Architecture](docs/architecture.md) · [Development](docs/development.md)
- [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) · [Security](SECURITY.md)

The Operator runs independently through kubectl or GitOps. An optional outbound
[Connector](docs/outbound-connector.md) is retained for future Orchestrator
integration; the Orchestrator is not yet available.

The API is alpha. Network isolation requires a configured NetworkPolicy and an
enforcing CNI. Application authentication belongs to the agent or ingress
infrastructure. See [Deploying and operating agents](docs/usage.md) for access and lifecycle settings.

Licensed under [Apache-2.0](LICENSE). Third-party attribution is in [NOTICE](NOTICE).
