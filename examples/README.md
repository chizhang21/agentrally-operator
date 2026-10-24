# Examples

Install [Core, Extensions and Arally](../docs/installation.md) first. Replace image
names and other placeholders before applying a file.

Follow the [minimal deployment sequence](../docs/usage.md) to create a Group and
wait for its namespace, publish its AgentDefinition, then create AgentInstances.
Each standalone instance example has a corresponding `*-definition.yaml`; the base
uses [base/definition.yaml](base/definition.yaml). Apply that file before the instance.

| File | Use | Prerequisites |
| --- | --- | --- |
| [base/group.yaml](base/group.yaml) | Group, quota and optional HTTPRoute defaults | Administrator permissions |
| [base/definition.yaml](base/definition.yaml) | Minimal reusable generic definition | Your image |
| [base/agent.yaml](base/agent.yaml) | Minimal generic agent | Your image |
| [group-policy.yaml](group-policy.yaml) | LimitRange, allocation pause and declarative prewarming | Administrator permissions; applications safe to start before allocation |
| [snapshot-workspace.yaml](snapshot-workspace.yaml) | Private workspace initialized from a snapshot | Ready namespaced VolumeSnapshot, snapshot controller and compatible CSI StorageClass |
| [cluster-capabilities.yaml](cluster-capabilities.yaml) | Administrator classifications for custom runtime/storage classes | Existing native classes and administrator permissions |
| [full.yaml](full.yaml) | Reference for all built-in instance fields | Referenced Secrets, ConfigMap, PVC, RuntimeClass, ServiceAccount, pull credentials and Gateway |
| [custom-template.yaml](custom-template.yaml) / [custom.yaml](custom.yaml) | Native SandboxTemplate reference | Apply template before instance |
| [httproute.yaml](httproute.yaml) | HTTP endpoint through Gateway API | Image serving port 8080; Group defaults or a unique eligible Gateway with a wildcard domain |
| [http-api-agent/](http-api-agent/README.md) | Ordered deployment with Secret environment variables, pull credentials and HTTPRoute | Agent image, model settings, API keys and Gateway |
| [openclaw/](openclaw/README.md) | Resident assistant with state retained across upgrades | External PVC and per-user credentials |
| [codex-mobile/](codex-mobile/README.md) | Build a Codex Mobile image locally or through a manual workflow | Docker, or a GitHub fork with Actions enabled |
| [rbac/editor.yaml](rbac/editor.yaml) | Namespace-scoped deployer access | Administrator applies the [RoleBinding](rbac/README.md) |

Apply one instance example at a time. The base and HTTPRoute examples use the same
name; do not apply the directory recursively. `full.yaml` is a field reference,
not a ready-to-run deployment.

Select assistant or coding on AgentDefinition with `spec.template`; see
[environment settings](../docs/agent-templates.md). Arally creates pools automatically.
It also creates `ClusterCapabilities/cluster`; use the optional report example
only to declare administrator mappings.
Read it with `kubectl get clustercapabilities cluster -o yaml`.

For an endpoint, declare the Agent's HTTP port and set `interaction.type`.
HTTPRoute uses Group defaults or Gateway discovery.
See [interaction setup](../docs/usage.md#interaction-endpoint).
