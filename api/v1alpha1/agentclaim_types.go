package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ModelProvider struct {
	// +kubebuilder:validation:MinLength=1
	BaseURL string `json:"baseURL"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// APIKeySecretRef is resolved only in the workload's namespace.
	APIKeySecretRef corev1.SecretKeySelector `json:"apiKeySecretRef"`
}

type WorkspaceMount struct {
	// ClaimName refers to an existing PVC; Arally never provisions or owns it.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
	// +kubebuilder:validation:MinLength=1
	MountPath string `json:"mountPath"`
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// AgentGroupReference identifies a cluster-scoped AgentGroup by Kubernetes name.
type AgentGroupReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="agentGroupRef.name is immutable"
	Name string `json:"name"`
}

// GatewayReference attaches an HTTPRoute to an administrator-managed Gateway.
type GatewayReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$"
	Name string `json:"name"`
	// Namespace defaults to the workload namespace.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// SectionName selects a listener on the Gateway.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +optional
	SectionName string `json:"sectionName,omitempty"`
}

// InteractionEndpoint exposes an HTTP application without interpreting its content.
// Omitting this block disables the endpoint and removes its owned access resources.
// +kubebuilder:validation:XValidation:rule="self.type != 'OpenShiftRoute' || !has(self.gatewayRef)",message="gatewayRef is only valid for HTTPRoute"
type InteractionEndpoint struct {
	// +kubebuilder:validation:Enum=HTTPRoute;OpenShiftRoute
	Type string `json:"type"`
	// Port overrides the Agent's named http port or sole TCP port.
	// The access Service uses port 80.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`
	// Hostname overrides the Group/listener-derived hostname for HTTPRoute.
	// OpenShift assigns one when omitted.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$"
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// Scheme describes the external listener. Omitted uses the Gateway listener
	// protocol for HTTPRoute, or HTTPS edge termination for OpenShiftRoute.
	// +kubebuilder:validation:Enum=HTTP;HTTPS
	// +optional
	Scheme string `json:"scheme,omitempty"`
	// +optional
	GatewayRef *GatewayReference `json:"gatewayRef,omitempty"`
}

type InteractionEndpointStatus struct {
	ServiceName string `json:"serviceName"`
	RouteName   string `json:"routeName"`
	// URL is the configured address, not an end-to-end connectivity or authentication check.
	// +optional
	URL string `json:"url,omitempty"`
	// Port is the resolved Agent listening port.
	// +optional
	Port int32 `json:"port,omitempty"`
	// GatewayRef identifies the selected HTTPRoute parent and listener.
	// +optional
	GatewayRef *GatewayReference `json:"gatewayRef,omitempty"`
}

// AgentClaimLifecycle controls execution cleanup; externally supplied PVCs are retained.
type AgentClaimLifecycle struct {
	// ShutdownTime is an absolute deadline, including time spent waiting for dependencies.
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`
	// TTLSecondsAfterFinished starts when Core confirms natural completion of the
	// current owned Pod. Suspension and Pod deletion do not start this TTL.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
	// ShutdownPolicy defaults to Retain, which keeps the AgentClaim and diagnostics
	// after execution cleanup. Delete removes the AgentClaim through its finalizer.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	ShutdownPolicy string `json:"shutdownPolicy,omitempty"`
	// RetainFailed skips completion TTL for failures; an absolute deadline still applies.
	// +kubebuilder:default=true
	// +optional
	RetainFailed *bool `json:"retainFailed,omitempty"`
}

// WarmPoolReference selects a namespaced upstream SandboxWarmPool.
type WarmPoolReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$"
	Name string `json:"name"`
}

// AgentClaimProvisioning configures mandatory upstream pool allocation.
// When omitted, Arally creates or reuses a configuration-specific pool.
// +kubebuilder:validation:XValidation:rule="has(self.warmPoolRef) == has(oldSelf.warmPoolRef)",message="warmPoolRef cannot be added or removed after creation"
type AgentClaimProvisioning struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="warmPoolRef is immutable"
	// WarmPoolRef optionally selects a named pool; omitted uses a configuration-specific shared pool.
	// +optional
	WarmPoolRef *WarmPoolReference `json:"warmPoolRef,omitempty"`
	// ClaimTimeoutSeconds bounds waiting for an owned, matching Sandbox allocation.
	// Application startup is observed separately; Lifecycle controls execution cleanup.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=60
	// +optional
	ClaimTimeoutSeconds *int32 `json:"claimTimeoutSeconds,omitempty"`
}

type AgentClaimLifecycleStatus struct {
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// FinishedReason persists success/failure even when dependency conditions change.
	// +optional
	FinishedReason string `json:"finishedReason,omitempty"`
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// ExpiredAt latches expiration, preventing execution resources from being recreated.
	// +optional
	ExpiredAt *metav1.Time `json:"expiredAt,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

type AgentClaimProvisioningStatus struct {
	// PoolName and TemplateName record the selected pool and effective native template.
	// +optional
	PoolName string `json:"poolName,omitempty"`
	// +optional
	TemplateName string `json:"templateName,omitempty"`
	// ConfigurationRevision pins creation inputs for the current allocation attempt.
	// +optional
	ConfigurationRevision string `json:"configurationRevision,omitempty"`

	// +optional
	ClaimName string `json:"claimName,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt records validated Sandbox assignment, independently of Pod readiness.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// TimedOutAt prevents a deleted or late-bound Claim from silently restarting allocation.
	// +optional
	TimedOutAt *metav1.Time `json:"timedOutAt,omitempty"`
	// +kubebuilder:validation:Enum=Pool
	// +optional
	Mode string `json:"mode,omitempty"`
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

// SandboxTemplateReference selects an upstream SandboxTemplate in the Claim's namespace.
type SandboxTemplateReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.provisioning) && has(self.provisioning.warmPoolRef)) == (has(oldSelf.provisioning) && has(oldSelf.provisioning.warmPoolRef))",message="warmPoolRef cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="has(self.template) != has(self.templateRef)",message="select exactly one of template or templateRef"
// +kubebuilder:validation:XValidation:rule="has(self.template) == has(self.runtime)",message="runtime is required for built-in templates and is not used with templateRef"
// +kubebuilder:validation:XValidation:rule="!has(self.template) || !has(self.agentContainer) || size(self.agentContainer) == 0 || self.agentContainer == 'agent'",message="built-in templates use the agent container"
type AgentClaimSpec struct {
	AgentGroupRef AgentGroupReference `json:"agentGroupRef"`
	// Template selects an Operator-bundled environment. Runtime supplies its image and instance settings.
	// +kubebuilder:validation:Enum=generic;assistant;coding
	// +optional
	Template string `json:"template,omitempty"`
	// +optional
	Runtime *AgentRuntime `json:"runtime,omitempty"`
	// TemplateRef references an existing native SandboxTemplate in this namespace.
	// Use this instead of Template/Runtime for custom native Pod configuration.
	// +optional
	TemplateRef *SandboxTemplateReference `json:"templateRef,omitempty"`
	// AgentContainer selects the container for model/workspace conveniences.
	// When omitted, the first container is used.
	// +optional
	AgentContainer string `json:"agentContainer,omitempty"`
	// +optional
	Security *AgentSecurity `json:"security,omitempty"`
	// +kubebuilder:validation:Enum=Running;Suspended
	// +kubebuilder:default=Running
	// +optional
	OperatingMode string `json:"operatingMode,omitempty"`
	// +optional
	ModelProvider *ModelProvider `json:"modelProvider,omitempty"`
	// +optional
	Workspace *WorkspaceMount `json:"workspace,omitempty"`
	// Interaction optionally exposes the Agent's HTTP API or management page.
	// +optional
	Interaction *InteractionEndpoint `json:"interaction,omitempty"`
	// RestartToken changes explicitly replace this execution through its owning CR.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	RestartToken string `json:"restartToken,omitempty"`
	// +optional
	Lifecycle *AgentClaimLifecycle `json:"lifecycle,omitempty"`
	// +optional
	Provisioning *AgentClaimProvisioning `json:"provisioning,omitempty"`
}

type AgentClaimStatus struct {
	// ObservedGeneration reports processed intent, not the running Pod's revision.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	SandboxName string `json:"sandboxName,omitempty"`
	// +optional
	ServiceName string `json:"serviceName,omitempty"`
	// +optional
	ServiceFQDN string `json:"serviceFQDN,omitempty"`
	// +optional
	Interaction *InteractionEndpointStatus `json:"interaction,omitempty"`
	// DesiredRevision identifies submitted Pod configuration, not Secret plaintext.
	// +optional
	DesiredRevision string `json:"desiredRevision,omitempty"`
	// AppliedRevision is populated only after observing the owned execution Pod.
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`
	// +optional
	PodName string `json:"podName,omitempty"`
	// +optional
	PodUID string `json:"podUID,omitempty"`
	// LastRestartToken records the accepted restart request for idempotent replacement.
	// +optional
	LastRestartToken string `json:"lastRestartToken,omitempty"`
	// +optional
	Lifecycle *AgentClaimLifecycleStatus `json:"lifecycle,omitempty"`
	// +optional
	Provisioning *AgentClaimProvisioningStatus `json:"provisioning,omitempty"`
	// Ready reports current desired configuration and execution readiness.
	// Available reports current execution health independently of pending updates.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=arc
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".spec.agentGroupRef.name"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.operatingMode"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=".status.conditions[?(@.type=='Available')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
type AgentClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentClaimSpec `json:"spec"`
	// +optional
	Status AgentClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentClaim `json:"items"`
}

func init() { SchemeBuilder.Register(&AgentClaim{}, &AgentClaimList{}) }
