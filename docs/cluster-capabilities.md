# Cluster capabilities

`ClusterCapabilities` reports runtime, storage, snapshot class and NetworkPolicy configuration.
Its controller reads cluster resources; AgentGroup and AgentInstance use the results
for deployment checks.

## Reading status

```sh
kubectl get clustercapabilities cluster -o yaml
```

Discovery is always enabled. The controller creates `ClusterCapabilities/cluster`
on startup and recreates it if deleted. Existing report spec and metadata remain
untouched; discovery only updates status. The API accepts only the name `cluster`;
the controller ignores older reports with other names.

Each section is audited directly against the API every 90 seconds. Relevant
dependency changes are coalesced and compared through the informer cache; an
unchanged candidate causes no API scan or status write. A changed candidate
triggers a direct audit of that section. A manager restart audits every section.
Failed sections retry after 10 seconds, with a separate five-second read budget
for each section. Snapshot classes use periodic direct reads without an informer,
so the optional API can appear after startup.

Check `observedGeneration`, `sections[].lastAttemptAt`,
`sections[].lastSuccessAt` and the corresponding condition:

- `RuntimeDiscoveryReady`
- `StorageDiscoveryReady`
- `NetworkPolicyDiscoveryReady`
- `SnapshotDiscoveryReady`
- `DiscoveryReady`

Each section can fail independently. A failed read clears that section and sets
its condition False. `observedAt` records the latest direct attempt across all
sections, including failures. Only a successful direct audit renews a section's
`lastSuccessAt`; cache reads and audits of other sections cannot renew it.
Deployment checks use successful current-generation sections no older than two
minutes. `DiscoveryReady=True` means every section was read successfully; a
missing optional snapshot class API is a valid observation, not an installation
failure. The report describes configuration; it does not test runtime execution,
storage I/O or traffic enforcement.

## Native observations

| Fields | Evidence and limits |
| --- | --- |
| `defaultRuntime`, `nodeRuntimes` | Default executor stays Unknown; kubelet CRI names and versions do not identify its isolation or image contract. |
| `runtimes` | RuntimeClass handler, scheduling selectors/tolerations, fixed Pod overhead and advisory eligible node count. These do not prove that the handler works. |
| `storageClasses` | Provisioner, binding mode, reclaim policy, expansion intent, allowed topologies and backend classification. Expansion intent does not prove resize support. |
| `csiDrivers`, `csiDriverDetails` | Driver names, attach requirement, fsGroup policy, volume lifecycle modes and CSINode registration counts/topology keys. Driver-specific node IDs are omitted. Registration does not establish health or optional CSI RPC support. |
| `snapshots.classAPIStatus`, `snapshots.classes` | VolumeSnapshotClass API availability, class driver, deletion policy and default annotation. These do not prove snapshot creation/restore; pre-provisioned snapshots may have no class. |
| `networkPolicy` | Recognized provider configuration and readiness. `enforcementVerified` stays false even after a separately run traffic test. |

The Operator reads these APIs without touching host sockets or creating probe
Pods. Missing snapshot APIs produce `Unavailable`; denied reads produce `Unknown`
and a `PermissionDenied` condition. The catalog hash in `catalogRevision` and
classification `source`/`rule` fields explain how a result was derived.

## Discovery catalog

Rules are embedded from [internal/capabilities/catalog](../internal/capabilities/catalog).
The project maintains the catalog; installation accepts no custom rule language.
`arally.io/runtime-type` and `arally.io/storage-type` labels do not affect recognition.
Use the limited administrator mappings below for custom class names, or add a
source rule and regression test for a generally applicable provider signature.

| File | Rules |
| --- | --- |
| `runtimes.yaml` | Exact handler aliases, runtime family, isolation category and image contract |
| `storage.yaml` | Ordered provisioner signatures, protocols and legacy NFS exceptions |
| `network.yaml` | DaemonSet/container signatures and provider configuration flags |

Go code checks readiness, node placement, driver registration and combines the
results. Parsing rejects unknown fields, duplicate handler/DaemonSet signatures
and invalid contracts.

Runtime rules identify container, Kata, gVisor and Wasm families. Unrecognized
handlers, including unknown Firecracker/microVM aliases, remain Unknown. Eligible
node counts use ready Linux nodes, RuntimeClass selectors and blocking taints;
they do not confirm handler installation or image execution. Node CRI versions
are reported separately.

Storage is classified as NFS, Block, Local or Unknown. Hostpath stays Local.
Known legacy NFS provisioners can appear without CSIDriver; recognized remote CSI
backends require driver registration. The report lists all classes without ranking them.

Network rules cover Calico, Cilium, OVN-Kubernetes, Antrea, Weave, Azure NPM,
AWS VPC CNI and kube-router. Flannel/kindnet signatures describe only those
components' lack of support; they cannot rule out a coexisting policy engine.
Checks use provider settings and current-generation DaemonSet readiness. Unknown
layouts and negative-only observations stay Unknown globally. Positive evidence
means a ready, configured provider was observed. `enforcementVerified` remains
false; test traffic separately.

Network discovery trusts installations in `kube-system`, `calico-system`,
`openshift-ovn-kubernetes`, `openshift-sdn` and `kube-flannel`. For another
installation namespace, an administrator can explicitly approve its evidence:

```sh
kubectl label namespace YOUR_CNI_NAMESPACE arally.io/network-discovery=trusted
```

Keep these namespaces and the approval label under administrator control.
DaemonSets with matching names in ordinary tenant namespaces are ignored.
Approval enables the existing catalog checks; it does not verify packet enforcement.

## Administrator mappings

An administrator can classify an existing RuntimeClass or StorageClass on the
canonical report. For example, merge the following settings into its spec:

```yaml
spec:
  runtimeMappings:
    - runtimeClassName: company-kata
      family: kata
      category: VirtualMachine
      imageContract: LinuxOCI
  storageMappings:
    - storageClassName: company-workspaces
      type: NFS
```

Mapped results say `source: AdministratorDeclared`. Mappings do not create native
classes, install handlers, change scheduling, bypass missing CSI registration or
override NetworkPolicy results. A runtime category of `WebAssembly` requires
`WasmWASI`; other categories require `LinuxOCI`. Each mapping list accepts at
most 64 named entries. Remove a mapping to return to built-in/native classification.
See the [complete report example](../examples/cluster-capabilities.yaml).

## Explicit diagnostics

`make e2e` checks the native report and executes a selected runc RuntimeClass in
a new disposable kind cluster. Optional profiles exercise Cilium application
traffic (`E2E_CNI=cilium`) and CSI workspace snapshot restoration
(`E2E_SNAPSHOTS=true`). See [development tests](development.md#real-lifecycle-release-gate).
These tests use the caller's isolated test credentials, leave the Operator's
permissions unchanged, and do not update discovery status with probe results.

## AgentInstance and group feedback

`AgentInstance.status.conditions[CapabilityValidated]` records deployment checks:

| Status | Result | Effect |
| --- | --- | --- |
| True | References and available classifications checked | Continue deployment |
| False | Missing/incompatible reference or fresh Unsupported network result | Block new, resumed or changed configuration |
| Unknown | Missing, stale or unrecognized observations | Continue, reporting uncertainty |

Runtime checks reject missing/terminating RuntimeClasses and direct conflicts
between RuntimeClass and Pod node selectors. They do not list nodes or reproduce
scheduler decisions. Kubernetes evaluates affinity, tolerations, node availability
and capacity; pending Pods expose the actual scheduling feedback. Discovery still
reports cluster inventory and advisory node counts.

PVC checks reject missing, terminating or lost external claims and Filesystem/Block
mismatches. Workspace templates require a positive storage request, native access
modes and a Filesystem volume. Their StorageClass evidence is checked before
allocation; Core provisions the actual claims. Pending `WaitForFirstConsumer` claims
are accepted. Static claims without StorageClass objects, and bound claims whose
StorageClass was removed, are accepted with Unknown classification. Arally does
not provision or adopt PVCs.

A Group with `spec.networkPolicy` reports `NetworkPolicySupported` separately.
Its `Ready` condition covers Namespace/quota/policy creation. Fresh Unsupported
results block new/resumed workloads; Supported describes provider configuration,
not a successful traffic test.

An existing running Sandbox with unchanged PodTemplate stays running when reports
change. Suspension remains possible if model/workspace references are missing;
that fallback sets `ConfigurationAccepted=False`. Deletion skips these checks.
Report changes and periodic reconciliation retry blocked deployments.

AgentInstance reads the owned execution Pod for scheduling and container diagnostics,
with current Core scheduling feedback as a fallback. `Available` follows current
Core readiness, refined by Pod observations; `Ready` additionally requires the
current desired configuration and requested endpoint to be satisfied. These are
separate from capability discovery and do not turn the report into an execution
or traffic probe.
See [status and diagnostics](usage.md#status-and-diagnostics).

Inventory and condition changes enqueue consumers. A timestamp-only refresh does
not enqueue every AgentInstance or Group; their periodic reconciliation still checks
freshness, so a refreshed unchanged report can unblock a consumer on its next poll.
Backends can read the same report through [Connector](outbound-connector.md).
instances still specify RuntimeClass, placement, PVCs and Group policy explicitly.
