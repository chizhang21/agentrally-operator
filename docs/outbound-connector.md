# Outbound Connector

Connector is an optional worker inside the Operator process.
Enable it with `connector.enabled: true` in the chart; it is disabled by default.
Only the elected Operator replica polls the backend over HTTPS.
Controllers continue reconciling if Connector is disabled or disconnected.

Connector executes restricted Kubernetes management requests.
Controllers reconcile the resulting CRs and write their status.
The backend authenticates users, authorizes operations and stores their delivery state.

## Installation

Create a Secret in the release namespace with a backend-issued `token` and, if needed, `ca.crt`.
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
Token changes are read on each request; CA changes need an Operator restart.

Connector uses the Operator's binary, Deployment and ServiceAccount.
Enabling it adds CR write permissions, EndpointSlice reads and Secret list/create permissions.
The top-level `resources` values cover the whole process.

The backend must bind credentials to cluster id and reject duplicate active connections for that id.

## Transport

There is one current protocol, with no version selector or compatibility fallback.
Paths are relative to `connector.endpoint`:

| Endpoint | Result |
| --- | --- |
| `GET internal/connectors/{id}/requests` | Long poll: 200 with one request or 204 when idle; finish within 30 seconds |
| `POST internal/connectors/{id}/responses` | Response delivery; any 2xx acknowledges it |

Both use Bearer authentication.
Redirects are rejected; messages are limited to 4 MiB.
Unknown envelope fields and malformed poll responses disconnect the session.

One worker executes management requests serially.
A response contains `id`, `status` and `body`.
`status` is the Kubernetes operation's HTTP code, separate from the transport ACK.
A successful DELETE means deletion was requested; finalizers may still be running.

Errors retain Kubernetes status codes and reasons.
Native messages and details are replaced because validation errors may echo submitted values.

## Capability discovery

Use an unscoped request with no name, body or pagination:

```json
{"id":"capabilities-1","method":"GET","resource":"connectorcapabilities"}
```

The response contains a `resources` map from resource name to supported HTTP methods.
It also reports `maxMessageBytes`, `maxListLimit` and feature flags.
The [shared fixture](../tests/fixtures/connector/management.json) fixes the complete response.

`scopedNamespaceCreate` is true under the management boundary described below.
Production `invocation`, `exclusiveHandoff`, `observedOperatingMode`, `activityLease` and `processIdentity` remain false.
Unsupported capabilities return `501 Unsupported`.
An application protocol declaration does not override these flags.

## Management scope

The backend must authenticate the caller and authorize its cluster and Group access.
Scope identifiers are identity preconditions, not credentials.

| Resource | Methods | Scope and response |
| --- | --- | --- |
| `agentgroups` | GET collection | Unscoped administrator inventory; identity metadata only |
| `agentgroups` | GET one, POST, PUT, DELETE | Group name and UID; POST omits the not-yet-known UID |
| `namespaces` | GET one | Verified Group namespace identity |
| `agentdefinitions`, `agentinstances` | GET, POST, PUT, DELETE | Group UID, namespace UID and matching `spec.agentGroupRef.name` |
| `secrets` | GET, POST | Group and namespace identity; metadata reads and Opaque creation |
| `persistentvolumeclaims` | GET | Group namespace; selected identity, spec and status fields |
| `agentworkspaces` | GET | Group and namespace identity; cluster-scoped envelope without `namespace` |
| `instanceobservations` | GET one | Group and namespace identity |
| `clustercapabilities` | GET | Unscoped, read-only discovery |

Group inventory is an administrator operation.
Do not expose the cluster-wide list to tenant callers.
After discovering or creating a Group, bind its UID and discover its namespace:

```json
{"id":"namespace-1","method":"GET","resource":"namespaces","scope":{"groupName":"demo","groupUID":"<group-uid>"}}
```

This bootstrap read may omit the Namespace name and expected namespace UID.
Subsequent namespaced requests must use the returned name and `scope.namespaceUID`.
A supplied stale UID is rejected, including during discovery.
Namespace labels and annotations are not returned.

Ordinary Group namespaces require the exact Group owner UID.
Persistent Group namespaces use the pinned `status.namespaceUID` and have no owner reference.

## CR writes

A creation request uses the native CR body:

```json
{
  "id": "create-instance-1",
  "method": "POST",
  "resource": "agentinstances",
  "namespace": "agentrally-demo",
  "scope": {"groupName": "demo", "groupUID": "<group-uid>", "namespaceUID": "<namespace-uid>"},
  "body": {
    "apiVersion": "arally.io/v1alpha1",
    "kind": "AgentInstance",
    "metadata": {"name": "assistant", "namespace": "agentrally-demo"},
    "spec": {"agentGroupRef": {"name": "demo"}, "definitionRef": {"name": "assistant"}}
  }
}
```

- POST needs explicit `metadata.name`; `generateName`, UID and resourceVersion are rejected.
- PUT requires the named CR's current UID and resourceVersion.
- Envelope and body names/namespaces must match.
  Incoming status is discarded.
- CR creation metadata permits only name, namespace, labels and annotations.
  Updates must preserve controller-owned finalizers and owner references.
  Connector rejects updates to deleting resources.
- Creates and updates request strict API-server field validation.
- DELETE needs native DeleteOptions with UID and resourceVersion preconditions and uses foreground deletion.
  Follow [Group deletion order](usage.md#lifecycle).

Creation ids must match `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`.
Connector injects `arally.io/connector-operation-id` into created CRs and Secrets.
Callers cannot supply that annotation, and CR updates must preserve it.
The backend must generate and persist the id and must not reuse it for another target or body.

A duplicate name returns AlreadyExists; it never becomes an update.
After a lost response, compare the observed marker, resource identity and original operation record.
The marker is a correlation aid, not an authorization credential or exactly-once guarantee.
It establishes no application readiness or business completion.

An explicit AgentInstance update may carry `intent: {"kind":"Explicit"}`.
Success records desired state; it does not confirm that execution has stopped.
Observation-driven `Observed` intents remain unsupported.

### Namespace lifecycle boundary

Namespaced requests verify Group UID, Namespace UID and ownership before and after the operation.
Writes reject a scope already observed to be terminating.

These checks do not form a cross-resource transaction.
Kubernetes cannot condition a namespaced create on its parent Namespace UID.
The backend must use one active management connection per cluster.
Group deletion through this serial worker cannot interleave with a creation.

Before an administrator or GitOps controller deletes, recreates or changes ownership of a Group/Namespace outside this channel, stop that Group's management writes and drain outstanding requests.
Connector does not guarantee isolation against concurrent administrative namespace replacement.
`409 ScopeChanged` can be returned after a write has taken effect; it does not prove rollback or prevent Secret exposure in an externally replaced namespace.

## Secret and storage references

Secret POST accepts only an Opaque Secret with `data` or `stringData`.
Allowed metadata is limited to explicit name and matching namespace.
Labels, annotations, ownerReferences, finalizers and server-assigned fields are rejected.
Secret update, delete and service-account-token creation are unavailable.

Secret GET and LIST negotiate PartialObjectMetadata only.
A server that cannot supply metadata is rejected; Connector does not fall back to a full Secret read.
The response contains name, namespace, UID, resourceVersion, timestamps and the Connector creation marker when present.
It omits data, stringData, type, keys and all other annotations and labels.
Creation returns the same metadata projection.

For an existing Secret, callers must provide the required key explicitly.
MCP management credentials need a management-side source; this interface cannot extract them from execution-cluster Secrets.
Metadata negotiation does not change Kubernetes RBAC: the Operator still has the native Secret get permission its controllers need.

PVC queries return identity plus accessModes, volumeMode, storageClassName, requested storage, phase and storage capacity.
They omit PV names, data sources and arbitrary metadata.
PVC lifecycle remains driven by AgentInstance workspace declarations and the controllers.

AgentWorkspace queries return spec/status after verifying their immutable Group and Namespace identities.
Lists use Operator-maintained UID labels as a query index and check spec independently.
Storage observations do not prove mountability, released writers or recovery readiness.

GET collections use Kubernetes pagination: `limit` defaults to 100 and cannot exceed 500.
Pass `continue` until empty, including after an empty filtered Workspace page.
Reference lists omit remaining-item counts.

Connector does not manage upstream SandboxTemplates or install runtimes.
RuntimeClass names must refer to real cluster resources.

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
Execution identity is unavailable until the Controller has recorded a complete accepted binding.

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
A latched invocation-preview failure also fails Connector readiness.
Management polling and local reconciliation continue until the Operator is restarted.

Connector keeps no durable operation log or separate capability cache.

## Invocation preview

Controlled JSON/SSE invocation is available behind the default-off
`connector.invocationPreview` flag.
See the [invocation protocol](invocation-protocol.md) for the Agent contract,
ConnectorOnly ingress, session epochs, limits and rollout gates.

Production `invocation` and `exclusiveHandoff` remain false.
Reference-backend tests do not replace real Orchestrator integration.
The [shared contract fixtures](../tests/fixtures/connector/README.md) record the current
management capabilities, unsupported requests and invocation stream examples.
Consumers can validate those fixtures without importing the Operator's Go types.
