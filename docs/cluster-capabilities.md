# Cluster capabilities

`ClusterCapabilities` reports runtime, storage and NetworkPolicy configuration.
Its controller reads cluster resources; AgentGroup and AgentClaim use the results
for deployment checks.

## Reading status

```sh
kubectl get clustercapabilities cluster -o yaml
```

Discovery is always enabled. The controller creates `ClusterCapabilities/cluster`
on startup and recreates it if deleted. Existing report spec and metadata remain
untouched; discovery only updates status. Only the default report is bootstrapped;
other named reports, if created, are not recreated after deletion.

Scans run every 30 seconds and when dependencies change. Check `observedAt`,
`observedGeneration` and the section's condition:

- `RuntimeDiscoveryReady`
- `StorageDiscoveryReady`
- `NetworkPolicyDiscoveryReady`
- `DiscoveryReady`

Each section can fail independently. A failed read clears that section and sets
its condition False. `observedAt` records the latest attempt, including failures.
Deployment checks use successful current-generation sections no older than two
minutes. The report describes configuration; it does not test runtime execution,
storage I/O or traffic enforcement.

## Discovery catalog

Rules are embedded from [internal/capabilities/catalog](../internal/capabilities/catalog).
The project maintains them; installation accepts no custom catalog or overrides.
`arally.io/runtime-type` and `arally.io/storage-type` labels do not affect recognition.
Add a source rule and regression test, then build a new image to support another provider.

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
AWS VPC CNI and kube-router. Flannel/kindnet signatures indicate lack of support.
Checks use provider settings and current-generation DaemonSet readiness. Unknown
layouts stay Unknown. `enforcementVerified` remains false; test traffic separately.

Network discovery trusts installations in `kube-system`, `calico-system`,
`openshift-ovn-kubernetes`, `openshift-sdn` and `kube-flannel`. For another
installation namespace, an administrator can explicitly approve its evidence:

```sh
kubectl label namespace YOUR_CNI_NAMESPACE arally.io/network-discovery=trusted
```

Keep these namespaces and the approval label under administrator control.
DaemonSets with matching names in ordinary tenant namespaces are ignored.
Approval enables the existing catalog checks; it does not verify packet enforcement.

## AgentClaim and group feedback

`AgentClaim.status.conditions[CapabilityValidated]` records deployment checks:

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

AgentClaim reads the owned execution Pod for scheduling and container diagnostics,
with current Core scheduling feedback as a fallback. `Available` follows current
Core readiness, refined by Pod observations; `Ready` additionally requires the
current desired configuration and requested endpoint to be satisfied. These are
separate from capability discovery and do not turn the report into an execution
or traffic probe.
See [status and diagnostics](usage.md#status-and-diagnostics).

Inventory and condition changes enqueue consumers. A timestamp-only refresh does
not enqueue every AgentClaim or Group; their periodic reconciliation still checks
freshness, so a refreshed unchanged report can unblock a consumer on its next poll.
Backends can read the same report through [Connector](outbound-connector.md).
Claims still specify RuntimeClass, placement, PVCs and Group policy explicitly.
