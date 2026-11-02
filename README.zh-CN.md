# AgentRally Operator

[English](README.md) | 简体中文

**AgentRally 是用于部署和管理隔离 AI Agent 运行环境的 Kubernetes Operator。**

运行自己的 Agent 镜像，通过 `kubectl` 或 GitOps 管理配置和生命周期。
选择内置环境或自定义 `SandboxTemplate`，引用已有的 Secret 和 ConfigMap。

按组管理 Agent，为各组配置命名空间策略和资源配额。
支持容量预热。

暂停或恢复执行。
通过外部 PVC 在升级时保留工作区数据。

就绪状态、Kubernetes Events 和资源用量指标支持日常运维。
可通过 HTTP 入口暴露 Agent 服务。

通过 Kubernetes RuntimeClass 选择运行时隔离方式，包括 Kata Containers 虚拟机和 gVisor 沙箱。

AgentRally 基于 [agent-sandbox Core 和 Extensions](https://github.com/kubernetes-sigs/agent-sandbox)
完成 Sandbox 分配和 Pod 管理。
每个 Agent 保留自己的应用逻辑和 API。

基础 CR 和完整字段参考放在本仓库的 [examples](examples) 中。
完整 Agent 部署示例放在独立的
[agentrally-examples](https://github.com/chizhang21/agentrally-examples) 仓库的 `operator/` 目录中。

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

安装 AgentRally `0.0.3`，启动一个小型 HTTP Agent，验证挂起、恢复后清理资源。

### 前提条件

- Kubernetes 集群，至少有一个 Linux amd64 节点，并具备管理 CRD 和集群 RBAC 的权限。
- 本机安装 kubectl、Helm 3.17 或更高版本、Git 和 curl。

### 1. 安装 Controller

先安装 agent-sandbox Core 和 Extensions，再从 GHCR 安装 Operator：

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.5/sandbox-with-extensions.yaml && kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
CHART_VERSION=0.0.3
helm install agentrally-operator oci://ghcr.io/chizhang21/helm/agentrally-operator \
  --version "$CHART_VERSION" \
  -n agentrally-system --create-namespace --wait --timeout 5m
```

Chart 使用 `ghcr.io/chizhang21/agentrally-operator:v0.0.3`。
自定义配置和私有仓库凭据见[安装指南](docs/installation.md)。
从源码构建见[开发指南](docs/development.md#package-versions)。

### 2. 启动 Agent

获取与 Chart 匹配的示例：

```sh
git clone --branch v0.0.3 --depth 1 https://github.com/chizhang21/agentrally-operator.git
cd agentrally-operator
kubectl apply -f examples/02-group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/03-definition.yaml -f examples/04-instance-basic.yaml && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=300s
```

[Definition](examples/03-definition.yaml) 使用公开镜像
`ghcr.io/chizhang21/agentrally-demo-agent:v0.0.3`。
该流程不需要模型凭证、Gateway、持久卷或额外的 RuntimeClass。
可选参数保留在共享 Group 和 Definition 示例的注释中。

如果 Agent 未就绪，查看条件和事件：

```sh
kubectl -n agentrally-demo describe agentinstance demo-agent
```

各诊断原因的含义见[诊断说明](docs/usage.md#status-and-diagnostics)。

### 3. 调用 Agent

获取当前 Pod 名称，启动本地端口转发：

```sh
POD=$(kubectl -n agentrally-demo get agentinstance demo-agent -o jsonpath='{.status.podName}')
kubectl -n agentrally-demo port-forward "pod/$POD" 18080:8080
```

保持该终端运行。
在另一个终端检查健康状态并发送消息：

```sh
curl -fsS http://127.0.0.1:18080/healthz
curl -fsS http://127.0.0.1:18080/chat -H 'Content-Type: application/json' --data '{"message":"Hello"}'
```

聊天接口返回：

```json
{"response":"AgentRally demo agent is ready.","mode":"demo"}
```

演示模式使用 Pydantic AI 的 `TestModel` 返回固定内容，不调用模型服务。
[Agent 项目](https://github.com/chizhang21/agentrally-examples/tree/v0.0.3/operator/http-api-agent)
提供镜像源码，以及通过 Secret 接入真实模型的步骤。

### 4. 挂起和恢复

```sh
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Suspended --timeout=120s
kubectl -n agentrally-demo patch agentinstance demo-agent --type=merge -p '{"spec":{"operatingMode":null}}' && kubectl -n agentrally-demo wait agentinstance/demo-agent --for=condition=Ready --timeout=300s
```

挂起会删除 Pod，恢复后会创建新 Pod。
重新获取 Pod 名称，再执行上面的端口转发和调用步骤。
需要持久卷或托管 HTTPRoute 时，先准备 StorageClass 和 Gateway，
再参考 [04-instance-full.yaml](examples/04-instance-full.yaml) 配置。

### 5. 清理资源

保持 Operator 运行，按 Instance、Definition、Group 的顺序删除：

```sh
kubectl delete -f examples/04-instance-basic.yaml --timeout=300s
kubectl delete -f examples/03-definition.yaml --timeout=120s
kubectl delete -f examples/02-group.yaml --timeout=300s
```

随后可卸载 Operator：

```sh
helm uninstall agentrally-operator -n agentrally-system
```

Helm 会保留 CRD。

## 生命周期行为

- **暂停和恢复：** 删除并重建 Pod。
  PVC 数据保留，进程内存和临时文件不会保留。
- **配置更新：** 已分配的 Agent 继续使用当前 Pod 配置，直到你设置新的 `restartToken`。
  普通重启保留已接受的 Definition Revision。
  升级时需要同时选择目标 `spec.revision` 并设置新 token。
- **工作区保留：** 私有 PVC 随 Sandbox 替换，引用的外部工作区 PVC 则保留数据。
- **到期回收：** 通过可选的最长执行时间、关闭期限和完成后 TTL，控制何时回收执行资源。
  关闭策略决定是否保留 AgentInstance。

操作命令和保留策略见[生命周期说明](docs/usage.md#lifecycle)。

## 文档

以下详细文档目前为英文：

| 我想…… | 阅读 |
| --- | --- |
| 安装或升级 Operator | [安装指南](docs/installation.md) |
| 配置 Helm Release | [Release 配置](docs/installation.md#release-configuration) · [Values 示例](examples/values) |
| 固定版本和管理工作区生命周期 | [生命周期](docs/usage.md#lifecycle) |
| 部署 Agent、配置入口和日常运维 | [使用指南](docs/usage.md) |
| 查看基础和完整 CR 字段参考 | [示例](examples) |
| 定制运行环境和池 | [Agent 模板](docs/agent-templates.md) |
| 查看集群现有配置 | [集群能力](docs/cluster-capabilities.md) |
| 了解资源归属和协调过程 | [架构](docs/architecture.md) |
| 构建、测试或打包改动 | [开发指南](docs/development.md) |
| 查看 CI 和发布流程 | [GitHub Workflows](.github/workflows) |
| 启用可选的后端集成 | [出站 Connector](docs/outbound-connector.md) |

## 项目状态

当前 API 为 **alpha**。

`ClusterCapabilities` 提供只读配置报告，供部署规划参考。

网络隔离需要配置 NetworkPolicy，并由支持它的 CNI 执行。

应用认证由 Agent 或入口设施提供。
配置细节见[使用指南](docs/usage.md)。

## 许可证

项目使用 [Apache-2.0](LICENSE) 许可证，第三方声明见 [NOTICE](NOTICE)。
