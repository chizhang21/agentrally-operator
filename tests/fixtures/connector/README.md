# Connector contract fixtures

[management-v1-v2.json](management-v1-v2.json) fixes the current capability document and
requests that must return 501 without accessing Kubernetes.
It covers management v1 and v2.
Enabling invocation preview changes only `invocationPreview`; production invocation,
exclusive handoff and observation-driven intents remain unsupported.

[invocation-v1.json](invocation-v1.json) contains an invocation command, ordered stream
frames and a correlated transport result.
The reference consumer rejects missing end frames, sequence gaps and execution changes.
An SSE end frame or HTTP success does not establish business completion.
Unknown delivery must not trigger automatic replay.

Run the Operator's fixture and reference-backend checks with:

```sh
go test ./internal/connector
```

Consumers should read these JSON fixtures independently of the Operator's Go types.
Passing the Operator tests establishes its side of the contract.
Real Orchestrator integration and combined management/invocation load validation remain
separate rollout gates.
