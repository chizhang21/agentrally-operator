# HTTP API Agent

A generic agent serving HTTP on port 8000, with model credentials, an external
tool API key and an API authentication token supplied through a Secret.

Install [Arally and its dependencies](../../docs/installation.md) first. Supply
your agent image and replace the model settings and Secret placeholders. Match
the environment variable names, port and data path to your application. Set the
registry hostname and credentials in `03-registry-secret.yaml` to match the image.
For a public image, skip that file and remove `runtime.imagePullSecrets`.

The Group uses an existing `gateway-system/shared` Gateway with an HTTPS listener
named `https`. Change these defaults to match your Gateway. It must allow
HTTPRoutes from `arally-http-api-agent`; point `*.agents.example.com` at its address and
configure a matching TLS certificate. If the Gateway proxy runs in a different
namespace, update the Group's NetworkPolicy selector as well.

```sh
kubectl apply -f examples/http-api-agent/01-group.yaml
kubectl wait agentgroup/http-api-agent --for=condition=Ready --timeout=120s
kubectl apply -f examples/http-api-agent/02-agent-secret.yaml
kubectl apply -f examples/http-api-agent/03-registry-secret.yaml
kubectl apply -f examples/http-api-agent/04-agent.yaml
kubectl -n arally-http-api-agent wait agentclaim/http-api-agent --for=condition=Ready --timeout=5m
kubectl -n arally-http-api-agent wait agentclaim/http-api-agent --for=condition=InteractionReady --timeout=5m
kubectl -n arally-http-api-agent get agentclaim http-api-agent -o jsonpath='{.status.interaction.url}{"\n"}'
```

For an application using Bearer authentication, use the Secret's `API_TOKEN`
value when calling its API. `TOOL_API_KEY` represents an external tool credential.
The Group allows ingress from its own Pods and from the Gateway namespace on
TCP 8000. It does not restrict egress. NetworkPolicy requires enforcement by the CNI.

This example has no PVC: its SQLite database is lost when the Pod is replaced.
For a private persistent workspace, enable the commented `workspace` template
before deploying. Every Sandbox gets its own PVC, including warm instances.
Data survives suspend/resume; execution replacement, deletion and expiry remove
the Sandbox and its disk. New managed pools default to zero spare instances.
See [prewarming policy](../../docs/agent-templates.md#allocation-and-policy)
before increasing that count.

Secret environment variables are loaded when a container starts. After changing
the Secret or its references, apply the files and change `spec.restartToken` to
replace the execution. Preserve any data in the container filesystem first; a
configuration change alone does not restart the running Agent.
