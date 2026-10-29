# Invocation preview

The elected Operator can forward bounded HTTP calls over the outbound Connector.
This feature is experimental and disabled by default.
Production invocation and exclusive handoff remain unsupported.

Enable `connector.enabled` with its normal backend credentials, then explicitly set
`connector.invocationPreview: true` for controlled integration testing.
The capability document reports `invocationPreview: true`, while `invocation` and
`exclusiveHandoff` remain `false`.
The backend must implement the protocol below before enabling the preview.

## Agent and Group configuration

The accepted AgentDefinition revision must declare `invocationProtocol: HTTPJSONSSEV1`.
The application implements `GET /ping` and `POST /start`.
Start accepts a JSON object or JSON string and returns bytes or an SSE response.
Application success is interpreted by the backend's Agent contract.

The Group must declare:

```yaml
spec:
  invocationAccess:
    mode: ConnectorOnly
```

The Operator's existing Group policy then permits ingress from Pods matching its
release, application and manager labels in its own namespace.
Namespace and Pod selectors are combined in one peer.
Existing egress rules and their scope are preserved.
ConnectorOnly rejects explicit ingress rules and nonempty Pod selectors in the Group.
Additional ingress policies or any Instance interaction configuration block invocation.
The Instance controller removes owned external routes when it observes this conflict.

`InvocationAccessReady` describes configuration.
`IngressEnforcementVerified` remains `Unknown`; actual CNI behavior requires a traffic test.
An extra policy may already permit direct traffic before the controller detects it.
Closing the Connector gate cannot revoke that direct access.
Administrators must control who can create policies and Pods with Operator identity labels.

## Authenticated sessions

All paths below are relative to `connector.endpoint` and start with
`/internal/v2/connectors/{id}`.
They use the existing Bearer credential over HTTPS and reject redirects.
The management v1 worker runs separately from invocation and control polling.

| Path | Method | Contract |
| --- | --- | --- |
| `/sessions` | POST | Send `protocolVersion: invocation.v1` and a random `nonce`. Return both unchanged and a new `connectionEpoch`. |
| `/invocations` | GET | Return one command, or 204 when idle. Finish long polls within 30 seconds. |
| `/controls` | GET | Independent gate/cancel polling, with the same long-poll contract. |
| `/invocations/{requestId}/result` | POST | Accept a JSON transport result; 2xx acknowledges receipt. |
| `/invocations/{requestId}/stream` | POST | Consume incremental `application/x-ndjson`; final 2xx acknowledges receipt. |
| `/controls/{id}/result` | POST | Accept a control receipt; 2xx acknowledges it. |

Every request after the handshake carries `X-Connector-Epoch` and the handshake credential.
The backend checks the epoch **before dequeuing** a command.
Creating a new session invalidates pending polls for the old epoch.
Never rebind old commands to the new epoch.
Epochs are opaque identifiers of at most 128 characters, using letters, digits, `.`, `_`, and `-`.
They start with a letter or digit and cannot be reused.

Invocation/control polling failures, control ACK failure, management disconnect,
observed token rotation and leader shutdown close the invocation session.
Forwarding is cancelled and every Start gate is closed.
Management CRUD retains its existing response retry behavior.
The readiness check reports management connectivity and latched preview failures.
It does not verify that an invocation session or application is ready.

## Commands and target verification

Each command contains `connectionEpoch`, `requestId`, `scope`, `instanceId`,
`instanceUID`, `executionId`, `gateEpoch`, `operation`, `issuedAt` and `expiresAt`.
Scope contains `groupName`, `groupUID` and `namespaceUID`.
Request IDs use the same identifier format as epochs.

`operation` is `Ping` or `Start`.
Ping has no body and uses gate epoch 0.
Start requires an open gate at the matching epoch and `contentType: application/json`.
Its `body` is a JSON object or UTF-8 JSON string.
Optional `headers` allow only `Accept`, `Idempotency-Key` and `Traceparent`.
Content-Type comes only from `contentType`.
Header aliases, CR/LF and values over 4 KiB are rejected.

Times use UTC RFC3339Nano.
The issue time may be at most five seconds ahead of the local clock.
The expiry must be in the future and within the operation's total budget.
The local deadline is the earlier of that expiry and the budget measured from receipt.

The Connector derives the namespace from the verified Group.
It checks Namespace ownership, Instance identity, accepted immutable revision,
Sandbox/Pod ownership and current container execution identity.
The selected agent container must expose a named TCP `http` port or one distinct TCP port.
Host-network Pods are unsupported.

With an enabled Sandbox Service, its owner, selector and ready EndpointSlices must
identify the current Pod and addresses.
Only explicit `spec.service: false` allows direct PodIP resolution without Service checks.
Application connections use the verified PodIP, with no environment proxy, redirects,
keepalive or automatic request retries.
Callers cannot supply a URL, address, service, port or path.

Identity is checked before dispatch and after a complete response.
These checks are observations, not atomic process fencing.

## Results and streams

Results contain `requestId`, `executionId`, `delivery`, `reason`, and any response
`statusCode`, allowed `headers` and `bodyBase64`.
Response headers allow only Content-Type, Cache-Control, Retry-After and Traceparent.
Authentication and cookie headers are excluded.

| Delivery | Meaning |
| --- | --- |
| NotDispatched | Rejected or cancelled before the local dispatch boundary. |
| ResponseReceived | Complete response and successful final identity check. |
| OutcomeUnknown | Dispatch began, but response delivery or final identity could not be confirmed. |

HTTP status, including 202, and transport completion do not prove task completion.
A result ACK may be retried with identical bytes for up to 30 seconds.
The backend must persist and deduplicate results by session and request ID.
Different result content for the same request is a protocol conflict.
An application request is never retried by the Connector.

SSE response bytes are forwarded unchanged as NDJSON frames.
Every frame carries `requestId`, `executionId` and a contiguous `sequence`, starting at zero.

- `headers`: sequence 0, response `statusCode` and allowed `headers`.
- `data`: `dataBase64`, containing at most 32 KiB of original SSE bytes.
- `end`: reason `EOF`, `Cancelled`, `DeadlineExceeded`, `Backpressure` or `UpstreamError`.

A stream cannot resume across disconnects.
Missing end frames, sequence gaps or identity mismatches mean an incomplete stream.
A broken connection may prevent delivery of the end frame itself.
After stream acknowledgement, a separate metadata result reports the final identity check.
The backend must accept that result before classifying transport as complete.
An EOF frame alone is insufficient.

## Gate, drain and cancel

Controls contain `id`, `kind`, `connectionEpoch`, the complete invocation identity,
and UTC `issuedAt`/`expiresAt` within a five-minute window.
`kind: Gate` adds `gateEpoch` and `action: Open|Close|Inspect`.
Open and Close require monotonically increasing epochs for an execution.
Repeating the same epoch and action is idempotent; conflicting reuse is rejected.
Inspect reads the gate without changing it.

Open verifies the current target and refuses while previous Starts are still draining.
Close and dispatch are ordered under the same local lock.
The receipt reports `activeStarts` and `drainDeadline`.
Existing Starts may drain for 30 seconds, then local forwarding is cancelled.
Receipts include `id`, HTTP-style `status`, `reason` and the observed `gateEpoch`.
Control processing and ACK share a five-second deadline from local receipt.

`kind: Cancel` adds `requestId` and `reason: ClientCancelled`.
Its identity and original issue/expiry times must match the call exactly.
It cancels queued or active forwarding, including when cancellation arrives first.
Neither Cancel nor Close changes `AgentInstance.spec.operatingMode`.
The backend orders gate closure, draining, explicit suspension and observation.
Cancelling HTTP forwarding does not prove the application task or old process stopped.

## Bounds and rollout gates

| Resource | Limit |
| --- | --- |
| Active Starts | 16 globally, 8 per Group, 1 per Instance |
| Active Pings | Independent global budget of 2, up to 2 per Instance |
| Pending calls | 64; capacity wait up to 5 seconds |
| Request / response bytes | 1 MiB / 4 MiB; result envelope up to 6 MiB |
| SSE | 1 MiB per event; 32 KiB decoded chunks, no complete-stream buffering |
| Ping | 5 seconds total |
| Start | 30-second response-header wait, 60-second body idle, 300 seconds total |
| Downstream writes | Abort after 5 seconds blocked |
| Session history | 1024 request summaries, 1024 control receipts, 64 gates, 64 early cancellations |

Active calls hold their capacity until result ACK completes.
Rejected calls retain a pending slot until ACK, and release request bodies beforehand.
The poller delivers queue-full refusals synchronously.
History exhaustion closes the session instead of discarding deduplication records.
After 64 distinct epochs, or if the backend reuses an epoch, the invocation preview
stops until the Operator process is restarted.
Its failure is latched; the preview never reconnects or clears anti-replay history
in the same process.
The elected replica's Connector readiness check fails, while management polling
and core reconciliation continue.
Restart the Operator after correcting the backend failure to re-enable the preview.

The [shared fixture](../tests/fixtures/connector/invocation-v1.json) and reference-backend
tests cover the Operator transport contract.
Production rollout still requires real Orchestrator integration and the combined
1 CPU / 512 MiB load test: 16 SSE streams at 16 KiB/s for five minutes, one slow reader,
management p95 at most two seconds and controller observation within ten seconds.
Target-cluster ingress enforcement must be verified separately.

Exclusive handoff remains disabled because no verified fencing provider is integrated.
Management `exclusivehandoff` requests return 501 Unsupported without Kubernetes writes.
An absent Pod, a reservation Lease or a cancelled HTTP request is not release evidence.
