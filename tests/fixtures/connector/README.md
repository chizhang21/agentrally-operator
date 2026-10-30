# Connector contract fixtures

[management.json](management.json) fixes the current capability document, a scoped CR creation and the Secret metadata response.
It includes unsupported operations and an envelope with an unknown field that must be rejected.
There is one current protocol and no version fallback.
Secret metadata has no `type` or `keys`; consumers must accept an explicit key for existing Secrets.

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
