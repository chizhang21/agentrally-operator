package v1alpha1

import corev1 "k8s.io/api/core/v1"

// AgentRuntime supplies instance settings for a built-in environment. Native
// Kubernetes types retain Secret references without copying credential values.
// Advanced Pod configuration belongs in a custom SandboxTemplate.
type AgentRuntime struct {
	// Image is the user's OCI image; built-in templates never select an application image.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^\S+$`
	Image string `json:"image"`
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// Command/Args replace the image entrypoint/arguments when supplied.
	// +optional
	Command []string `json:"command,omitempty"`
	// +optional
	Args []string `json:"args,omitempty"`
	// Env overrides built-in variables by name. Secret values remain valueFrom references.
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`
	// Resources replaces the built-in requests/limits block when supplied.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// RuntimeClassName is an existing resource name, passed through without alias mapping.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// Ports and probes are opt-in; built-ins impose no HTTP application contract.
	// +optional
	Ports []corev1.ContainerPort `json:"ports,omitempty"`
	// +optional
	ReadinessProbe *corev1.Probe `json:"readinessProbe,omitempty"`
	// +optional
	StartupProbe *corev1.Probe `json:"startupProbe,omitempty"`
	// +optional
	LivenessProbe *corev1.Probe `json:"livenessProbe,omitempty"`
}

// TemplateSource identifies allocation inputs, separating built-ins from native names.
func (s AgentClaimSpec) TemplateSource() string {
	if s.Template != "" {
		return "builtin/" + s.Template
	}
	if s.TemplateRef != nil {
		return s.TemplateRef.Name
	}
	return ""
}
