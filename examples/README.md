# AgentRally Operator CR references

This directory contains basic deployment manifests and full configuration references.
Complete applications belong in [agentrally-examples](https://github.com/chizhang21/agentrally-examples/tree/main/operator).

## Basic deployment

Install [Core, Extensions and AgentRally](../docs/installation.md).
Replace the image in [basic/definition.yaml](basic/definition.yaml).
Run from the repository root:

```sh
kubectl apply -f examples/basic/group.yaml && kubectl wait agentgroup/demo --for=condition=Ready --timeout=120s
kubectl apply -f examples/basic/definition.yaml -f examples/basic/instance.yaml
```

The Group namespace must exist before namespaced resources can be created.
Running is the default operating mode.

## Full field references

| Resource | Reference |
| --- | --- |
| AgentGroup | [full/group.yaml](full/group.yaml) |
| AgentDefinition | [full/definition.yaml](full/definition.yaml) |
| AgentInstance | [full/instance.yaml](full/instance.yaml) |
| ClusterCapabilities administrator mappings | [full/cluster-capabilities.yaml](full/cluster-capabilities.yaml) |
| Native source for `templateRef` | [full/sandbox-template.yaml](full/sandbox-template.yaml) |

These files document available configuration; they require adaptation before use.
Use a built-in `template` or a native `templateRef` on a Definition.
The full Group and namespaced references use `agentrally-reference`, separate from the basic deployment.
Do not apply this directory recursively.

Prepare referenced Secrets, ConfigMaps, ServiceAccounts, storage and routing dependencies first.
AgentWorkspace is controller-managed and has no user-created example.
ClusterCapabilities is created automatically; apply mappings only when needed.

See [Usage](../docs/usage.md) for behavior and [deployer RBAC](../config/rbac/editor-role.yaml) for access grants.
`make check` validates the CR schemas, field preservation and resource references.
