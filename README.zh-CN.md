# Arally Operator

[English](README.md) | 简体中文

**在 Kubernetes 和 OpenShift 上部署、管理容器化 Agent。**

用 Kubernetes 资源声明 Agent 的镜像、配置和生命周期，通过 `kubectl` 或 GitOps 管理。

Arally 准备运行配置，并通过
[agent-sandbox Core 和 Extensions](https://github.com/kubernetes-sigs/agent-sandbox)
申请执行实例，由后者负责 Sandbox 分配和 Pod 管理。每个 Agent 保留自己的应用逻辑和 API。

## 可以做什么

- **运行自己的 Agent 镜像。** 选择 `generic`、`assistant`、`coding` 环境，
  或使用自定义 `SandboxTemplate`。内置环境提供 Pod 默认配置，Agent 应用由你的镜像提供。
- **接入凭据和存储。** 引用已有的 Secret、ConfigMap，并为每个 Sandbox 创建独立的工作空间 PVC，
  预热实例也使用自己的卷。引用外部工作区 PVC 可在升级时保留文件；
  CSI 支持恢复时，可从已有 VolumeSnapshot 初始化私有工作空间。
- **控制生命周期。** 暂停、恢复或显式重启 Agent，也可以设置关闭期限或在任务完成后回收。
- **按组管理 Agent。** 每组使用专用命名空间，配置 Pod Security、资源配额、LimitRange 和网络策略。
  可以按配置设置预热容量，也可以在 Group 层面暂停新分配。
- **配置访问和查看状态。** 通过 Gateway API 或 OpenShift Route 提供可选的 HTTP 入口，
  并查看就绪状态、组容量概览和生命周期指标。

## 工作方式

Arally 在 `arally.io/v1alpha1` 下提供四种资源：

| 资源 | 作用域 | 职责 |
| --- | --- | --- |
| `AgentGroup` | 集群 | 准备一个组的命名空间和策略。 |
| `AgentDefinition` | 命名空间 | 发布可复用配置和不可变版本，不启动 Agent。 |
| `AgentInstance` | 命名空间 | 固定定义版本，管理一个 Agent 的执行、工作区和生命周期。 |
| `ClusterCapabilities` | 集群 | 报告运行时、存储和网络配置，供部署检查参考。 |

先创建 `AgentGroup`，再发布 AgentDefinition，并在同一命名空间中创建 AgentInstance。例如，
[基础 Group](examples/base/group.yaml) 准备 `arally-demo` 命名空间，
下面的 [AgentInstance](examples/base/agent.yaml) 在其中申请一个 Agent：

```yaml
apiVersion: arally.io/v1alpha1
kind: AgentDefinition
metadata:
  name: demo-agent
  namespace: arally-demo
spec:
  agentGroupRef:
    name: demo
  template: generic
  container:
    image: registry.example.com/my-agent:v1
---
apiVersion: arally.io/v1alpha1
kind: AgentInstance
metadata:
  name: demo-agent
  namespace: arally-demo
spec:
  agentGroupRef:
    name: demo
  definitionRef:
    name: demo-agent
```

Arally 管理配置、模板快照、池和分配申请；所依赖的 agent-sandbox 控制器负责分配
Sandbox 并管理 Pod。资源关系详见[架构说明](docs/architecture.md)。

## 快速开始

以下步骤从源码构建 Operator，并安装本地打包的 Helm Chart。

### 前提条件

- Kubernetes 或 OpenShift 集群，以及管理 CRD 和集群 RBAC 的权限。
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

- **暂停和恢复：** 删除并重建 Pod。PVC 数据保留，进程内存和临时文件不会保留。
- **配置更新：** 已分配的 Agent 继续使用当前 Pod 配置，直到你设置新的 `restartToken`。
  私有 PVC 随 Sandbox 替换，引用的外部工作区 PVC 则保留数据。定义更新不会自动改变已固定的实例版本。
- **到期回收：** 通过可选的关闭期限和完成后 TTL，控制何时回收执行资源、是否保留 AgentInstance。

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

当前 API 为 **alpha**。`ClusterCapabilities` 提供只读配置报告，供部署规划参考。

网络隔离需要配置 NetworkPolicy，并由支持它的 CNI 执行。应用认证由 Agent 或入口设施提供。
配置细节见[使用指南](docs/usage.md)。

[贡献指南](CONTRIBUTING.md) · [变更记录](CHANGELOG.md) · [安全策略](SECURITY.md)

## 许可证

项目使用 [Apache-2.0](LICENSE) 许可证，第三方声明见 [NOTICE](NOTICE)。
