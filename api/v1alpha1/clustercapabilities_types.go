package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
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
	// +kubebuilder:validation:Enum=Observed;Unknown
	Status string `json:"status"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// Source identifies classification evidence, not a successful runtime probe.
	// +kubebuilder:validation:Enum=NativeAPI;BuiltInCatalog;AdministratorDeclared
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

// NodeRuntimeObservation reports kubelet CRI versions without inferring executors.
type NodeRuntimeObservation struct {
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
	Nodes          int32  `json:"nodes"`
}

type StorageObservation struct {
	Name        string `json:"name"`
	Provisioner string `json:"provisioner"`
	// +kubebuilder:validation:Enum=NFS;Block;Local;Unknown
	Type             string `json:"type"`
	Source           string `json:"source"`
	Default          bool   `json:"default"`
	DriverRegistered bool   `json:"driverRegistered"`
	// +optional
	VolumeBindingMode storagev1.VolumeBindingMode `json:"volumeBindingMode,omitempty"`
	// +optional
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
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

type SnapshotClassObservation struct {
	Name           string `json:"name"`
	Driver         string `json:"driver"`
	DeletionPolicy string `json:"deletionPolicy"`
	Default        bool   `json:"default"`
}

// SnapshotObservation reports VolumeSnapshotClass API availability, not backend
// snapshot creation or restore success. Pre-provisioned snapshots may have no class.
type SnapshotObservation struct {
	// +kubebuilder:validation:Enum=Available;Unavailable;Unknown
	ClassAPIStatus string `json:"classAPIStatus"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +listType=map
	// +listMapKey=name
	// +optional
	Classes []SnapshotClassObservation `json:"classes,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="(self.category == 'WebAssembly') == (self.imageContract == 'WasmWASI')",message="WebAssembly requires WasmWASI; other categories require LinuxOCI"
type RuntimeMapping struct {
	// +kubebuilder:validation:MinLength=1
	RuntimeClassName string `json:"runtimeClassName"`
	// +kubebuilder:validation:MinLength=1
	Family string `json:"family"`
	// +kubebuilder:validation:Enum=Container;UserSpaceKernel;VirtualMachine;WebAssembly
	Category string `json:"category"`
	// +kubebuilder:validation:Enum=LinuxOCI;WasmWASI
	ImageContract string `json:"imageContract"`
}

type StorageMapping struct {
	// +kubebuilder:validation:MinLength=1
	StorageClassName string `json:"storageClassName"`
	// Type describes the backend; Block does not mean PVC volumeMode: Block.
	// +kubebuilder:validation:Enum=NFS;Block;Local;Unknown
	Type string `json:"type"`
}

type ClusterCapabilitiesSpec struct {
	// RuntimeMappings apply only to the named classes. Missing classes are not invented.
	// +listType=map
	// +listMapKey=runtimeClassName
	// +kubebuilder:validation:MaxItems=64
	// +optional
	RuntimeMappings []RuntimeMapping `json:"runtimeMappings,omitempty"`
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

type ClusterCapabilitiesStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedAt is the completion time of the last scan attempt, including failures.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// CatalogRevision identifies the embedded classification rules.
	// +optional
	CatalogRevision string `json:"catalogRevision,omitempty"`
	// +listType=map
	// +listMapKey=name
	// +optional
	Sections []DiscoverySectionObservation `json:"sections,omitempty"`
	// +optional
	DefaultRuntime *RuntimeObservation `json:"defaultRuntime,omitempty"`
	// +listType=map
	// +listMapKey=name
	// +optional
	Runtimes []RuntimeObservation `json:"runtimes,omitempty"`
	// +listType=map
	// +listMapKey=implementation
	// +listMapKey=version
	// +optional
	NodeRuntimes []NodeRuntimeObservation `json:"nodeRuntimes,omitempty"`
	// +listType=map
	// +listMapKey=name
	// +optional
	StorageClasses []StorageObservation `json:"storageClasses,omitempty"`
	// +listType=set
	// +optional
	CSIDrivers []string `json:"csiDrivers,omitempty"`
	// +listType=map
	// +listMapKey=name
	// +optional
	CSIDriverDetails []CSIDriverObservation `json:"csiDriverDetails,omitempty"`
	// +optional
	Snapshots *SnapshotObservation `json:"snapshots,omitempty"`
	// +optional
	NetworkPolicy *NetworkPolicyObservation `json:"networkPolicy,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=arcap
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="ClusterCapabilities must be named cluster"
// +kubebuilder:printcolumn:name="Discovered",type=string,JSONPath=".status.conditions[?(@.type=='DiscoveryReady')].status"
// +kubebuilder:printcolumn:name="NetworkPolicy",type=string,JSONPath=".status.networkPolicy.status"
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=".status.observedAt"
type ClusterCapabilities struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +optional
	Spec ClusterCapabilitiesSpec `json:"spec,omitempty"`
	// +optional
	Status ClusterCapabilitiesStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ClusterCapabilitiesList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCapabilities `json:"items"`
}

func init() { SchemeBuilder.Register(&ClusterCapabilities{}, &ClusterCapabilitiesList{}) }
