package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// AgentWorkspaceSpec fixes the first binding. Rebinding requires a future verified release protocol.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspace identity and initial retention policy are immutable"
type AgentWorkspaceSpec struct {
	GroupName    string            `json:"groupName"`
	GroupUID     types.UID         `json:"groupUID"`
	Namespace    string            `json:"namespace"`
	NamespaceUID types.UID         `json:"namespaceUID"`
	InstanceName string            `json:"instanceName"`
	InstanceUID  types.UID         `json:"instanceUID"`
	Definition   DefinitionBinding `json:"definition"`
	RevisionUID  types.UID         `json:"revisionUID"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=31536000
	RetentionSeconds    int64                            `json:"retentionSeconds"`
	VolumeClaimTemplate corev1.PersistentVolumeClaimSpec `json:"volumeClaimTemplate"`
}

type AgentWorkspaceStatus struct {
	// Bound describes ownership, not PVC readiness or verified execution release.
	// +kubebuilder:validation:Enum=Bound
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	PVCUID types.UID `json:"pvcUID,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=arw
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=".spec.instanceName"
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=".spec.namespace"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
type AgentWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentWorkspaceSpec `json:"spec"`
	// +optional
	Status AgentWorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentWorkspace `json:"items"`
}
