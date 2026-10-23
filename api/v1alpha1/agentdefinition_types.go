package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentDefinitionSpec describes reusable deployment configuration, not an execution.
// +kubebuilder:validation:XValidation:rule="has(self.template) != has(self.templateRef)",message="select exactly one of template or templateRef"
// +kubebuilder:validation:XValidation:rule="has(self.template) == has(self.container)",message="container is required for built-in templates and is not used with templateRef"
// +kubebuilder:validation:XValidation:rule="!has(self.template) || !has(self.agentContainer) || size(self.agentContainer) == 0 || self.agentContainer == 'agent'",message="built-in templates use the agent container"
type AgentDefinitionSpec struct {
	AgentGroupRef AgentGroupReference `json:"agentGroupRef"`
	// +kubebuilder:validation:Enum=generic;assistant;coding
	// +optional
	Template string `json:"template,omitempty"`
	// Container supplies image and process settings for a built-in environment.
	// +optional
	Container *AgentContainerSpec `json:"container,omitempty"`
	// +optional
	TemplateRef *SandboxTemplateReference `json:"templateRef,omitempty"`
	// +optional
	AgentContainer string `json:"agentContainer,omitempty"`
	// +optional
	Security *AgentSecurity `json:"security,omitempty"`
	// +optional
	ModelProvider *ModelProvider `json:"modelProvider,omitempty"`
	// Workspace provides a private PVC-template default. Existing user PVCs are
	// bound separately on each AgentInstance.
	// +kubebuilder:validation:XValidation:rule="!has(self.existingClaimRef)",message="existing workspaces belong to AgentInstance, not a reusable definition"
	// +optional
	Workspace *WorkspaceMount `json:"workspace,omitempty"`
}

type AgentDefinitionStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// CurrentRevision identifies the latest published immutable configuration.
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`
	// +optional
	TemplateName string `json:"templateName,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type AgentDefinitionReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ard
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".spec.agentGroupRef.name"
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=".status.currentRevision"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
type AgentDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentDefinitionSpec `json:"spec"`
	// +optional
	Status AgentDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDefinition `json:"items"`
}

func init() { SchemeBuilder.Register(&AgentDefinition{}, &AgentDefinitionList{}) }

type ModelProvider struct {
	// +kubebuilder:validation:MinLength=1
	BaseURL string `json:"baseURL"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// APIKeySecretRef is resolved only in the workload's namespace.
	APIKeySecretRef corev1.SecretKeySelector `json:"apiKeySecretRef"`
}

// AgentSecurity selects the process identity; SCC authorization remains an
// administrator operation outside the Operator's RBAC.
// +kubebuilder:validation:XValidation:rule="self.profile != 'AnyUID' || (has(self.serviceAccountName) && size(self.serviceAccountName) > 0 && self.serviceAccountName != 'default')",message="AnyUID requires a dedicated serviceAccountName"
// +kubebuilder:validation:XValidation:rule="self.profile != 'Restricted' || !has(self.runAsUser) || self.runAsUser > 0",message="Restricted cannot run as UID 0"
type AgentSecurity struct {
	// +kubebuilder:validation:Enum=Restricted;AnyUID
	// +kubebuilder:default=Restricted
	// +optional
	Profile string `json:"profile,omitempty"`
	// ServiceAccountName refers to an existing namespaced account. Tokens remain disabled.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// RunAsUser is omitted by default so OpenShift can allocate an allowed UID.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`
}

// SandboxTemplateReference selects an upstream SandboxTemplate in the definition's namespace.
type SandboxTemplateReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}
