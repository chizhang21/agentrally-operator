# Runtime improvement plan

Make execution isolation, version selection, workspace retention and cleanup predictable.
Keep the existing AgentDefinition, Revision, AgentInstance, Execution and Workspace vocabulary.

AgentDefinition publishes reusable configuration without starting an execution.
AgentInstance retains logical identity while executions can be suspended or replaced.
AgentGroup supplies administrator policy within its dedicated namespace.
Core and Extensions retain ownership of allocation, Pods and private PVCs.

This plan follows the [AgentCore lifecycle](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-lifecycle-settings.html) distinction between logical identity and compute lifetime.
Activity needs an explicit application or caller signal, as described by [AgentCore sessions](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-sessions.html).
AWS timeout defaults are not imposed on this Operator.

## 1. Existing lifecycle reliability

- [x] Use direct API reads for Group deletion guards.
- [x] Cover delayed caches and fail closed on failed resource inventories.
- [x] Report managed pool convergence with an independent CapacityReady condition.
- [x] Preserve Group Ready as namespace and policy readiness.
- [x] Report stalled execution cleanup consistently during restart, expiry and deletion.

Deletion must not infer an empty namespace from a delayed cache.
The guard is an observation, not an atomic transaction with unrelated Kubernetes clients.
Controllers stop new allocations once the Group is terminating.
Administrators must not create new resources concurrently with namespace deletion.

CapacityReady describes managed capacity, including upstream spare readiness.
External pools remain user-managed and do not gate this condition.
Failed pool reconciliation is visible even when namespace policy remains Ready.

Cleanup timeouts report a problem and retain protection.
They do not force-delete Pods, remove foreign finalizers or bypass storage cleanup.

Files: `internal/controller/agentgroup`, `internal/controller/agentinstance`.
Verify: regression tests for delayed caches, pool failures and stalled cleanup.

## 2. Executable isolation, version and workspace contracts

- [x] Add optional Group execution policy for allowed RuntimeClasses and digest-pinned images.
- [x] Validate the resolved Pod, including custom templates and init containers.
- [x] Document digest pinning and the separate behavior of credential rotation.
- [x] Add optional exclusive binding for externally managed workspaces.
- [x] Cover concurrent exclusive acquisitions, retained ownership and cleanup.

`AgentGroup.spec.executionPolicy.allowedRuntimeClasses` requires an explicitly selected class from the administrator's list.
An omitted policy preserves current behavior.
Capability discovery remains advisory; class selection does not attest the underlying runtime.

`AgentGroup.spec.executionPolicy.requireImageDigest` requires content-addressed images for execution containers.
Published revisions continue to retain references without copying credential plaintext.

`AgentInstance.spec.workspace.exclusive` optionally reserves an existing PVC for that Instance UID.
A namespaced coordination Lease records ownership without modifying the PVC.
Acquisition and validation are separate states.
Private access is accepted only after a direct binding inventory has been validated.
A failed inventory read or rollback cannot skip validation on retry.
The reservation survives suspension, expiry and execution replacement.
It is released after instance execution cleanup, before finalizer removal.
Shared bindings must respect an existing reservation.
Private PVC templates already receive separate volumes and need no reservation.

RWO is a node-level constraint, not an instance ownership guarantee.
CSI-supported RWOP can additionally constrain Pod mounts.
See [Kubernetes access modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes).

Files: API types, final Pod validation, workspace binding, generated CRD/RBAC, examples and usage.
Verify: policy bypass rejection, concurrent acquisition, UID replacement and existing retention tests.

## 3. Optional activity-driven lifecycle

- [x] Add an optional maximum execution lifetime anchored at validated allocation.
- [x] Add optional idle suspension using an explicitly referenced activity Lease.
- [x] Fence activity to the Instance UID and current Sandbox UID.
- [x] Keep missing, stale or invalid activity from being mistaken for idle.
- [x] Preserve manual suspension and version binding during automatic wake-up.
- [x] Document a restricted activity-writer contract and test lifecycle transitions.

`lifecycle.maxLifetimeSeconds` counts wall-clock time from allocation completion, including suspension.
It does not reset on activity or Pod recreation within the same execution.
The existing absolute deadline and completion TTL remain supported.
The earliest applicable deadline wins.

`lifecycle.idleTimeoutSeconds` requires `lifecycle.activityLeaseRef`.
Both are omitted by default.
The activity Lease belongs to the Instance and identifies the current Sandbox UID.
Its renewal time and duration establish signal freshness.
Explicit activity state and last-activity time distinguish Busy, Idle and heartbeat health.

The caller aggregates concurrent work and renews the Lease at a bounded rate.
The Operator does not write one Kubernetes object per application request.
Untrusted activity cannot extend the maximum execution lifetime.

Idle suspension retains the Sandbox and its workspace.
Fresh activity can wake an automatically suspended execution.
An explicit `operatingMode: Suspended` always takes precedence.
Automatic wake-up does not change restartToken or clear the accepted Revision.
Process memory is not restored.

Files: lifecycle API and evaluation, activity observation, effective mode, watches and diagnostics.
Verify: busy/idle/stale/future signals, ownership fencing, manual suspension, revision preservation and controller restart.

## 4. Capacity cost and performance evidence

- [x] Add an optional total managed-spare budget while preserving per-pool targets.
- [x] Reduce spare targets before increasing others, and leave external pools untouched.
- [x] Ignore activity-only member updates when scheduling Group inventory and pruning.
- [x] Measure cold, warm and resume latency with existing status timestamps and metrics.
- [x] Add an explicit real-RuntimeClass E2E profile suitable for a Kata-enabled disposable cluster.
- [x] Run isolated E2E, record evidence and distinguish any unavailable Kata validation.

`prewarming.maxTotalReplicas` caps desired managed spares across resolved configurations.
New pools start at zero when the total budget is enabled.
Group reconciliation assigns the budget deterministically.
Definition retirement only drains its own capacity and does not rebalance sibling pools.
Terminating pools retain their target budget until foreground cleanup completes.
Native ResourceQuota remains the hard resource-admission boundary.

Definition publication never implicitly starts applications.
First use of an unseen configuration can still be cold.
Existing-PVC configurations continue to enforce zero spares.

Use the existing allocation, startup, suspension and cleanup histograms.
Record observed P50/P95 separately from application transaction latency.
Optimize the existing allocation path only when measurements identify a bottleneck.

A real Kata profile must use an installed runtime and actual runtime-backed Pods.
A renamed runc RuntimeClass is not Kata evidence.
Initial acceptance uses disposable clusters.
A live Kata run requires a separately confirmed target and deployment authorization.

Files: prewarming policy, pool reconciliation, E2E runner and development documentation.
Verify: deterministic budget tests, envtest, isolated lifecycle E2E and optional hardware-backed runtime profile.

## Repository housekeeping

- [x] Remove the change-history and context files and their links.
- [x] Keep local AGENT.md instructions out of version control.
- [x] Preserve existing documentation edits and use separate sentences and paragraphs for different meanings.
- [x] Run generation, source checks, lint, tests and applicable integration/E2E checks.

Git publication and live deployment require separate user instructions.

## Validation record

Validation completed on 2026-10-24.

| Layer | Command or profile | Result |
| --- | --- | --- |
| Source, generation and packaging | `make check lint` | Passed; zero lint issues |
| Concurrency | `go test -race ./internal/controller/agentinstance ./internal/controller/agentgroup ./internal/allocation` | Passed |
| Real API integration | `go test -tags=integration ./cmd ./tests/packaging -count=1 -timeout=180s` with envtest binaries | Passed |
| Focused runtime E2E | Retained exclusive workspace, activity lifecycle and total spare budget | 3 passed in 454.509 seconds |
| Complete E2E | `E2E_CNI=cilium E2E_SNAPSHOTS=true E2E_OPENCLAW=true make e2e` | 11 passed, 1 Kata-specific skip, in 1569.947 seconds |
| Real Kata runtime | `TestKataRuntimeLifecycle` on a separately authorized existing cluster | Passed in 348.283 seconds |

Both E2E runs used the matching local image through `E2E_IMAGE=arally-operator:runtime-plan-final`.
Its image ID was `sha256:02ca5dc4f9380e95368b3253c1999250203130639924dbd80fb12dff8091de69`.
The 207 runtime, test and Chart source files remained unchanged throughout final acceptance.
Both disposable clusters were deleted after their runs.
Those initial isolated runs did not publish images or modify shared clusters.

The complete profile exercised real Core and Extensions controllers, Cilium traffic enforcement, CSI snapshot recovery and OpenClaw lifecycle behavior.
It also covered definition pinning, dependency recovery, exclusive workspace contention, automatic idle suspension, wake-up, manual suspension, maximum lifetime and managed-spare budgeting.
Regression tests cover activity timestamp serialization and workspace reservations disappearing or failing validation during acquisition.

Observed complete-profile readiness measurements:

| Operation | Samples | P50 | P95 |
| --- | --- | --- | --- |
| Cold creation to readiness | 1 | 7.79 s | 7.79 s |
| Warm creation to readiness | 2 | 1.86 s | 1.88 s |
| Resume to readiness | 3 | 2.01 s | 2.01 s |

These are nearest-rank smoke measurements in disposable kind, not production SLOs or application transaction latency.
Readiness polling and Kubernetes timestamp precision limit the observations.

## Existing-cluster Kata acceptance

After a separately authorized Operator upgrade, the Kata profile passed on an existing Kubernetes cluster on 2026-10-24.
It used the installed `kata-qemu-runtime-rs` handler and a temporary local-path workspace.
The workload reported guest kernel `6.18.35`; the host reported `6.12.43+deb13-amd64`.

The test verified execution startup, suspension, workspace data retention on resume, explicit replacement and final reclamation.
A controlled Pod finalizer held cleanup beyond five minutes to verify the stalled condition.
Removing that test finalizer allowed normal cleanup to complete.
The temporary Group, namespace and persistent volume were removed.

The requested agent remained suspended with its original external PVC UID and Bound state.
Other existing agents remained Ready and Available with unchanged Pod UIDs.
This validates the exercised Kata lifecycle paths and storage configuration; it is not a general isolation attestation.
