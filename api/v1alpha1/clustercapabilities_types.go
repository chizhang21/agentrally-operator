package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeObservation is configuration evidence, not a successful runtime probe.
type RuntimeObservation struct {
	Name string `json:"name"`
	// +optional
	Handler       string `json:"handler,omitempty"`
	Family        string `json:"family"`
	Category      string `json:"category"`
	ImageContract string `json:"imageContract"`
	EligibleNodes int32  `json:"eligibleNodes"`
	// HandlerEvidence covers the existing scheduling candidates, not Pod startup.
	// Missing node inventory is unknown rather than evidence of no support.
	// +optional
	HandlerEvidence *RuntimeHandlerEvidence `json:"handlerEvidence,omitempty"`
	// Features counts node reports across the same scheduling candidates.
	// +optional
	Features *RuntimeFeaturesObservation `json:"features,omitempty"`
	// +kubebuilder:validation:Enum=Observed;Unknown
	Status string `json:"status"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// Source identifies classification evidence, not a successful runtime probe.
	// +kubebuilder:validation:Enum=NativeAPI;BuiltInCatalog;AdministratorDeclared;RuntimeClassDeclared;InstallerMetadata
	// +optional
	Source string `json:"source,omitempty"`
	// Rule identifies the built-in rule or administrator mapping.
	// +optional
	Rule string `json:"rule,omitempty"`
	// +optional
	Scheduling *nodev1.Scheduling `json:"scheduling,omitempty"`
	// Overhead is the RuntimeClass fixed Pod overhead, not measured consumption.
	// +optional
	Overhead corev1.ResourceList `json:"overhead,omitempty"`
}

// RuntimeHandlerEvidence partitions eligibleNodes by native handler inventory.
// A reported handler is configured evidence, not a successful execution probe.
type RuntimeHandlerEvidence struct {
	// +kubebuilder:validation:Minimum=0
	ReportedNodes int32 `json:"reportedNodes"`
	// +kubebuilder:validation:Minimum=0
	MissingFromReportedInventoryNodes int32 `json:"missingFromReportedInventoryNodes"`
	// +kubebuilder:validation:Minimum=0
	NoInventoryNodes int32 `json:"noInventoryNodes"`
}

// RuntimeFeatureObservation retains coverage instead of implying uniform support.
// Reported false may include unavailable CRI feature introspection.
type RuntimeFeatureObservation struct {
	// +kubebuilder:validation:Minimum=0
	ReportedSupportedNodes int32 `json:"reportedSupportedNodes"`
	// +kubebuilder:validation:Minimum=0
	ReportedUnsupportedNodes int32 `json:"reportedUnsupportedNodes"`
	// +kubebuilder:validation:Minimum=0
	UnknownNodes int32 `json:"unknownNodes"`
}

type RuntimeFeaturesObservation struct {
	UserNamespaces          RuntimeFeatureObservation `json:"userNamespaces"`
	RecursiveReadOnlyMounts RuntimeFeatureObservation `json:"recursiveReadOnlyMounts"`
}

// NodeRuntimeHandlerObservation inventories reports from non-terminating nodes.
// A handler may have no RuntimeClass and therefore not be selectable by a Pod.
type NodeRuntimeHandlerObservation struct {
	// Name is empty for the default handler, whose executor remains unidentified.
	Name string `json:"name"`
	// +kubebuilder:validation:Minimum=0
	ReportedNodes int32                      `json:"reportedNodes"`
	Features      RuntimeFeaturesObservation `json:"features"`
}

// NodeRuntimeObservation reports kubelet CRI versions without inferring executors.
type NodeRuntimeObservation struct {
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
	Nodes          int32  `json:"nodes"`
}

// NodePlatformObservation groups non-terminating nodes by their reported platform.
// Counts include unready and unschedulable nodes, without asserting image compatibility.
type NodePlatformObservation struct {
	// OS is status.nodeInfo.operatingSystem; empty means unreported.
	OS string `json:"os"`
	// Architecture is status.nodeInfo.architecture; empty means unreported.
	Architecture string `json:"architecture"`
	// +kubebuilder:validation:Minimum=0
	Nodes int32 `json:"nodes"`
}

// NodeResourceObservation aggregates a native extended resource across non-terminating
// nodes. Resource names are retained without inferring device type or runtime support.
type NodeResourceObservation struct {
	Name corev1.ResourceName `json:"name"`
	// Nodes counts reports in capacity or allocatable, including zero quantities.
	// +kubebuilder:validation:Minimum=0
	Nodes int32 `json:"nodes"`
	// Capacity sums reported capacity; missing values contribute zero.
	Capacity resource.Quantity `json:"capacity"`
	// Allocatable sums reported allocatable, including allocated resources and reports
	// from unready or unschedulable nodes. It is not current free capacity.
	// Missing values contribute zero; capacity is not used as a fallback.
	Allocatable resource.Quantity `json:"allocatable"`
}

type StorageObservation struct {
	Name        string `json:"name"`
	Provisioner string `json:"provisioner"`
	// CSIDriver identifies the node driver, which may differ from an aggregate
	// provisioner. Empty for non-CSI or unrecognized driver selection.
	// +optional
	CSIDriver string `json:"csiDriver,omitempty"`
	// Type classifies a known Kubernetes volume interface, not its backing store.
	// Catalog entries and CSI filesystem parameters describe directory mounts;
	// the same class may also support raw Block volumes. Actual use follows the
	// PVC's volumeMode. Unknown type can coexist with Observed registration.
	// +kubebuilder:validation:Enum=Filesystem;Block;Unknown
	Type    string `json:"type"`
	Source  string `json:"source"`
	Default bool   `json:"default"`
	// DriverDeclared reports a CSIDriver object; it is not node registration.
	// +optional
	DriverDeclared bool `json:"driverDeclared,omitempty"`
	// DriverRegistered requires a non-terminating CSINode registration.
	DriverRegistered bool `json:"driverRegistered"`
	// Status reports native driver registration or recognized non-CSI configuration,
	// independently of Type. Observed does not establish driver health or volume I/O.
	// +kubebuilder:validation:Enum=Observed;Unknown
	Status string `json:"status"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Rule string `json:"rule,omitempty"`
	// AllowVolumeExpansion is StorageClass intent, not a successful resize test.
	// +optional
	AllowVolumeExpansion *bool `json:"allowVolumeExpansion,omitempty"`
	// +optional
	AllowedTopologies []corev1.TopologySelectorTerm `json:"allowedTopologies,omitempty"`
}

// CSIDriverObservation reports native driver behavior and node registration.
// Registration is not proof of current driver health, I/O or optional CSI RPCs.
type CSIDriverObservation struct {
	Name string `json:"name"`
	// Declared distinguishes a CSIDriver object from node-only registration.
	// +optional
	Declared bool `json:"declared,omitempty"`
	// +optional
	AttachRequired *bool `json:"attachRequired,omitempty"`
	// +optional
	FSGroupPolicy *storagev1.FSGroupPolicy `json:"fsGroupPolicy,omitempty"`
	// +listType=set
	// +optional
	VolumeLifecycleModes []storagev1.VolumeLifecycleMode `json:"volumeLifecycleModes,omitempty"`
	RegisteredNodes      int32                           `json:"registeredNodes"`
	// TopologyKeys contains key names, without publishing driver-specific node IDs.
	// +listType=set
	// +optional
	TopologyKeys []string `json:"topologyKeys,omitempty"`
}

type NetworkPolicyProviderObservation struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Provider  string `json:"provider"`
	// Supported means observed ready configuration, not tested packet enforcement.
	// +kubebuilder:validation:Enum=Supported;Unsupported;Unknown
	Status string `json:"status"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// Source and Rule identify a versioned configuration signature.
	// +optional
	Source string `json:"source,omitempty"`
	// +optional
	Rule string `json:"rule,omitempty"`
}

type NetworkPolicyObservation struct {
	// +kubebuilder:validation:Enum=Supported;Unsupported;Unknown
	Status string `json:"status"`
	Reason string `json:"reason"`
	// EnforcementVerified remains false; discovery does not run traffic probes.
	EnforcementVerified bool `json:"enforcementVerified"`
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	// +optional
	Providers []NetworkPolicyProviderObservation `json:"providers,omitempty"`
}

// RuntimeMapping classifies one existing RuntimeClass. Administrator declarations
// do not verify the handler, alter scheduling or bypass native reference checks.
// +kubebuilder:validation:XValidation:rule="self.category == 'Unknown' || self.imageContract == 'Unknown' || ((self.category == 'WebAssembly') == (self.imageContract == 'WasmWASI'))",message="Known WebAssembly contracts require WasmWASI; known non-WebAssembly contracts require LinuxOCI"
type RuntimeMapping struct {
	// +kubebuilder:validation:MinLength=1
	RuntimeClassName string `json:"runtimeClassName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Family string `json:"family"`
	// +kubebuilder:validation:Enum=Container;UserSpaceKernel;VirtualMachine;WebAssembly;Unknown
	Category string `json:"category"`
	// +kubebuilder:validation:Enum=LinuxOCI;WasmWASI;Unknown
	ImageContract string `json:"imageContract"`
}

type StorageMapping struct {
	// +kubebuilder:validation:MinLength=1
	StorageClassName string `json:"storageClassName"`
	// Type declares the intended Kubernetes volume interface for this class.
	// It does not change PVC volumeMode or native driver support.
	// +kubebuilder:validation:Enum=Filesystem;Block;Unknown
	Type string `json:"type"`
}

// CapabilityReference links a user-facing capability to its native evidence.
type CapabilityReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Namespace is empty for cluster-scoped resources.
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:Enum=Observed;Unknown;Unsupported
	Status string `json:"status"`
	// +kubebuilder:validation:Enum=NativeAPI;BuiltInCatalog;AdministratorDeclared;RuntimeClassDeclared;InstallerMetadata
	// +optional
	Source string `json:"source,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// CapabilityObservation groups configuration evidence into user-facing concepts.
// Observed means at least one reference has positive configuration evidence.
// It does not attest execution, storage I/O or traffic enforcement.
type CapabilityObservation struct {
	// Name is the capability identifier used for programmatic selection.
	Name string `json:"name"`
	// DisplayName is the human-readable capability label.
	DisplayName string `json:"displayName"`
	// +kubebuilder:validation:Enum=Isolation;Storage;Networking
	Category string `json:"category"`
	// Description explains the capability and limits of its configuration evidence.
	Description string `json:"description"`
	// Status is Observed when at least one resource has positive configuration
	// evidence. Unknown does not mean the capability is unavailable.
	// +kubebuilder:validation:Enum=Observed;Unknown
	Status string `json:"status"`
	// Resources identifies native choices and retains each choice's uncertainty.
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	// +optional
	Resources []CapabilityReference `json:"resources,omitempty"`
}

// ClusterCapabilitiesSpec contains administrator-supplied classification overrides.
// Discovery preserves this input and publishes its results through status.
type ClusterCapabilitiesSpec struct {
	// RuntimeMappings apply only to the named classes. Missing classes are not invented.
	// +listType=map
	// +listMapKey=runtimeClassName
	// +kubebuilder:validation:MaxItems=64
	// +optional
	RuntimeMappings []RuntimeMapping `json:"runtimeMappings,omitempty"`
	// StorageMappings classifies named StorageClasses without changing their
	// provisioning behavior or proving that a volume can be mounted.
	// +listType=map
	// +listMapKey=storageClassName
	// +kubebuilder:validation:MaxItems=64
	// +optional
	StorageMappings []StorageMapping `json:"storageMappings,omitempty"`
}

type DiscoverySectionObservation struct {
	// Name is the corresponding discovery condition type.
	Name string `json:"name"`
	// LastAttemptAt records a direct API audit, including failures.
	// +optional
	LastAttemptAt *metav1.Time `json:"lastAttemptAt,omitempty"`
	// LastSuccessAt advances only after a successful direct API audit. Reading
	// informer cache or refreshing another section cannot renew this evidence.
	// +optional
	LastSuccessAt *metav1.Time `json:"lastSuccessAt,omitempty"`
}

// ClusterCapabilitiesStatus contains the latest passive discovery observations.
// Read capabilities for the user-facing summary and the inventories for native
// choices. Conditions and section timestamps describe evidence freshness.
type ClusterCapabilitiesStatus struct {
	// ObservedGeneration identifies the spec generation processed by discovery.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedAt is the completion time of the last scan attempt, including failures.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// CatalogRevision identifies the embedded classification rules.
	// +optional
	CatalogRevision string `json:"catalogRevision,omitempty"`
	// Capabilities is the entry point for reading user-facing configuration families.
	// It is derived from the inventories below; consult the corresponding section
	// condition and lastSuccessAt before relying on its evidence.
	// +listType=map
	// +listMapKey=name
	// +optional
	Capabilities []CapabilityObservation `json:"capabilities,omitempty"`
	// Sections records successful and attempted API audits independently per domain.
	// +listType=map
	// +listMapKey=name
	// +optional
	Sections []DiscoverySectionObservation `json:"sections,omitempty"`
	// DefaultRuntime describes the node default without inferring its executor.
	// +optional
	DefaultRuntime *RuntimeObservation `json:"defaultRuntime,omitempty"`
	// Runtimes inventories selectable RuntimeClasses and their configuration evidence.
	// +listType=map
	// +listMapKey=name
	// +optional
	Runtimes []RuntimeObservation `json:"runtimes,omitempty"`
	// NodeRuntimes lists reported CRI implementations and versions, not isolation types.
	// +listType=map
	// +listMapKey=implementation
	// +listMapKey=version
	// +optional
	NodeRuntimes []NodeRuntimeObservation `json:"nodeRuntimes,omitempty"`
	// Platforms groups node-reported OS/CPU architecture, with RuntimeDiscoveryReady
	// freshness. It does not establish scheduling eligibility or image compatibility.
	// +listType=map
	// +listMapKey=os
	// +listMapKey=architecture
	// +optional
	Platforms []NodePlatformObservation `json:"platforms,omitempty"`
	// ExtendedResources inventories node-advertised resources such as GPUs, with
	// RuntimeDiscoveryReady freshness. Totals are not current free capacity and
	// do not establish compatibility with a selected RuntimeClass.
	// +listType=map
	// +listMapKey=name
	// +optional
	ExtendedResources []NodeResourceObservation `json:"extendedResources,omitempty"`
	// RuntimeHandlers includes node-reported configurations without inventing classes.
	// +listType=map
	// +listMapKey=name
	// +optional
	RuntimeHandlers []NodeRuntimeHandlerObservation `json:"runtimeHandlers,omitempty"`
	// StorageClasses inventories provisioning choices and their classification evidence.
	// Unknown evidence is not a negative storage availability result.
	// +listType=map
	// +listMapKey=name
	// +optional
	StorageClasses []StorageObservation `json:"storageClasses,omitempty"`
	// CSIDrivers contains driver names declared by CSIDriver or registered by CSINode.
	// +listType=set
	// +optional
	CSIDrivers []string `json:"csiDrivers,omitempty"`
	// CSIDriverDetails retains native driver declarations and node registration counts.
	// +listType=map
	// +listMapKey=name
	// +optional
	CSIDriverDetails []CSIDriverObservation `json:"csiDriverDetails,omitempty"`
	// NetworkPolicy reports recognized provider configuration, not tested traffic enforcement.
	// +optional
	NetworkPolicy *NetworkPolicyObservation `json:"networkPolicy,omitempty"`
	// Conditions reports whether each discovery section read its inputs successfully.
	// A successful scan does not establish that the discovered capability works.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=arcap
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="ClusterCapabilities must be named cluster"
// +kubebuilder:printcolumn:name="RuntimeScan",type=string,JSONPath=".status.conditions[?(@.type=='RuntimeDiscoveryReady')].status",description="Runtime discovery read succeeded; execution is not verified"
// +kubebuilder:printcolumn:name="StorageScan",type=string,JSONPath=".status.conditions[?(@.type=='StorageDiscoveryReady')].status",description="Storage discovery read succeeded; volume I/O is not verified"
// +kubebuilder:printcolumn:name="NetworkScan",type=string,JSONPath=".status.conditions[?(@.type=='NetworkPolicyDiscoveryReady')].status",description="Network discovery read succeeded; traffic enforcement is not verified"
// +kubebuilder:printcolumn:name="NetworkPolicy",type=string,JSONPath=".status.networkPolicy.status",description="Observed NetworkPolicy provider support"
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=".status.observedAt",description="Latest direct scan attempt, including failures"
// ClusterCapabilities is the cluster-scoped discovery report named cluster.
// Spec contains administrator input; status contains controller-owned observations.
type ClusterCapabilities struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec supplies optional classification overrides for existing native classes.
	// +optional
	Spec ClusterCapabilitiesSpec `json:"spec,omitempty"`
	// Status publishes capability summaries, native inventories and discovery freshness.
	// +optional
	Status ClusterCapabilitiesStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterCapabilitiesList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCapabilities `json:"items"`
}
