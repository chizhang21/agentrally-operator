# Examples

Install [Core, Extensions and Arally](../docs/installation.md) first. Replace image
names and other placeholders before applying a file.

Follow the [minimal deployment sequence](../docs/usage.md) to create a Group and
wait for its namespace before applying an AgentClaim.

| File | Use | Prerequisites |
| --- | --- | --- |
| [base/group.yaml](base/group.yaml) | Group, quota and optional HTTPRoute defaults | Administrator permissions |
| [base/agent.yaml](base/agent.yaml) | Minimal generic agent | Your image |
| [full.yaml](full.yaml) | Reference for all built-in Claim fields | Referenced Secrets, ConfigMap, PVC, RuntimeClass, ServiceAccount, pull credentials and Gateway |
| [custom-template.yaml](custom-template.yaml) / [custom.yaml](custom.yaml) | Native SandboxTemplate reference | Apply template before Claim |
| [httproute.yaml](httproute.yaml) | HTTP endpoint through Gateway API | Image serving port 8080; Group defaults or a unique eligible Gateway with a wildcard domain |
| [http-api-agent/](http-api-agent/README.md) | Ordered deployment with Secret environment variables, pull credentials and HTTPRoute | Agent image, model settings, API keys and Gateway |
| [codex-mobile/](codex-mobile/README.md) | Build a Codex Mobile image locally or through a manual workflow | Docker, or a GitHub fork with Actions enabled |
| [openshift.yaml](openshift.yaml) | OpenShift HTTP endpoint | Image serving port 8080 and Route API |
| [anyuid.yaml](anyuid.yaml) | Legacy image needing a fixed UID/root | Baseline Group and [SCC-authorized account](../docs/usage.md#legacy-agent-images-on-openshift) |
| [rbac/editor.yaml](rbac/editor.yaml) | Namespace-scoped deployer access | Administrator applies the [RoleBinding](rbac/README.md) |

Apply one Claim example at a time. The base, HTTPRoute and OpenShift examples use the same
name; do not apply the directory recursively. `full.yaml` is a field reference,
not a ready-to-run deployment.

Select assistant or coding with `spec.template`; see
[environment settings](../docs/agent-templates.md). Arally creates pools automatically.
It also creates `ClusterCapabilities/cluster`; no report YAML is needed.
Read it with `kubectl get clustercapabilities cluster -o yaml`.

For an endpoint, declare the Agent's HTTP port and set `interaction.type`.
HTTPRoute uses Group defaults or Gateway discovery; OpenShiftRoute uses the router.
See [interaction setup](../docs/usage.md#interaction-endpoint).
