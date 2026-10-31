# Connector integration

Use the [Helm values example](../chart/connector-values.example.yaml) to enable Connector.
This document defines the protocol a backend must implement to manage resources through it.

Connector is an optional worker inside the Operator process.
Enable it with `connector.enabled: true` in the chart; it is disabled by default.
Only the elected Operator replica polls the backend over HTTPS.
Controllers continue reconciling if Connector is disabled or disconnected.

Connector executes restricted Kubernetes management requests.
Controllers reconcile the resulting CRs and write their status.
The backend authenticates users, authorizes operations and stores their delivery state.

## Installation

Edit [connector-values.example.yaml](../chart/connector-values.example.yaml) with your backend endpoint and assigned cluster id.
The backend must implement the transport and management contracts below.

For an existing release in `agentrally-system`, create the referenced Secret from a backend-issued token file:

```sh
kubectl -n agentrally-system create secret generic agentrally-connector-credentials \
  --from-file=token=/path/to/backend-token
```

Add `-f chart/connector-values.example.yaml` to the [Helm upgrade command](installation.md#upgrade-agentrally).
For a first installation, create the release namespace and credentials before running Helm with these values.

System CA roots are used by default.
For a private backend CA, add `--from-file=ca.crt=/path/to/backend-ca.crt` when creating the Secret and uncomment `caFile` in the example.
Token changes are read on each request; CA changes need an Operator restart.

Connector uses the Operator's binary, Deployment and ServiceAccount.
Enabling it adds CR write permissions, EndpointSlice reads and Secret list/create/delete permissions.
It also adds ConfigMap create/delete permissions; the Controller's existing ConfigMap reads and watch remain unchanged.
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
`secretCreateTypes` lists the allowed Secret creation types: `Opaque` and `kubernetes.io/dockerconfigjson`.
The [shared fixture](../tests/fixtures/connector/management.json) fixes the complete response.

`scopedNamespaceCreate` is true under the management boundary described below.
`instanceDiagnostics` is true: instance observations include a safe primary diagnostic.
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
| `secrets` | GET, POST, DELETE | Group and namespace identity; metadata reads, restricted credential creation and guarded deletion |
| `configmaps` | GET, POST, DELETE | Group and namespace identity; configuration creation, metadata reads and guarded deletion |
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
  Unknown fields and other deletion options are rejected.
  Follow [Group deletion order](usage.md#lifecycle).

Creation ids must match `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`.
Connector injects `arally.io/connector-operation-id` into created CRs, Secrets and ConfigMaps.
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

Secret POST accepts `Opaque` (the default) and `kubernetes.io/dockerconfigjson`.
Opaque accepts `data`, `stringData` or both, using Kubernetes' normal merge semantics.
Registry credentials require exactly one `.dockerconfigjson` entry in either map.
The other map must be absent or empty.
Kubernetes validates the embedded JSON and native Secret size limit.
Successful creation does not verify registry authentication or image pull access.

Allowed metadata is limited to explicit name and matching namespace.
Labels, annotations, ownerReferences, finalizers and server-assigned fields are rejected.
Secret update, patch and service-account-token creation are unavailable.

Secret GET and LIST negotiate PartialObjectMetadata only.
A server that cannot supply metadata is rejected; Connector does not fall back to a full Secret read.
The response contains name, namespace, UID, resourceVersion, timestamps and the Connector creation marker when present.
It omits data, stringData, type, keys and all other annotations and labels.
Creation returns the same metadata projection.

Secret DELETE requires the current name, Group/Namespace scope and native DeleteOptions:

```json
{
  "id": "delete-secret-1",
  "method": "DELETE",
  "resource": "secrets",
  "namespace": "agentrally-demo",
  "name": "model-key-old",
  "scope": {"groupName": "demo", "groupUID": "<group-uid>", "namespaceUID": "<namespace-uid>"},
  "body": {"preconditions": {"uid": "<secret-uid>", "resourceVersion": "<secret-resource-version>"}}
}
```

Connector reads metadata only and requires a valid Connector creation marker.
Unmarked external or GitOps Secrets remain referenceable but cannot be deleted through this interface.
The backend must authorize cleanup and compare the marker with its saved creation operation.
The marker is an accidental-deletion guard, not proof of ownership.

Stale UID or resourceVersion returns 409 before deletion.
Both preconditions also reach Kubernetes, protecting against changes after the metadata read.
Unavailable or unsafe metadata fails closed without reading the full Secret or attempting deletion.
Success returns a generic 202 Status; finalizers may still retain the object.
Deleting a Secret does not revoke upstream credentials or remove values already held by a process.

For an existing Secret, callers must provide the required key explicitly.
MCP management credentials need a management-side source; this interface cannot extract them from execution-cluster Secrets.
Metadata negotiation does not change Kubernetes RBAC: the Operator still has the native Secret get permission its controllers need.

The backend owns rotation and cleanup timing.
Create a new named Secret, switch references and replace affected executions before retiring the old one.
Consider suspended or retained instances pinned to older Revisions as well as the current Definition.
The Operator does not scan references or restart Pods in response to Secret changes.
The initial Connector token still comes from the installation workflow.

ConfigMap POST accepts native `data`, `binaryData` and optional `immutable`.
Empty ConfigMaps are valid.
Only explicit name and matching namespace are allowed in metadata.
Kubernetes validates keys, data/binaryData overlap and total size.
Creates return identity metadata; GET/LIST never return configuration content.
The backend retains the content and key names from its own writes.

ConfigMap DELETE uses the same creation-marker, UID/resourceVersion and scope protections as Secret deletion.
Unmarked external ConfigMaps remain referenceable but cannot be deleted through Connector.
PUT, PATCH, remote watch and automatic configuration reload are unavailable.
The backend creates a new named configuration and switches references before cleanup.

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

`diagnostic` contains `stage`, `reason`, safe `message` and the verified source resource's
`kind`, `name`, `uid` and `resourceVersion`.
It summarizes configuration, allocation, startup, runtime or cleanup evidence.
Configuration diagnoses describe the current desired generation; execution identity
and health describe the accepted execution and may remain healthy during a configuration error.
Stale configuration conditions are ignored; an in-progress restart remains visible across spec updates.
Missing evidence produces an Observation diagnostic, never a guessed healthy state.
Raw Pod/Event messages, logs and private metadata are omitted.

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
This applies to both POST and DELETE.
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
