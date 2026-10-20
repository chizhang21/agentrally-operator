package v1alpha1

import (
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

type ClusterCapabilitiesSpec struct{}

type ClusterCapabilitiesStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedAt is the completion time of the last scan attempt, including failures.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
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
