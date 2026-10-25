# Cluster capabilities

`ClusterCapabilities` reports runtime, storage and NetworkPolicy
configuration.
Its controller reads cluster resources; AgentGroup and AgentInstance use the results for
deployment checks.

## Reading status

```sh
kubectl get clustercapabilities cluster -o yaml
```

Discovery is always enabled.
The controller creates `ClusterCapabilities/cluster` on startup and recreates it if
deleted.

Existing report spec and metadata remain untouched; discovery only updates status.
The API accepts only the name `cluster`; the controller ignores older reports with other
names.

Each section is audited directly against the API every 90 seconds.
Relevant dependency changes are coalesced and compared through the informer cache; an
unchanged native candidate causes no full section audit or status write.
Optional Cilium node configuration is read directly when a network candidate needs it.
A changed candidate triggers a direct audit of that section.
A manager restart audits every section.

Failed sections retry after 10 seconds, with a separate five-second read budget for each
section.

Cilium node configuration uses direct reads without an optional informer, so its API
can appear after startup.

Check `observedGeneration`, `sections[].lastAttemptAt`, `sections[].lastSuccessAt` and
the corresponding condition:

- `RuntimeDiscoveryReady`
- `StorageDiscoveryReady`
- `NetworkPolicyDiscoveryReady`

Each section can fail independently.
A failed read clears that section and sets its condition False.

`observedAt` records the latest direct attempt across all sections, including failures.
Only a successful direct audit renews a section's `lastSuccessAt`; cache reads and
audits of other sections cannot renew it.

Deployment checks use successful current-generation sections no older than two minutes.
Each condition describes its section's discovery success.
There is no aggregate discovery condition.

The report describes configuration; it does not test runtime execution, storage I/O or
traffic enforcement.

## User-facing capabilities

Start with `status.capabilities` to understand the available configuration families.
The detailed inventories retain exact component names, volume interfaces and
native settings.

| Category | Capability | Meaning |
| --- | --- | --- |
| Isolation | Standard containers | A configured container executor sharing the host kernel |
| Isolation | Application kernel isolation | A userspace application kernel, such as gVisor |
| Isolation | Virtual machine isolation | A separate guest kernel, such as Kata; confidential computing is not verified |
| Isolation | WebAssembly execution | A Wasm/WASI workload contract, including Spin |
| Isolation | Pod user namespaces | Node-reported support for mapping container users to different host users; Pods must opt in |
| Storage | Recursive read-only mounts | Node-reported support for recursive read-only volume mounts; workloads must request them |
| Storage | Filesystem volumes | Directory mounts, regardless of backend protocol or location |
| Storage | Raw block volumes | Administrator-declared raw-device use with PVC `volumeMode: Block` |
| Networking | Pod network policies | Recognized policy-controller configuration; packet enforcement is untested |

Unclassified runtimes and storage remain visible as separate groups.
The node default executor remains Unknown.
A missing group means no corresponding class was classified in the current inventory;
it does not establish that the cluster cannot provide the capability.

A group is `Observed` when at least one referenced option has positive configuration
evidence.
Other options in that group can remain `Unknown` or `Unsupported`.
Read each resource's `status`, `reason` and `source` before selecting it.
For example:

```yaml
status:
  capabilities:
    - name: VirtualMachineIsolation
      displayName: Virtual machine isolation
      category: Isolation
      description: Workloads use a separate guest kernel; VM startup and confidential computing are not verified.
      status: Observed
      resources:
        - kind: RuntimeClass
          namespace: ""
          name: kata-ready
          status: Observed
          source: BuiltInCatalog
        - kind: RuntimeClass
          namespace: ""
          name: kata-without-nodes
          status: Unknown
          source: BuiltInCatalog
          reason: NoEligibleNodes
```

The summary is derived from section inventories without additional API requests.
It does not renew evidence timestamps.
Failed sections cannot retain their previous positive resource references.

## Native observations

| Fields | Evidence and limits |
| --- | --- |
| `defaultRuntime`, `nodeRuntimes` | Default executor stays Unknown; kubelet CRI names and versions do not identify its isolation or image contract. |
| `runtimes` | RuntimeClass classification, scheduling, Pod overhead, candidate-node handler coverage and feature reports. These do not prove that the handler works. |
| `runtimeHandlers` | Handler configurations reported by non-terminating Nodes, including handlers without a RuntimeClass. An empty name denotes the default handler without identifying its executor. |
| `storageClasses` | Provisioner, actual CSI driver identity, declaration/registration flags, volume interface, expansion intent and allowed topologies. Expansion intent does not prove resize support. |
| `csiDrivers`, `csiDriverDetails` | Union of declared and node-registered drivers, with declaration flag, attach requirement, fsGroup policy, volume lifecycle modes and CSINode registration counts/topology keys. Driver-specific node IDs are omitted. Registration does not establish health or optional CSI RPC support. |
| `networkPolicy` | Recognized provider configuration and readiness. `enforcementVerified` stays false even after a separately run traffic test. |

The Operator reads these APIs without touching host sockets or creating probe Pods.
Denied section reads set that section's discovery condition False with reason
`PermissionDenied`.

Global discovery does not query snapshot APIs or inventory snapshot classes.
When a workspace explicitly uses a VolumeSnapshot source, the instance controller
reads that specific snapshot and checks readiness and restore capacity before allocation.
Existing restored workspaces retain their initialization source identity during
suspend/resume.
See [snapshot workspace initialization](usage.md#initialize-from-a-volumesnapshot).

The catalog hash in `catalogRevision` and classification `source`/`rule` fields explain
how a result was derived.

## Discovery catalog

Rules are embedded from
[internal/capabilities/catalog](../internal/capabilities/catalog).
The project maintains the catalog; installation accepts no custom rule language.

`arally.io/runtime-type` and `arally.io/storage-type` labels do not affect recognition.
RuntimeClass declarations and the administrator mappings below classify custom handlers.
Generally applicable installer signatures belong in the catalog with a regression test.

| File | Rules |
| --- | --- |
| `runtimes.yaml` | Exact handler aliases and installer metadata signatures, runtime family, isolation category and image contract |
| `storage.yaml` | Exact provisioners, bounded CSI namespace suffixes, protocol/driver parameters and non-CSI provisioners |
| `network.yaml` | DaemonSet/container/label signatures and policy-controller options |

Go code checks readiness, node placement, driver registration and combines the results.
Parsing rejects unknown fields, duplicate handler/provisioner/DaemonSet signatures and invalid
contracts.

### RuntimeClass recognition

Runtime rules identify runc, crun, youki, Kata, gVisor, crun-vm and Wasm families,
including Spin.
Kata aliases cover QEMU, Cloud Hypervisor, Dragonball, Firecracker, remote and the
documented runtime-rs, GPU and confidential-computing configurations.
Recognizing an alias does not establish GPU availability or hardware attestation.
Unrecognized handlers, including unknown Firecracker/microVM aliases, remain Unknown.

Kata uses the same catalog matcher as other runtimes.
One installer signature matches `app.kubernetes.io/managed-by: kata-deploy` together
with the RuntimeClass selector `katacontainers.io/kata-runtime: "true"`.
This recognizes new installer-managed variants without parsing instance suffixes,
debug variants, versions or VMM names.
Exact aliases continue to recognize older installations without these metadata fields.

A metadata-only match reports `source: InstallerMetadata` and leaves the image
contract Unknown.
An exact handler match supplies its catalog contract when both signals agree.
Conflicting automatic family signals report `RuntimeClassificationConflict`.

Eligible node counts use ready Linux nodes, RuntimeClass selectors and blocking taints;
they do not confirm handler installation or image execution.
Node CRI versions are reported separately.
Handlers named `containerd` or `cri-o` do not identify the underlying executor and
remain unclassified without another classification source.

`handlerEvidence` partitions each class's `eligibleNodes` into:

- `reportedNodes`: candidates whose native inventory includes this handler.
- `missingFromReportedInventoryNodes`: candidates publishing a non-empty inventory
  without this handler.
- `noInventoryNodes`: candidates without an inventory, including older CRI versions.

If all candidates publish inventories and none reports the handler, the class retains
its classification but reports Unknown with `RuntimeHandlerNotReported`.
Missing inventories cannot establish that a runtime is unsupported.
The counts omit Pod-specific affinity, tolerations and resource requirements.

`features.userNamespaces` and `features.recursiveReadOnlyMounts` each count
`reportedSupportedNodes`, `reportedUnsupportedNodes` and `unknownNodes` across the
same candidates.
Feature support can differ between nodes.
A false report can include unavailable CRI feature introspection; it does not establish
that the implementation can never support the feature.
Feature capability summaries retain the exact coverage in the inventory and use
`PartialFeatureCoverage` when only some candidates report support.

### RuntimeClass declarations

An installer or administrator can attach this annotation to an existing RuntimeClass:

```yaml
metadata:
  annotations:
    arally.io/runtime-classification: |
      {"version":"v1","family":"acme-vm","category":"VirtualMachine","imageContract":"Unknown"}
```

This is an AgentRally declaration contract.
All four fields are required; the value must be strict JSON of at most 4096 bytes.
`family` accepts a new name of up to 128 characters without an Operator update.
Category and image-contract values follow the administrator mapping rules below.
Future categories or contracts require an API update.

Precedence is central `runtimeMappings`, an explicit RuntimeClass declaration, then
catalog handler and installer signatures.
Declared results report `source: RuntimeClassDeclared`.
Malformed declarations and unknown versions remain Unknown without falling back to
a familiar-looking handler.
An explicit central mapping can override the declaration.

Keep RuntimeClass write permissions under administrator control.
Declarations describe configuration; they do not install runtimes, override node
evidence or establish workload compatibility.
AgentGroup RuntimeClass allowlists still use the actual selected native class.

Isolation and image compatibility are independent observations.
For example, crun-vm is classified as VirtualMachine with an Unknown image contract;
its VM images are not assumed interchangeable with ordinary Linux application images.

### Storage recognition

Storage classification describes Kubernetes volume interfaces:

- `Filesystem`: directory mounts, including NFS, SMB, CephFS, local-path and
  filesystems backed by cloud disks or SAN volumes.
- `Block`: raw-device use with PVC `volumeMode: Block` and container `volumeDevices`.
- `Unknown`: the interface has not been classified.

The corresponding capability groups are `Filesystem`, `BlockStorage` and
`UnclassifiedStorage`.

A StorageClass does not declare its supported volume modes.
The catalog identifies known directory-mount interfaces; it does not infer raw Block
support from iSCSI, FC, FCoE or another backend protocol.
The same class may also support raw Block volumes.
Administrator mappings can declare that intended use.
The actual PVC's `volumeMode` determines how a workload consumes its volume.

S3 and other bucket-backed CSI mounts are Filesystem interfaces.
This classification does not establish full POSIX semantics.
Direct object API access is outside StorageClass discovery.

Session lifetime is independent of the volume interface.
Local-path remains Filesystem whether its directories are local or shared.
The classification makes no durability, sharing or cross-node availability promise.

The catalog covers AWS, Azure, GCP, Alibaba, Tencent, Huawei, OCI and IBM storage,
alongside Ceph, Longhorn, OpenEBS, OpenStack, Synology and local provisioners.
Synology's iSCSI and NFS mounts share the Filesystem classification.
Azure Files has the same classification for SMB and NFS.
Huawei Everest uses `csi.storage.k8s.io/csi-driver-name` to identify the actual node
driver instead of treating its aggregate provisioner name as a CSI driver.

vSphere, Trident and recognized bucket-mount drivers expose Filesystem interfaces
regardless of the selected backing store.
`kubernetes.io/no-provisioner` remains Unknown because static provisioning does not
identify the eventual PV's interface.
Unrecognized provisioner identities also remain Unknown.
StorageClass names do not determine classification.

`driverDeclared` means a CSIDriver object exists.
`driverRegistered` requires a non-terminating CSINode entry for the actual driver.
The CSIDriver object is optional, so registration can be observed without a declaration.
Known non-CSI provisioners do not require CSI registration.
Recognized CSI classes without node registration remain Unknown.
The report lists all classes without ranking them.

### NetworkPolicy recognition

Network rules cover Calico, Cilium, OVN-Kubernetes, Antrea, Weave, Azure NPM, AWS VPC
CNI, kube-router, Flannel's optional policy sidecar and the standalone Kubernetes
Network Policies controller.
Kube-OVN's CNI component is recognized, but its separate policy-controller configuration
is not inferred from that component alone.
kindnet support depends on its installed implementation/version and remains Unknown
without sufficient evidence.

Checks use provider settings and current-generation DaemonSet readiness.
Official labels together with expected containers recognize renamed installations.
Cilium configuration is read from the agent's mounted ConfigMap, including custom
names and configuration directories.
The official `cilium-dbg build-config` init workflow is also recognized when its
output and the agent's configuration directory resolve to the same shared EmptyDir.
This includes the default ConfigMap source and an explicit single ConfigMap source.
The default node-configuration source is checked through direct, read-only
`CiliumNodeConfig` lists without an optional informer.
Possible node-level policy overrides keep the result Unknown.
Custom source chains, repeated source flags and override filters remain Unknown.
Other writers to the generated directory and alternate API sources also prevent
associating the generated files with this cluster's observed configuration.
Explicit command-line values override environment and ConfigMap values.
The agent must explicitly select its configuration directory.
Unsupported key projections, shadowing mounts, binary ConfigMap data and additional configuration files
remain Unknown rather than borrowing a default from an unrelated mounted directory.
Boolean and string options follow their different argument syntax; later explicit
options take precedence.
Unresolved shell commands or environment-derived configuration remain uncertain.
Policy audit mode is distinguished from policy enforcement configuration.
An absent optional provider ConfigMap does not hide a coexisting configured engine.
Unknown layouts and negative-only observations stay Unknown globally.
Positive evidence means a ready, configured provider was observed.
`enforcementVerified` remains false; test traffic separately.

Network discovery trusts installations in `kube-system`, `calico-system` and
`kube-flannel`.
For another installation namespace, an administrator can explicitly approve its
evidence:

```sh
kubectl label namespace YOUR_CNI_NAMESPACE arally.io/network-discovery=trusted
```

Keep these namespaces and the approval label under administrator control.
DaemonSets with matching names in ordinary tenant namespaces are ignored.
Approval enables the existing catalog checks; it does not verify packet enforcement.

## Administrator mappings

An administrator can classify an existing RuntimeClass or StorageClass on the canonical
report.
For example, merge the following settings into its spec:

```yaml
spec:
  runtimeMappings:
    - runtimeClassName: company-kata
      family: kata
      category: VirtualMachine
      imageContract: LinuxOCI
  storageMappings:
    - storageClassName: company-workspaces
      type: Filesystem
```

Mapped results say `source: AdministratorDeclared`.
Mappings do not create native classes, install handlers, change scheduling, bypass
missing CSI registration or override NetworkPolicy results.
They also preserve native handler inventory mismatches.

A runtime category or image contract can independently be `Unknown`.
Otherwise `WebAssembly` requires `WasmWASI`, and other known categories require `LinuxOCI`.
Each mapping list accepts at most 64 named entries.
Remove a mapping to return to RuntimeClass declaration or automatic classification.
See the [complete report example](../examples/cluster-capabilities.yaml).

## Recognition sources

Catalog entries follow official component identities and configuration contracts:

- [Kubernetes RuntimeClass scheduling](https://kubernetes.io/docs/concepts/containers/runtime-class/#scheduling),
  [Kata runtime classes](https://kata-containers.github.io/kata-containers/kata-deploy-runtimeclasses/)
  and [Spin's runtime handler](https://github.com/spinframework/containerd-shim-spin#quickstart).
- [CSI driver identities](https://kubernetes-csi.github.io/docs/drivers.html),
  [CSIDriver declarations](https://kubernetes-csi.github.io/docs/csi-driver-object.html)
  and [CSINode registration](https://kubernetes-csi.github.io/docs/csi-node-object.html).
- [Kubernetes volume modes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#volume-mode)
  and [S3 bucket mounts](https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md).
- [Synology protocols](https://github.com/SynologyOpenSource/synology-csi#creating-storage-classes),
  [Azure Files parameters](https://github.com/kubernetes-sigs/azurefile-csi-driver/blob/master/docs/driver-parameters.md)
  and [Huawei aggregate storage classes](https://support.huaweicloud.com/intl/en-us/usermanual-cce/cce_10_0380.html).
- [Cilium configuration](https://docs.cilium.io/en/stable/network/kubernetes/configuration/),
  [Cilium's generated configuration and node overrides](https://github.com/cilium/cilium/blob/v1.20.3/cilium-dbg/cmd/build-config.go),
  [AWS policy-agent configuration](https://github.com/aws/amazon-vpc-cni-k8s/blob/master/charts/aws-vpc-cni/templates/daemonset.yaml),
  [kube-router options](https://github.com/cloudnativelabs/kube-router/blob/master/pkg/options/options.go),
  [Flannel's policy sidecar](https://github.com/flannel-io/flannel/blob/master/Documentation/netpol.md)
  and [the standalone upstream policy controller](https://github.com/kubernetes-sigs/kube-network-policies/blob/master/install.yaml).

## Explicit diagnostics

`make e2e` checks the native report and executes a selected runc RuntimeClass in a new
disposable kind cluster.
Optional profiles exercise Cilium application traffic (`E2E_CNI=cilium`) and CSI
workspace snapshot restoration (`E2E_SNAPSHOTS=true`).
See [development tests](development.md#real-lifecycle-release-gate).

These tests use the caller's isolated test credentials, leave the Operator's permissions
unchanged, and do not update discovery status with probe results.

## AgentInstance and group feedback

`AgentInstance.status.conditions[CapabilityValidated]` records deployment checks:

| Status | Result | Effect |
| --- | --- | --- |
| True | References and available classifications checked | Continue deployment |
| False | Missing/incompatible reference or fresh Unsupported network result | Block new, resumed or changed configuration |
| Unknown | Missing, stale or unrecognized observations | Continue, reporting uncertainty |

Runtime checks reject missing/terminating RuntimeClasses and direct conflicts between
RuntimeClass and Pod node selectors.
They do not list nodes or reproduce scheduler decisions.
Unknown classifications, image contracts and reported handler mismatches remain
advisory and are surfaced through `CapabilityValidated=Unknown`.
Specialized image contracts also remain unverified by the Operator.

Kubernetes evaluates affinity, tolerations, node availability and capacity; pending Pods
expose the actual scheduling feedback.
Discovery still reports cluster inventory and advisory node counts.

PVC checks reject missing, terminating or lost external claims and Filesystem/Block
mismatches.
Workspace templates require a positive storage request, native access modes and a
Filesystem volume.
Their StorageClass evidence is checked before allocation; Core provisions the actual
claims.

Pending `WaitForFirstConsumer` claims are accepted.
Binding and PV reclamation follow native Kubernetes settings.
The capability report does not copy `volumeBindingMode` or `reclaimPolicy`.

Suspending an Instance retains its existing PVC.
A StorageClass's `Delete` reclaim policy applies after its claim is deleted, not when
its Pod is suspended.

Static claims without StorageClass objects, and bound claims whose StorageClass was
removed, are accepted with Unknown classification.
AgentRally does not provision or adopt PVCs.

A Group with `spec.networkPolicy` reports `NetworkPolicySupported` separately.
Its `Ready` condition covers Namespace/quota/policy creation.
Fresh Unsupported results block new/resumed workloads; Supported describes provider
configuration, not a successful traffic test.

An existing running Sandbox with unchanged PodTemplate stays running when reports
change.
Suspension remains possible if model/workspace references are missing; that fallback
sets `ConfigurationAccepted=False`.
Deletion skips these checks.

Report changes and periodic reconciliation retry blocked deployments.

AgentInstance reads the owned execution Pod for scheduling and container diagnostics,
with current Core scheduling feedback as a fallback.
`Available` follows current Core readiness, refined by Pod observations; `Ready`
additionally requires the current desired configuration and requested endpoint to be
satisfied.

These are separate from capability discovery and do not turn the report into an
execution or traffic probe.
See [status and diagnostics](usage.md#status-and-diagnostics).

Inventory and condition changes enqueue consumers.
A timestamp-only refresh does not enqueue every AgentInstance or Group; their periodic
reconciliation still checks freshness, so a refreshed unchanged report can unblock a
consumer on its next poll.

Backends can read the same report through [Connector](outbound-connector.md).
instances still specify RuntimeClass, placement, PVCs and Group policy explicitly.
