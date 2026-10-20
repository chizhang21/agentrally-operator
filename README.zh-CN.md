# Arally Operator

[English](README.md) | 简体中文

**Arally · Agent Rally**

在 Kubernetes 和 OpenShift 上运行你的 Agent。通过 `AgentClaim` 提供镜像、配置和
工作空间，Arally 准备运行环境并向
[agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) 申请实例。
Agent 保留自己的应用接口和执行逻辑。

## 管理哪些资源

API group 为 `arally.io/v1alpha1`。

| 资源 | 用途 |
| --- | --- |
| `AgentGroup` | 专用命名空间、Pod Security 设置，以及可选的配额和网络策略。 |
| `AgentClaim` | Agent 配置、池分配、挂起恢复、重启、到期回收和可选的 HTTP 入口。 |
| `ClusterCapabilities` | 发现运行时、存储和网络配置，供部署检查使用。 |

可选择内置的 `generic`、`assistant`、`coding` 环境，或引用自己的 `SandboxTemplate`。
Agent 镜像、Secret 和持久化 PVC 单独提供。Arally 创建模板快照、池和分配申请；
agent-sandbox 负责 Sandbox 分配和 Pod 管理。

## 快速开始

按[安装指南](docs/installation.md#install-upstream-agent-sandbox)安装 agent-sandbox
**Core 和 Extensions**。然后使用 Helm 3.17 或更高版本安装 Arally，
账号需要具备管理 CRD 和集群 RBAC 的权限：

```sh
helm install arally-operator oci://ghcr.io/chizhang21/helm/arally-operator \
  -n arally-system --create-namespace --wait --timeout 5m
```

在本地检出的仓库中，替换 [agent.yaml](examples/base/agent.yaml) 的镜像后应用示例：

```sh
kubectl apply -f examples/base/group.yaml
kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/agent.yaml
kubectl -n arally-demo wait agentclaim/demo-agent --for=condition=Ready --timeout=5m
```

凭据、持久存储和 HTTP 入口配置见[示例](examples/README.md)。实例未就绪或失败时，
运行 `kubectl -n arally-demo describe agentclaim demo-agent`，结合
[诊断说明](docs/usage.md#status-and-diagnostics)排查。

## 文档

以下详细文档目前为英文：

- [安装与升级](docs/installation.md) · [Chart 配置](chart/README.md)
- [部署与运维](docs/usage.md) · [示例](examples/README.md)
- [环境与池](docs/agent-templates.md) · [集群能力](docs/cluster-capabilities.md)
- [架构](docs/architecture.md) · [开发](docs/development.md)
- [贡献指南](CONTRIBUTING.md) · [变更记录](CHANGELOG.md) · [安全](SECURITY.md)

Operator 可以通过 kubectl 或 GitOps 独立使用。可选的出站
[Connector](docs/outbound-connector.md) 为后续 Orchestrator 集成保留，
Orchestrator 目前尚未就绪。

当前 API 为 alpha。网络隔离需要配置 NetworkPolicy，并由支持它的 CNI 执行。
应用认证由 Agent 或入口设施提供。权限和生命周期设置见[部署与运维](docs/usage.md)。

项目使用 [Apache-2.0](LICENSE) 许可证，第三方声明见 [NOTICE](NOTICE)。
