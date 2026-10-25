# Activity and idle suspension

Idle suspension is optional and disabled by default.
Use it only when a trusted caller or application can report business activity.
HTTP health, low CPU use and lack of network traffic do not prove idle state.

## Configure lifecycle

Add the policy to an AgentInstance:

```yaml
lifecycle:
  idleTimeoutSeconds: 900
  activityLeaseRef:
    name: agent-activity
  maxLifetimeSeconds: 28800
```

Idle timeout and Lease reference must be configured together.
Maximum lifetime is independent and optional.
It counts wall-clock time from validated Sandbox allocation, including suspended time.
The earliest maximum lifetime, absolute shutdown time or applicable completion TTL triggers cleanup.
Activity does not reset a maximum lifetime.

## Caller-managed Lease

After allocation, read the Instance UID and `status.sandboxUID`.
An administrator or trusted caller creates a namespaced Lease with the following shape.
Replace the bracketed values before applying it.

```yaml
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: agent-activity
  namespace: agentrally-demo
  ownerReferences:
  - apiVersion: arally.io/v1alpha1
    kind: AgentInstance
    name: my-agent
    uid: "<AgentInstance UID>"
    controller: true
  annotations:
    arally.io/activity-state: Busy
    arally.io/last-activity-time: "<last business activity, RFC3339Nano>"
spec:
  holderIdentity: "<status.sandboxUID>"
  leaseDurationSeconds: 120
  renewTime: "<current UTC time, RFC3339Nano>"
```

The owner UID fences the logical Instance.
The holder identity fences its current Sandbox execution.
Reusing an old Lease after explicit execution replacement cannot wake the new execution.

`renewTime` is a heartbeat freshness timestamp.
`leaseDurationSeconds` must be between 1 and 300 seconds.
Renew at a bounded interval comfortably shorter than that duration, for example every 30 seconds with a 120-second duration.

The activity annotation must be `Busy` or `Idle`.
`Busy` means at least one request or background task is active.
The caller aggregates concurrent work and reports `Idle` only after all work completes.

The last-activity annotation records the most recent business activity.
Heartbeat renewals alone must not advance it.
It must not regress within an execution or be more than five seconds ahead of the heartbeat or Operator clock.
Activity observations use microsecond precision, matching Kubernetes Lease timestamps.

For `Idle`, the timeout starts at the later of last activity and allocation completion.
For `Busy`, no idle deadline is applied.
Missing, stale, malformed or mismatched signals report an independent ActivityReady condition and do not newly suspend execution.

## Suspension and wake-up

The Operator records idle suspension in `status.activity` without changing user spec.
Core removes the Pod while retaining the Sandbox and workspace.
Fresh activity can wake that automatically suspended execution.
An external caller can renew the Lease while the Pod is absent.
Wait for current execution readiness before sending application traffic after wake-up.
The Operator does not queue or replay requests.

An already idle-suspended execution remains suspended when its signal disappears.
Fresh activity or removal of the idle policy is required to resume it.
Explicit `operatingMode: Suspended` takes precedence over activity.

Automatic wake-up retains the accepted Definition revision and Sandbox UID.
It starts a new process and does not restore process memory.
Explicit restartToken keeps its existing replacement and version-selection behavior.
Maximum lifetime expiry follows the existing Retain/Delete cleanup policy.
A retained, expired Instance needs explicit restart to allocate a new execution.

## Permissions

The signal writer should receive access only to the named activity Lease:

```yaml
rules:
- apiGroups: [coordination.k8s.io]
  resources: [leases]
  resourceNames: [agent-activity]
  verbs: [get, patch, update]
```

Pre-create the Lease before granting these permissions.
Bind the namespaced Role only to the trusted writer.
No AgentInstance status or workspace reservation permissions are needed.

Applications continue to run without Kubernetes API tokens by default.

See [Usage](usage.md) for lifecycle cleanup and storage retention.
