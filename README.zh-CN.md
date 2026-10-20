# Arally Operator

[English](README.md) | 简体中文

**Arally · Agent Rally**

在 Kubernetes 和 OpenShift 上部署你自己的 Agent 镜像。Arally 准备运行配置，并向
[agent-sandbox Core 和 Extensions](https://github.com/kubernetes-sigs/agent-sandbox)
申请执行实例。`AgentClaim` 可以引用已有的 Secret 和工作空间 PVC。
Agent 保留自己的应用接口和执行逻辑。

## 管理哪些资源

API group 为 `arally.io/v1alpha1`。

| 资源 | 用途 |
| --- | --- |
| `AgentGroup` | 专用命名空间、Pod Security 设置，以及可选的配额和网络策略。 |
| `AgentClaim` | Agent 配置、池分配、挂起恢复、重启、到期回收和可选的 HTTP 入口。 |
| `ClusterCapabilities` | 只读发现运行时、存储和网络配置，供部署检查参考。 |

可选择内置的 `generic`、`assistant`、`coding` 环境，或引用自己的 `SandboxTemplate`。
内置环境提供 Pod 配置默认值，Agent 应用由你的镜像提供。Arally 创建模板快照、池和
分配申请；agent-sandbox 负责 Sandbox 分配和 Pod 管理。Secret 和持久化 PVC 由外部管理。

已分配实例的 Pod 配置变更需要显式修改 `restartToken` 才会生效。暂停和恢复会删除并
重建 Pod：PVC 数据保留，进程内存和临时文件不会保留。

## 快速开始

需要 kubectl、Helm 3.17 或更高版本、Docker、Make 和 Python 3。
Kubernetes 账号需要具备管理 CRD 和集群 RBAC 的权限。先获取源码：

```sh
git clone https://github.com/chizhang21/arally-operator.git
cd arally-operator
```

按[安装指南](docs/installation.md#install-upstream-agent-sandbox)安装 agent-sandbox
**Core 和 Extensions v1.0.5**。然后构建、上传 Operator 镜像并打包 Chart。
将 `registry.example.com/your-team` 替换为你的镜像仓库路径，提前登录该仓库，
并确保集群节点能够拉取镜像。以下命令构建 Linux amd64 镜像，与 Chart 默认节点选择器一致：

```sh
make configure VERSION=0.0.0-dev \
  IMAGE_TAG_BASE=registry.example.com/your-team/arally-operator
DOCKER_DEFAULT_PLATFORM=linux/amd64 make docker-build
make docker-push
make chart-package
```

使用私有 Operator 镜像时，先准备[拉取凭据](docs/installation.md#private-images)，
并按说明添加 `imagePullSecrets` 设置：

```sh
helm install arally-operator ./dist/arally-operator-0.0.0-dev.tgz \
  -n arally-system --create-namespace --wait --timeout 5m
```

将 [agent.yaml](examples/base/agent.yaml) 的镜像替换为你的 Agent 镜像，确保它兼容
内置环境的[非 root 默认配置](docs/agent-templates.md)。先创建 Group，再创建 AgentClaim：

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
- [架构](docs/architecture.md) · [开发](docs/development.md) · [CI 与发布](docs/ci.md)
- [贡献指南](CONTRIBUTING.md) · [变更记录](CHANGELOG.md) · [安全](SECURITY.md)

Operator 通过 kubectl 或 GitOps 使用。可选的出站
[Connector](docs/outbound-connector.md) 在 Operator 进程中运行，用于后端集成，默认关闭。

当前 API 为 alpha。网络隔离需要配置 NetworkPolicy，并由支持它的 CNI 执行。
应用认证由 Agent 或入口设施提供。权限和生命周期设置见[部署与运维](docs/usage.md)。

项目使用 [Apache-2.0](LICENSE) 许可证，第三方声明见 [NOTICE](NOTICE)。
