# Agent deployer access

[editor.yaml](editor.yaml) grants create, read, update and delete access to
AgentDefinitions and AgentInstances, including reading status.
It does not allow status writes.

Installation does not apply this example or bind users to it.

The Operator's ServiceAccount has separate permissions.

An administrator can bind the role to an existing `agent-user` ServiceAccount:

```sh
kubectl apply -f examples/rbac/editor.yaml
kubectl -n agentrally-demo create rolebinding agentinstance-editor \
  --clusterrole=agentinstance-editor-role --serviceaccount=agentrally-demo:agent-user
```

The RoleBinding limits access to `agentrally-demo`.
It grants no access to AgentGroup, ClusterCapabilities, SandboxTemplate, Secret, PVC,
Sandbox, Pod or Namespace.
Add permissions for those resources if the user's work requires them.
See [user responsibilities](../../docs/usage.md#users-and-permissions).
