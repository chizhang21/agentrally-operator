# Resident OpenClaw assistant

This AgentDefinition references Alice's gateway Secret; its AgentInstance binds
Alice's retained PVC. Application conversations stay inside OpenClaw. Instances of
one definition share its credential references. For another user with different
credentials, create a separate definition referencing that user's Secret and an
instance with its own PVC. There is no per-instance environment override.

This example pins the official `2026.10.4` image. Paths and the LAN listener follow
its [Compose configuration](https://github.com/openclaw/openclaw/blob/v2026.10.4/docker-compose.yml).
Startup/readiness use `/startupz`; liveness uses `/healthz`, following the
[versioned probe guidance](https://github.com/openclaw/openclaw/blob/v2026.10.4/docs/install/docker.md#health-checks).
The probes allow five seconds for a response; the default one-second timeout
was exceeded during live CLI activity. The writable HOME also retains
`.config/openclaw` credentials. `/tmp` is ephemeral.

## Install

A default filesystem StorageClass must support the image UID/GID 1000. Configure
`workspace.yaml` if another class is needed. Install agent-sandbox Core and
Extensions before the Operator. Then:

```sh
kubectl apply -f examples/openclaw/group.yaml
kubectl wait agentgroup/openclaw --for=condition=Ready --timeout=120s
kubectl apply -f examples/openclaw/workspace.yaml
# Supply a private token file; do not commit it.
kubectl -n arally-openclaw create secret generic alice-gateway --from-file=token=/path/to/token
kubectl apply -f examples/openclaw/definition.yaml
kubectl -n arally-openclaw wait agentdefinition/openclaw --for=condition=Ready --timeout=120s
kubectl apply -f examples/openclaw/instance.yaml
kubectl -n arally-openclaw wait agentinstance/alice --for=condition=Ready --timeout=5m
```

On an empty workspace, the startup command writes a minimal local Gateway config
with Control UI disabled, then replaces the shell with OpenClaw. Existing config is
left intact. Complete provider/channel onboarding using the application's CLI in
the reported `status.podName`. Before enabling Control UI, configure its allowed
origins and pairing: the [versioned Gateway startup check](https://github.com/openclaw/openclaw/blob/v2026.10.4/src/gateway/server-runtime-config.ts)
requires explicit origins for a non-loopback UI. This example creates no public
route. Kubernetes readiness does not establish successful channel login or model calls.

## Upgrade and retain state

Change the image in the definition and apply it. Existing instances stay on their
accepted revision. Set a new restart token to adopt the latest published revision:

```sh
kubectl apply -f examples/openclaw/definition.yaml
kubectl -n arally-openclaw wait agentdefinition/openclaw --for=condition=Ready --timeout=120s
kubectl -n arally-openclaw patch agentinstance alice --type=merge \
  -p '{"spec":{"restartToken":"upgrade-1"}}'
```

Use a unique token for each restart. For reproducible artifacts, pin an image digest;
revision pinning captures configuration, not mutable image tags or Secret contents.
Back up application state before upgrades that migrate its schema.

The external PVC survives replacement and instance deletion. The Operator records
its UID and rejects same-name replacement. Delete it explicitly when its data is
no longer needed. Never attach the same user PVC to multiple independent assistants.
Existing-workspace allocations enforce zero spares even if Group policy changes;
channel consumers and application cron jobs must not run in unassigned replicas.
Suspend stops OpenClaw, including its cron jobs. Resume restarts the process with
retained files; the Operator does not buffer messages or wake it on demand.

## Run the OpenClaw test

```sh
E2E_OPENCLAW=true E2E_TEST='^TestOpenClawLifecycle$' make e2e
```

The driver builds the current Operator, pulls the image selected in this example,
and creates a disposable kind cluster with real agent-sandbox Core/Extensions. It
uses a generated gateway token and deletes the cluster afterward. No existing
cluster is modified and no image is published.

The test starts this actual OpenClaw image, checks HTTP probes and authenticated
health RPC, and creates a disabled scheduler job through OpenClaw's CLI. It checks
the same job ID, application config and retained files after suspend/resume and
explicit execution replacement. It also checks revision pinning, zero workspace
spares and PVC retention after deleting the instance. The replacement changes
configuration while keeping the OpenClaw image version fixed.

This profile uses kind's default container runtime and filesystem storage. It does
not test Kata, external channels, provider onboarding or live model inference.
