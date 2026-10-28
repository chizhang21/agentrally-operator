# Outbound Connector

Connector is an optional worker inside the Operator process.
Enable it with `connector.enabled: true` in the chart; it is disabled by default.
Only the elected Operator replica polls the backend over HTTPS and forwards AgentRally CR
operations to Kubernetes.

The controllers continue reconciling if Connector is disabled or disconnected.
Kubernetes validates and stores objects, and the controllers write their status.

## Installation

Create a Secret in the release namespace with a backend-issued `token` and, if needed,
`ca.crt`.
Reference it in Helm values:

```yaml
connector:
  enabled: true
  endpoint: https://agentrally.example.com/
  id: production-cluster
  credentialsSecret: agentrally-connector-credentials
  # For a private backend CA:
  caFile: /var/run/agentrally-connector/ca.crt
```

Omit `caFile` for system CA roots.

The image contains one `/operator` binary; Connector uses the same Deployment, container
and ServiceAccount.
Enabling it mounts the Secret into Operator Pods and adds the CR write permissions
needed beyond the controller role, plus read-only EndpointSlice discovery.

Remote writes remain restricted to the CRs listed below.
Management v2 also exposes verified namespace identity and instance observations.

The top-level `resources` values cover the whole process.

Token changes are read on each request; CA changes need an Operator restart.

The backend must bind credentials to cluster id and reject duplicate active sessions for
the same id.

## Transport

Paths are relative to `connector.endpoint`:

| Endpoint | Result |
| --- | --- |
| `GET internal/v1/connectors/{id}/requests` | Long poll: 200 with one request or 204 when idle; finish within 30 seconds |
| `POST internal/v1/connectors/{id}/responses` | Response delivery; any 2xx acknowledges it |

Both use Bearer authentication.
Redirects are rejected; bodies are limited to 4 MiB.
Malformed poll responses disconnect the session.

One worker executes requests serially.
Errors retain Kubernetes status codes and reasons.
Native error messages and details are replaced because validation errors may echo submitted values.

The transport paths stay at v1.
The request envelope's optional `version` selects the management protocol independently.
Omitting it selects legacy v1.

## Native management v1

Legacy v1 is a trusted cluster-administrator interface.
It does not enforce Group scope and must not be exposed directly to tenant callers.

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

- GET with `name` reads an object; otherwise it lists.
  `limit` defaults to 100, maximum 500; `continue` uses Kubernetes pagination.
- POST needs explicit `metadata.name`; `generateName` is rejected.
- PUT needs the named CR and current `metadata.resourceVersion`.
- Envelope and body names/namespaces must match.
  Incoming status is discarded.
- Native creates and updates request strict API-server field validation.
  Retired `idleTimeoutSeconds` and `activityLeaseRef` fields return
  `400 RemovedActivityContract` instead of silently disappearing.
- DELETE needs native DeleteOptions with UID and resourceVersion preconditions; it
  requests foreground deletion.
  Follow [Group deletion order](usage.md#lifecycle).

A response has `id`, `status` and `body`.
`status` is the Kubernetes operation's HTTP code; `body` is its object or Status error:

```json
{"id":"capabilities-1","status":200,"body":{"apiVersion":"arally.io/v1alpha1","kind":"ClusterCapabilities","metadata":{"name":"cluster"},"spec":{}}}
```

An actual capability response also contains [discovery status](cluster-capabilities.md).

A successful DELETE means deletion was requested; finalizers may still be running.

The delivery endpoint's HTTP code is separate from the operation's `status`.

Connector cannot manage Secrets, PVCs or upstream SandboxTemplates.
Prepare those through kubectl, GitOps or an administrator.

RuntimeClass names must be real cluster names.
Connector does not install runtimes.

## Management capability discovery

Use an unscoped request with no name, body or pagination:

```json
{"id":"protocol-1","method":"GET","resource":"connectorcapabilities"}
```

The response body has `schemaVersion: management.capabilities/v1`.
It lists each supported management version and its resource/method matrix.
It also reports message and pagination limits.

`invocation`, `observedOperatingMode`, `activityLease`, `processIdentity` and
`scopedNamespaceCreate` are currently false.
Unsupported protocol versions and capabilities return `501 Unsupported`.
An application protocol declaration does not override these capability flags.

Old Connectors may reject capability discovery.
Treat that as legacy management only; do not downgrade scoped or automatic operations to v1.

## Scoped management v2

Set `version: v2` and provide `scope.groupName` and the expected `scope.groupUID`.
The backend must authenticate the caller and authorize that Group.
Scope identifiers are identity preconditions, not credentials.

| Resource | Methods | Preconditions |
| --- | --- | --- |
| `agentgroups` | GET one, POST, PUT, DELETE | Name matches Group scope; POST omits the not-yet-known Group UID |
| `namespaces` | GET one | Verified Group ownership; only name, UID, resourceVersion and deletion timestamp are returned |
| `agentdefinitions`, `agentinstances` | GET, PUT, DELETE | Expected namespace UID and matching `spec.agentGroupRef` |
| `instanceobservations` | GET one | Expected namespace UID and matching Group reference |
| `clustercapabilities` | GET | Unscoped |

After creating or confirming a Group, discover its namespace identity:

```json
{"id":"namespace-1","version":"v2","method":"GET","resource":"namespaces","scope":{"groupName":"demo","groupUID":"<group-uid>"}}
```

This bootstrap read may omit the Namespace name and expected namespace UID.
Subsequent namespaced requests must use the returned name and `scope.namespaceUID`.
A supplied stale UID is always rejected, including during discovery.
Namespace labels and annotations are not returned.

PUT requires the current object's UID and resourceVersion.
DELETE requires both in native DeleteOptions.
Namespaced POST is unsupported because the API server cannot condition creation on
the parent namespace UID.
Prepare those resources through an authorized administrator or GitOps workflow.

An explicit AgentInstance update may carry `intent: {"kind":"Explicit"}`.
It requests native desired state; success does not confirm that the workload has stopped.
Observation-driven intents with `kind: Observed` always return Unsupported before
accessing Kubernetes.
Clients must disable that policy, rather than retrying it as Explicit or v1.

## Instance observations

`GET instanceobservations` returns stable `instanceId` (the CR name), `instanceUID`,
Group UID, the accepted Definition binding and any verifiable current execution.
The backend combines its authenticated cluster identity with Group UID and instance ID
to identify a logical instance.
Recreating the same name produces a different CR UID.

The execution ID covers Instance, Sandbox and Pod UIDs, the selected agent container,
container ID, restart count and start time.
A restart within the same Pod therefore changes execution identity.
The container name comes from the accepted Revision, never a guessed sidecar position.
Until an old binding is backfilled, execution identity is unavailable.

`identityScope` is Container and `processIdentityVerified` is false.
Replacing an application process inside a live container cannot be detected this way.
Requests for Process observation return Unsupported.

| State | Evidence |
| --- | --- |
| Active | An owned, ready Pod with the selected container running |
| Idle | Current-generation suspension confirmation and an absent owned Pod |
| Terminated | An owned terminal Pod with the selected container terminated |
| Omitted | Incomplete observation, starting, stopping or removed execution resources; inspect `reason` |

Every state carries `stateEvidence: KubernetesObservation` and
`executionRelease: Unknown`.
Neither an absent API object nor these states prove that a disconnected node has
released its processes or exclusive storage.
They cannot authorize exclusive takeover.

The connector rechecks observed objects and absences before replying.
Changes produce `409 ObservationChanged`; this is not a cross-resource transaction.
Scope changes can produce `409 ScopeChanged` after a write has already taken effect.
Read current state before deciding whether to retry.

## Failure handling

Transport errors retry with bounded backoff.

After executing a write, Connector retries the in-memory response until acknowledged,
without re-executing the write.
The backend must accept duplicate responses for the same request id.

A process restart can lose an unacknowledged response.
A Kubernetes timeout may also leave the write result uncertain.
Mark it unknown and read the resource before retrying.
Request ids correlate messages; they do not guarantee exactly-once execution.

Dispatch requests only during an authenticated live poll.
Expire pending requests on disconnect and reject writes while offline.

When enabled, `/readyz/connector` on the Operator health port reports backend
connectivity on the elected replica; standby replicas pass this check without polling.
A disconnected leader fails the aggregate `/readyz` check while `/healthz` and local
reconciliation continue.

Connector keeps no durable operation log or separate capability cache.

## Invocation preview

Controlled JSON/SSE invocation is available behind the default-off
`connector.invocationPreview` flag.
See the [invocation protocol](invocation-protocol.md) for the Agent contract,
ConnectorOnly ingress, session epochs, limits and rollout gates.

Production `invocation` and `exclusiveHandoff` remain false.
Reference-backend tests do not replace real Orchestrator integration.
