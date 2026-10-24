# AgentRally Operator

[English](README.md) | 简体中文

**在 Kubernetes 上部署、管理隔离的 AI Agent 运行环境。**

运行自己的 Agent 镜像，通过 `kubectl` 或 GitOps 管理配置和生命周期。
选择内置环境或自定义 `SandboxTemplate`，引用已有的 Secret 和 ConfigMap。

按组管理 Agent，为各组配置命名空间策略和资源配额。
支持容量预热。

暂停或恢复执行。
通过外部 PVC 在升级时保留工作区数据。

可选的 HTTP 入口、就绪状态和生命周期指标支持日常运维。

通过 Kubernetes RuntimeClass 选择运行时隔离方式，包括 Kata Containers 虚拟机和 gVisor 沙箱。

AgentRally 基于 [agent-sandbox Core 和 Extensions](https://github.com/kubernetes-sigs/agent-sandbox)
完成 Sandbox 分配和 Pod 管理。
每个 Agent 保留自己的应用逻辑和 API。

## 运行时隔离

选用的 RuntimeClass 决定 Agent 所在 Pod 的隔离机制：

| 运行时 | 隔离机制 |
| --- | --- |
| 节点默认运行时 | 由节点的运行时配置决定。 |
| [Kata Containers](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md) | 基于硬件虚拟化，每个 Pod 沙箱使用独立的客户机内核。 |
| [gVisor](https://gvisor.dev/docs/) | 在用户态运行，每个沙箱拥有独立的应用内核。 |

Kata 提供轻量虚拟机隔离。
具体的 VM 或 microVM 配置及工作负载兼容性，取决于集群安装的
[Hypervisor/VMM](https://github.com/kata-containers/kata-containers/blob/main/docs/design/virtualization.md)。

在 `AgentDefinition` 中选择已有的 RuntimeClass：

```yaml
spec:
  container:
    runtimeClassName: kata # 替换为集群中实际存在的 RuntimeClass 名称。
```

使用 `kubectl get runtimeclasses` 查看可用名称。
省略该字段时使用节点默认运行时。

集群管理员负责安装运行时、配置节点 CRI handler 并创建 RuntimeClass。
AgentRally 通过 agent-sandbox 将该选择传递到执行 Pod。

自定义模板的配置方式，以及如何将运行时变更应用到已有 Agent，
见[运行时配置](docs/agent-templates.md#container-runtime)。

## 快速开始

以下步骤从源码构建 Operator，并安装本地打包的 Helm Chart。

### 前提条件

- Kubernetes 集群，以及管理 CRD 和集群 RBAC 的权限。
- 已安装 agent-sandbox **Core 和 Extensions v1.0.5**，步骤见
  [上游安装说明](docs/installation.md#install-upstream-agent-sandbox)。
- 本机安装 kubectl、Helm 3.17 或更高版本、Docker、Make 和 Python 3。
- 一个你能推送镜像、集群能拉取镜像的容器镜像仓库。

### 1. 构建并打包 Operator

将 `registry.example.com/your-team` 替换为你的镜像仓库路径，并提前登录该仓库。

以下命令构建 Linux amd64 镜像，与 Chart 默认节点选择器一致。

```sh
git clone https://github.com/chizhang21/arally-operator.git
cd arally-operator

make configure VERSION=0.0.0-dev \
  IMAGE_TAG_BASE=registry.example.com/your-team/arally-operator
DOCKER_DEFAULT_PLATFORM=linux/amd64 make docker-build
make docker-push
make chart-package
```

### 2. 安装 Operator

使用私有 Operator 镜像时，先准备[拉取凭据](docs/installation.md#private-images)，
并按说明为以下命令添加 `imagePullSecrets` 参数。

```sh
helm install arally-operator ./dist/arally-operator-0.0.0-dev.tgz \
  -n arally-system --create-namespace --wait --timeout 5m
```

### 3. 部署 Agent

将 [definition.yaml](examples/base/definition.yaml) 中的占位镜像替换为你的 Agent 镜像，
确保它兼容内置环境的[非 root 默认配置](docs/agent-templates.md)。

先创建 Group，等待其命名空间就绪，再发布 Definition 和创建 AgentInstance：

```sh
kubectl apply -f examples/base/group.yaml
kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/definition.yaml
kubectl -n arally-demo wait agentdefinition/demo-agent --for=condition=Ready --timeout=120s
kubectl apply -f examples/base/agent.yaml
kubectl -n arally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=5m
```

凭据、持久存储和 HTTP 入口配置见[专题示例](examples/README.md)。

如果 Agent 未就绪，先查看它的条件和事件：

```sh
kubectl -n arally-demo describe agentinstance demo-agent
```

各诊断原因的含义见[诊断说明](docs/usage.md#status-and-diagnostics)。

## 生命周期行为

- **暂停和恢复：** 删除并重建 Pod。
  PVC 数据保留，进程内存和临时文件不会保留。
- **空闲管理：** 通过调用方维护的可选[活动 Lease](docs/activity.md)，自动暂停和唤醒执行。
- **配置更新：** 已分配的 Agent 继续使用当前 Pod 配置，直到你设置新的 `restartToken`。
  定义更新不会自动改变已固定的实例版本。
- **工作区保留：** 私有 PVC 随 Sandbox 替换，引用的外部工作区 PVC 则保留数据。
- **到期回收：** 通过可选的最长执行时间、关闭期限和完成后 TTL，控制何时回收执行资源。
  关闭策略决定是否保留 AgentInstance。

操作命令和保留策略见[生命周期说明](docs/usage.md#lifecycle)。

## 文档

以下详细文档目前为英文：

| 我想…… | 阅读 |
| --- | --- |
| 安装或升级 Operator | [安装指南](docs/installation.md) |
| 配置 Helm Release | [Chart 配置](chart/README.md) |
| 复用配置、固定版本和保留工作区 | [定义与实例](docs/agent-definitions.md) |
| 部署 Agent、配置入口和日常运维 | [使用指南](docs/usage.md) |
| 尝试凭据、存储和访问配置 | [示例](examples/README.md) |
| 定制运行环境和池 | [Agent 模板](docs/agent-templates.md) |
| 查看集群现有配置 | [集群能力](docs/cluster-capabilities.md) |
| 了解资源归属和协调过程 | [架构](docs/architecture.md) |
| 构建、测试或打包改动 | [开发指南](docs/development.md) |
| 了解 CI 和镜像发布流程 | [CI](docs/ci.md) |
| 启用可选的后端集成 | [出站 Connector](docs/outbound-connector.md) |

## 项目状态

当前 API 为 **alpha**。

`ClusterCapabilities` 提供只读配置报告，供部署规划参考。

网络隔离需要配置 NetworkPolicy，并由支持它的 CNI 执行。

应用认证由 Agent 或入口设施提供。
配置细节见[使用指南](docs/usage.md)。

[贡献指南](CONTRIBUTING.md) · [安全策略](SECURITY.md)

## 许可证

项目使用 [Apache-2.0](LICENSE) 许可证，第三方声明见 [NOTICE](NOTICE)。
