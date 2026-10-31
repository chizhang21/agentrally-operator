# Connector contract fixtures

[management.json](management.json) fixes the current capability document and scoped creation contracts.
It includes metadata-only Secret/ConfigMap results and guarded deletion requests with their generic 202 Status.
It includes unsupported operations and an envelope with an unknown field that must be rejected.
There is one current protocol and no version fallback.
`secretCreateTypes` lists exactly `Opaque` and `kubernetes.io/dockerconfigjson`.
Secret metadata has no `type` or `keys`; consumers must accept an explicit key for existing Secrets.
Secret DELETE requires the creation marker and matching UID/resourceVersion.
ConfigMap creation/discovery/deletion uses the same metadata and identity boundaries.
Empty ConfigMaps are valid; their content and key lists are omitted from results.
The backend must also authorize cleanup and compare the marker with its creation record.
Accepted deletion does not prove that consumers have stopped using a credential.
`instanceDiagnostic` fixes the safe summary embedded in instance observations.
Its resource identity is observation evidence; it does not prove process release.

Enabling invocation preview changes only `invocationPreview`.
Production invocation, exclusive handoff and observation-driven intents remain unsupported.

[invocation.json](invocation.json) contains an invocation command, ordered stream frames and a correlated transport result.
The reference consumer rejects missing end frames, sequence gaps and execution changes.
An SSE end frame or HTTP success does not establish business completion.
Unknown delivery must not trigger automatic replay.

Run the fixture and reference-backend checks with:

```sh
go test ./internal/connector
```

Consumers should read these JSON fixtures independently of the Operator's Go types.
Real Orchestrator integration and combined management/invocation load validation remain separate rollout gates.
