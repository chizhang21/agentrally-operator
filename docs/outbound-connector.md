# Outbound Connector

Connector is an optional worker inside the Operator process. Enable it with
`connector.enabled: true` in the chart; it is disabled by default. Only the elected
Operator replica polls the backend over HTTPS and forwards Arally CR operations
to Kubernetes. The controllers continue reconciling if Connector is disabled or
disconnected. Kubernetes validates and stores objects, and the controllers write
their status.

## Installation

Create a Secret in the release namespace with a backend-issued `token` and,
if needed, `ca.crt`. Reference it in Helm values:

```yaml
connector:
  enabled: true
  endpoint: https://arally.example.com/
  id: production-cluster
  credentialsSecret: arally-connector-credentials
  # For a private backend CA:
  caFile: /var/run/arally-connector/ca.crt
```

Omit `caFile` for system CA roots. The image contains one `/operator` binary;
Connector uses the same Deployment, container and ServiceAccount. Enabling it
mounts the Secret into Operator Pods and adds the CR write permissions needed
beyond the controller role. Request validation still restricts remote operations
to the four CRs below. The top-level `resources` values cover the whole process.
Token changes are read on each request; CA changes need an Operator restart.
The backend must bind credentials to cluster id and reject duplicate active
sessions for the same id.

## Backend protocol v1

Paths are relative to `connector.endpoint`:

| Endpoint | Result |
| --- | --- |
| `GET internal/v1/connectors/{id}/requests` | Long poll: 200 with one request or 204 when idle; finish within 30 seconds |
| `POST internal/v1/connectors/{id}/responses` | Response delivery; any 2xx acknowledges it |

Both use Bearer authentication. Redirects are rejected; bodies are limited to
4 MiB. Malformed poll responses disconnect the session. One worker executes
requests serially and returns native Kubernetes errors.

| Resource | Scope | Methods |
| --- | --- | --- |
| `agentgroups` | Cluster | GET, POST, PUT, DELETE |
| `agentdefinitions` | Namespace required | GET, POST, PUT, DELETE |
| `agentinstances` | Namespace required | GET, POST, PUT, DELETE |
| `clustercapabilities` | Cluster | GET |

A request has `id`, `method`, `resource` and optional `name`, `namespace`, `body`,
`limit`, `continue`:

```json
{"id":"capabilities-1","method":"GET","resource":"clustercapabilities","name":"cluster"}
```

- GET with `name` reads an object; otherwise it lists. `limit` defaults to 100,
  maximum 500; `continue` uses Kubernetes pagination.
- POST needs explicit `metadata.name`; `generateName` is rejected.
- PUT needs the named CR and current `metadata.resourceVersion`.
- Envelope and body names/namespaces must match. Incoming status is discarded.
- DELETE needs native DeleteOptions with UID and resourceVersion preconditions;
  it requests foreground deletion. Follow [Group deletion order](usage.md#lifecycle).

A response has `id`, `status` and `body`. `status` is the Kubernetes operation's
HTTP code; `body` is its object or Status error:

```json
{"id":"capabilities-1","status":200,"body":{"apiVersion":"arally.io/v1alpha1","kind":"ClusterCapabilities","metadata":{"name":"cluster"},"spec":{}}}
```

An actual capability response also contains [discovery status](cluster-capabilities.md).
A successful DELETE means deletion was requested; finalizers may still be running.
The delivery endpoint's HTTP code is separate from the operation's `status`.

Connector cannot manage Secrets, PVCs or upstream SandboxTemplates. Prepare those
through kubectl, GitOps or an administrator. RuntimeClass names must be real cluster
names. Connector does not install runtimes.

## Failure handling

Transport errors retry with bounded backoff. After executing a write, Connector
retries the in-memory response until acknowledged, without re-executing the write.
The backend must accept duplicate responses for the same request id.

A process restart can lose an unacknowledged response. A Kubernetes timeout may
also leave the write result uncertain. Mark it unknown and read the resource before
retrying. Request ids correlate messages; they do not guarantee exactly-once execution.

Dispatch requests only during an authenticated live poll. Expire pending requests
on disconnect and reject writes while offline. When enabled, `/readyz/connector`
on the Operator health port reports backend connectivity on the elected replica;
standby replicas pass this check without polling. A disconnected leader fails the
aggregate `/readyz` check while `/healthz` and local reconciliation continue.
Connector keeps no durable operation log or separate capability cache.
