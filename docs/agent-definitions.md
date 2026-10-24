# Definitions, revisions and instances

An AgentDefinition is reusable deployment configuration.
It selects a built-in `generic`, `assistant` or `coding` environment with `container`
settings, or a namespace-local native `templateRef`.
Security, model configuration and an optional private workspace default also belong to
the definition.

Publishing it creates immutable native SandboxTemplate revisions, without allocating a
Sandbox or Pod.

An AgentInstance is one logical agent identity.
It references a definition in the same namespace and AgentGroup.
It owns lifecycle requests, operating mode, optional HTTP access and any external
workspace binding.

A definition can serve multiple instances; their executions and private workspace PVCs
remain separate.

Instances share the definition's environment and credential references.
Different credential bindings require separate definitions; there are no per-instance
env overrides.

## Create and inspect

Apply the Group and wait for its namespace, then apply the definition before its
instances.
See the [base definition](../examples/base/definition.yaml) and
[instance](../examples/base/agent.yaml).

```sh
kubectl -n arally-demo get agentdefinitions,agentinstances
kubectl explain agentdefinition.spec --recursive
kubectl explain agentinstance.spec --recursive
```

`AgentDefinition.status.currentRevision` is the latest published configuration hash.
`status.templateName` identifies its native SandboxTemplate.

A source template edit publishes a new revision even when the definition's own
generation is unchanged.

Failed publication leaves the last revision available to already pinned instances; new
instances without an explicit revision wait for a current Ready definition.

## Pinning and upgrades

When configuration is first accepted before allocation, an instance records the
definition's name, UID and accepted revision under `status.definition`.
Omitting `spec.revision` selects the latest publication when configuration is accepted.
Subsequent definition or source-template edits do not move existing instances, including
during suspend/resume.

To use a specific existing publication, set `AgentInstance.spec.revision` to its full
64-character hash.

To upgrade an existing instance, publish the definition, then change
`spec.restartToken`.
A restart waits for old execution cleanup before allocating the selected revision.
Without an explicit revision it adopts the latest; with one it uses that version.
Changing `spec.revision` alone requires restart.

Revision pinning captures configuration and native template contents.
It does not freeze Secret values, ConfigMap contents, mutable image tags or storage
contents.
Pin images by digest where needed.
An administrator can enforce this with `AgentGroup.spec.executionPolicy.requireImageDigest`.
The policy applies to regular and init containers in the final resolved Pod.

Credential rotation can require an explicit restart to update creation-time environment
variables.
The Operator never stores fetched Secret contents in revisions or status.

A bound instance cannot switch to another definition identity, even if a deleted
definition is recreated with the same name.
Create a new instance for that change.

## Workspace lifetime

| Workspace | Declaration | Execution replacement / expiry / instance deletion |
| --- | --- | --- |
| Ephemeral | Default assistant/coding emptyDir | Files disappear when the Pod is removed |
| Private PVC | `workspace.volumeClaimTemplate` on instance or definition | Core removes the old Sandbox's PVC; replacement gets a new PVC |
| Retained PVC | `workspace.existingClaimRef` on instance | External PVC remains; replacement mounts the same recorded UID |

Choose exactly one PVC input.
References stay inside the instance namespace and must be filesystem volumes.
An existing workspace must not be owned by an upstream Sandbox.
The Operator reads and records it without adoption or deletion.

Set `workspace.exclusive: true` to reserve an external PVC for one Instance UID.
The reservation survives suspension, expiry and explicit execution replacement.
It is released when the instance finishes deletion.
Other shared or exclusive bindings must respect it.
The Operator uses a coordination Lease and does not modify the PVC.

Missing, terminating or replaced PVCs block allocation.

An accepted UID survives restart; recreating a same-name PVC requires a new instance to
accept the new storage identity.

Existing-workspace allocations enforce zero managed spares.
External named pools must also have zero spares.

Each user assistant needs its own PVC and credentials; do not attach one retained
workspace to multiple independent instances.

The [OpenClaw example](../examples/openclaw/README.md) shows a resident assistant whose
files survive application upgrades.

Application conversations, scheduled jobs and channel identity remain application
concerns.
Suspend stops those jobs.
Optional [activity-driven suspension and wake-up](activity.md) use a trusted caller's Lease.
Callers still create instances explicitly and arrange when to invoke application jobs.

## Delete

Delete instances and wait for execution cleanup before deleting their definition.
The definition finalizer protects referenced revisions and drains its unused managed
capacity, respecting native SandboxClaim references.
Revision templates are garbage-collected with the definition.
There is no separate Revision CRD.
Old publications are retained until definition deletion; there is no automatic
revision-history compaction.

Remove external PVCs explicitly after preserving needed data.

Group deletion waits for definitions, instances, native SandboxClaims, Sandboxes and PVCs before removing the namespace.
Deleting a namespace directly remains a Kubernetes operation that removes its namespaced
resources, including externally managed PVCs and Secrets.
