# Cluster capabilities

`ClusterCapabilities/cluster` reports runtime, storage and network configuration.
The Operator creates the report automatically and updates only its status.
Administrators can add classification overrides to its spec.
Discovery does not run workloads, mount volumes or probe traffic enforcement.

## Reading status

```sh
kubectl get arcap cluster
kubectl get arcap cluster -o yaml
kubectl explain clustercapabilities.status --recursive
```

| Field | Use |
| --- | --- |
| `capabilities` | User-facing capability groups with references to native choices |
| `runtimes`, `runtimeHandlers`, `nodeRuntimes` | RuntimeClass classification, scheduling and node-reported handler evidence |
| `storageClasses`, `csiDriverDetails` | Volume interface and CSI declaration/registration evidence |
| `networkPolicy` | Recognized provider configuration and readiness |
| `platforms`, `extendedResources` | Node-reported OS/CPU architecture and extended resource totals |
| `conditions`, `sections` | Per-domain read success and freshness |

Use a capability's stable `name` for programmatic selection and `displayName` for presentation.
Follow its `resources` references for the underlying class or provider.
A missing capability group means no matching configuration was classified.
`Unknown` means the evidence is insufficient, rather than proving unavailability.

## Freshness

The independent conditions are `RuntimeDiscoveryReady`, `StorageDiscoveryReady` and
`NetworkPolicyDiscoveryReady`.
A True discovery condition means native reads succeeded, even if inventory is empty
or the resulting capability is Unknown.
There is no aggregate discovery condition.

Direct audits run every 90 seconds.
Relevant events compare cached candidates and trigger a new audit when inputs change.
Unchanged cache reads do not renew evidence.
Each section has a five-second read budget; failures clear that section and retry after ten seconds.
Manager restart audits all sections.

Check `observedGeneration`, the section condition and `sections[].lastSuccessAt`.
Deployment checks accept successful current-generation evidence for two minutes.
`observedAt` and `lastAttemptAt` include failed attempts and are not success timestamps.

## User-facing capabilities

| Category | Groups |
| --- | --- |
| Isolation | Standard containers, application kernels, virtual machines, WebAssembly and unclassified runtimes |
| Storage | Filesystem volumes, raw block volumes and unclassified storage |
| Networking | Pod NetworkPolicy configuration |
| Node features | Reported Pod user namespaces and recursive read-only mounts |

Native features retain supported, unsupported and unknown node counts.
Nodes and Pods must still opt into the corresponding feature.
No capability summary establishes hardware attestation, runtime execution, volume durability or packet filtering.

## RuntimeClass recognition

RuntimeClass names do not determine isolation.
Recognition uses exact handler aliases and bounded installer metadata from the
[runtime catalog](../internal/capabilities/catalog/runtimes.yaml).
It covers container, VM and Wasm families, including Kata and gVisor.
Unrecognized handlers remain visible as Unknown.
The node default executor also stays Unknown: a kubelet CRI name does not identify its isolation.

RuntimeClass scheduling, Pod overhead, ready Linux candidate counts and reported
handler coverage are separate evidence.
A node that omits handler inventory does not prove the handler is absent.
A listed handler does not prove it can execute an image.
Feature reports do not replace scheduler placement or admission.

Kata uses the same matcher as other families.
The `kata-deploy` installer label together with its runtime node selector can classify
new installer-managed variants without parsing VMM names or version suffixes.
Metadata-only recognition leaves the image contract Unknown.
Conflicting declarations or handler evidence remain uncertain.

### RuntimeClass declarations

An administrator or installer can annotate an existing RuntimeClass:

```yaml
metadata:
  annotations:
    arally.io/runtime-classification: |
      {"version":"v1","family":"acme-vm","category":"VirtualMachine","imageContract":"Unknown"}
```

All four fields are required.
The value must be strict JSON, at most 4096 bytes, with a family name of at most 128 characters.
Categories are `Container`, `UserSpaceKernel`, `VirtualMachine`, `WebAssembly` or `Unknown`.
Image contracts are `LinuxOCI`, `WasmWASI` or `Unknown`.
A known WebAssembly category requires WasmWASI; other known categories require LinuxOCI,
unless either part is explicitly Unknown.
Invalid declarations and unsupported versions do not fall back to an unrelated catalog match.
Administrator mappings take precedence over this annotation.

## Storage recognition

Classification follows the interface used by Kubernetes:

| Type | Meaning |
| --- | --- |
| `Filesystem` | A directory mount, including NFS, SMB, local-path, disk/SAN filesystems and bucket-backed CSI mounts |
| `Block` | Administrator-declared raw-device use with PVC `volumeMode: Block` |
| `Unknown` | The volume interface has not been classified |

Backend protocols such as FC, FCoE or iSCSI do not imply a raw Block interface.
A StorageClass may support both modes; the actual PVC decides how a workload consumes it.
Bucket mounts do not establish POSIX semantics.
Direct object APIs are outside StorageClass discovery.
Session lifetime, durability, shared access and cross-node availability are separate properties.

The [storage catalog](../internal/capabilities/catalog/storage.yaml) records known
provisioners, bounded namespace suffixes and documented driver-selection parameters.
StorageClass names do not affect classification.
An exact CSIDriver or CSINode match identifies a custom CSI driver without a catalog update.
For an identified CSI driver, one non-empty native `csi.storage.k8s.io/fstype` or legacy
`fstype` parameter can indicate Filesystem intent.
Ambiguous or arbitrary backend parameters do not supply that classification.

`driverDeclared` means a CSIDriver exists.
`driverRegistered` means the actual driver appears in a non-terminating CSINode.
A declaration alone does not establish node registration.
A known CSI class without registration remains Unknown.
Known non-CSI provisioners need no CSI registration.
Aggregate provisioners are associated with their selected driver rather than with a guessed node driver name.

`type` and `status` answer different questions.
A registered custom driver can have `type: Unknown` and `status: Observed`.
Expansion intent, topology, attach requirement, fsGroup policy and CSI volume lifecycle
modes remain native observations; they do not prove health, resize support or storage I/O.
The report does not publish driver-specific node IDs, binding mode or reclaim policy.

Snapshot APIs are checked only when an Instance explicitly references a VolumeSnapshot.
Global discovery does not inventory snapshot classes.
See [workspace initialization](usage.md#initialize-from-a-volumesnapshot).

## NetworkPolicy recognition

The [network catalog](../internal/capabilities/catalog/network.yaml) records provider
identities and policy-controller options.
Recognition combines expected containers or installation labels, effective configuration
and current-generation DaemonSet readiness.
It includes Cilium, Calico, OVN-Kubernetes and other cataloged providers.
An absent or unrecognized CNI remains Unknown.
Negative evidence about one provider does not exclude a coexisting policy engine.

Cilium supports direct ConfigMap mounts and the official generated-configuration path.
Command-line values take precedence over environment and ConfigMap values.
Ambiguous source chains, shadowed files, unresolved commands and possible node overrides
remain Unknown.
Optional Cilium APIs use direct reads so they can appear after Operator startup.
Audit mode is separate from enforcement configuration.

Discovery trusts `kube-system`, `calico-system` and `kube-flannel`.
An administrator can approve another installation namespace:

```sh
kubectl label namespace YOUR_CNI_NAMESPACE arally.io/network-discovery=trusted
```

Protect this label and provider configuration from tenant modification.
Approval permits recognition; it does not verify packets.
`enforcementVerified` remains false, including after separately executed traffic tests.
See [native outbound policy](usage.md#restrict-outbound-traffic).

## Platforms and extended resources

OS and CPU architecture come from non-terminating Nodes' `status.nodeInfo`.
Extended resource names and totals come from native capacity and allocatable declarations.
New vendor resource names need no catalog update.
No image manifest or hardware is probed.

These inventories include unready, cordoned and tainted nodes.
Counts are inventory reports, rather than scheduling candidates.
Allocatable is not current free capacity and does not subtract running Pods.
The fields are informational and reuse the runtime scan without adding admission checks.

## Administrator mappings

Use [full/cluster-capabilities.yaml](../examples/full/cluster-capabilities.yaml) to
classify existing RuntimeClasses and StorageClasses.
Each mapping list accepts at most 64 named entries.
Results use `source: AdministratorDeclared`; removing a mapping restores automatic classification.
The runtime declaration's category/image-contract rules apply to mappings too.

Mappings do not create classes, install components, bypass CSI registration or erase
native handler mismatches.
`source`, `rule` and `catalogRevision` identify how automatic results were derived.
Catalog rules are embedded and maintained with the project; there is no installation-time rule language.

## AgentInstance and group feedback

`CapabilityValidated` reports deployment checks:

| Status | Effect |
| --- | --- |
| True | Continue with the checked references and available evidence |
| False | Block new, resumed or changed configuration for a missing or incompatible reference |
| Unknown | Continue while reporting missing, stale or unclassified evidence |

The Operator rejects missing or terminating RuntimeClasses, direct selector conflicts,
unusable external PVCs and unsupported workspace volume modes.
The scheduler remains responsible for affinity, tolerations and capacity.
Pending WaitForFirstConsumer claims are accepted.
A bound claim can remain usable even if its StorageClass was removed.

A Group's `NetworkPolicySupported` describes provider evidence.
Group Ready covers namespace and policy preparation, rather than filtering packets.
Fresh Unsupported network evidence blocks new or resumed execution.
An unchanged running execution is not stopped merely because the report changes.
Suspension and deletion remain possible when new desired references are unavailable.

Suspend/resume retains an existing PVC.
A StorageClass's Delete reclaim policy applies after PVC deletion, not Pod suspension.
Core owns private execution PVCs; the opt-in persistent-workspace preview creates and
protects a separate platform binding.
See [workspace lifetimes](usage.md#model-and-workspace).

## Verification

[Development](development.md#real-lifecycle-release-gate) documents isolated runtime,
Cilium traffic and CSI snapshot tests.
Results cover the tested versions and paths only.
They do not update discovery with probe results.
The [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/),
[CSI driver](https://kubernetes-csi.github.io/docs/csi-driver-object.html) and
[NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
contracts define the native observations.
