# Migrating from Activity Leases

This version retires `spec.lifecycle.idleTimeoutSeconds`,
`spec.lifecycle.activityLeaseRef` and `status.activity`.
The Operator now follows explicit `spec.operatingMode` only.
Reservation Leases and leader election continue to work.

New installations need no migration.
Existing installations must migrate before applying the new CRD schema.
The migration tool must read the old fields while the old schema still serves them.

## Preserve existing intent

The tool freezes each instance's effective intent:

- Explicitly suspended instances remain Suspended.
- Instances suspended by the old activity observer become explicitly Suspended.
- Other instances preserve Running, including when that value was defaulted.

It removes the two retired lifecycle fields in the same update.
It does not delete PVCs or Leases.
Existing idle suspension and automatic wake-up policies must be disabled in the
calling platform until a supported replacement is available.

## Upgrade order

First run the read-only plan, naming the target context and Operator Deployment:

```sh
python3 hack/migrate_activity.py --context <context> \
  --namespace <operator-namespace> --deployment <operator-deployment>
```

Pause every external writer that can change instances or activity signals.
Pause deployment automation that could restart the old Operator.
Scale the old Operator Deployment to zero and allow its Pods to disappear.

Apply the migration while the old CRDs are still installed:

```sh
python3 hack/migrate_activity.py --context <context> \
  --namespace <operator-namespace> --deployment <operator-deployment> \
  --apply --receipt /path/to/private/activity-receipt.jsonl
```

The tool refuses to proceed if the Deployment has not observed scale-down,
an owned ReplicaSet still requests Pods, or an old Pod remains, including terminating Pods.
Each object update requires its current UID and resourceVersion.
The append-only receipt is written before the update and again after confirmation.
Use an existing private directory outside the repository and retain the receipt through the upgrade.
The file requires mode 0600 and excludes Secret and Pod contents.

After every instance has migrated, repeat the dry run and inspect the result.
Then upgrade the CRDs and Operator together using the normal release procedure.
Confirm explicit suspension and resumption before resuming external writers.
This repository's tests do not establish that an Orchestrator deployment is ready
for this change; consumer migration is a separate rollout requirement.

## Interrupted migrations

Any conflict or failed receipt write stops the migration.
Do not upgrade the CRDs while migration is incomplete.
Inspect the receipt and current objects, then repeat the tool.

The atomic `arally.io/activity-migrated: "true"` marker prevents a retry from
overwriting later administrator intent using stale activity status.
Reintroduced legacy fields on a marked object require investigation.
The tool does not automatically roll back successful updates.

Old activity Leases may remain after migration.
Remove them separately only after verifying their identities and consumers.
Do not delete coordination Leases by resource type; other Leases are still required.

Before schema upgrade, the old Operator can be restarted with the frozen explicit modes.
After schema upgrade, use a forward fix rather than directly rolling back to an
Activity Lease version.
Re-enabling the old policy requires a separately reviewed migration.
