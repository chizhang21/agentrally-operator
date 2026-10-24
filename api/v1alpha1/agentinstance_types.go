package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:validation:XValidation:rule="has(self.volumeClaimTemplate) != has(self.existingClaimRef)",message="select exactly one of volumeClaimTemplate or existingClaimRef"
// +kubebuilder:validation:XValidation:rule="!has(self.exclusive) || !self.exclusive || has(self.existingClaimRef)",message="exclusive requires existingClaimRef"
type WorkspaceMount struct {
	// VolumeClaimTemplate provisions a private PVC for each Sandbox, including
	// warm instances. Core owns the PVC for the lifetime of that Sandbox.
	// +optional
	VolumeClaimTemplate *corev1.PersistentVolumeClaimSpec `json:"volumeClaimTemplate,omitempty"`
	// ExistingClaimRef mounts an externally managed PVC in the instance namespace.
	// The Operator pins its UID, never adopts or deletes it, and disables spares.
	// +optional
	ExistingClaimRef *PersistentVolumeClaimReference `json:"existingClaimRef,omitempty"`
	// Exclusive reserves the external PVC for this Instance UID until instance deletion.
	// Suspension, expiry and explicit execution replacement retain the reservation.
	// +optional
	Exclusive bool `json:"exclusive,omitempty"`
	// +kubebuilder:validation:MinLength=1
	MountPath string `json:"mountPath"`
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

type PersistentVolumeClaimReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// WorkspaceBinding identifies external storage retained across execution changes.
type WorkspaceBinding struct {
	ClaimName string    `json:"claimName"`
	UID       types.UID `json:"uid"`
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

type ActivityLeaseReference struct {
	// Name identifies a caller-managed activity Lease in the instance namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// AgentInstanceLifecycle controls execution cleanup, including Sandbox-owned PVCs.
// +kubebuilder:validation:XValidation:rule="has(self.idleTimeoutSeconds) == has(self.activityLeaseRef)",message="idleTimeoutSeconds and activityLeaseRef must be configured together"
type AgentInstanceLifecycle struct {
	// MaxLifetimeSeconds bounds wall-clock execution lifetime from validated
	// allocation, including suspension. Activity and Pod recreation do not reset it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +optional
	MaxLifetimeSeconds *int32 `json:"maxLifetimeSeconds,omitempty"`
	// IdleTimeoutSeconds suspends execution after trusted idle activity.
	// Missing, stale or invalid signals do not cause automatic suspension.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +optional
	IdleTimeoutSeconds *int32 `json:"idleTimeoutSeconds,omitempty"`
	// ActivityLeaseRef supplies the optional activity signal, fenced to this
	// Instance UID and its current Sandbox UID.
	// +optional
	ActivityLeaseRef *ActivityLeaseReference `json:"activityLeaseRef,omitempty"`
	// ShutdownTime is an absolute deadline, including time spent waiting for dependencies.
	// +optional
	ShutdownTime *metav1.Time `json:"shutdownTime,omitempty"`
	// TTLSecondsAfterFinished starts when Core confirms natural completion of the
	// current owned Pod. Suspension and Pod deletion do not start this TTL.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
	// ShutdownPolicy defaults to Retain, which keeps the AgentInstance and diagnostics
	// after execution cleanup. Delete removes the AgentInstance through its finalizer.
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

// AgentInstanceProvisioning configures mandatory upstream pool allocation.
// When omitted, Arally creates or reuses a configuration-specific pool.
// +kubebuilder:validation:XValidation:rule="has(self.warmPoolRef) == has(oldSelf.warmPoolRef)",message="warmPoolRef cannot be added or removed after creation"
type AgentInstanceProvisioning struct {
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

type AgentInstanceLifecycleStatus struct {
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

type AgentInstanceProvisioningStatus struct {
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
	// LaunchType is upstream launch provenance, independent of application readiness.
	// Omitted when upstream did not report a recognized classification.
	// +kubebuilder:validation:Enum=warm;cold
	// +optional
	LaunchType string `json:"launchType,omitempty"`
	// TimedOutAt prevents a deleted or late-bound Claim from silently restarting allocation.
	// +optional
	TimedOutAt *metav1.Time `json:"timedOutAt,omitempty"`
	// +kubebuilder:validation:Enum=Pool
	// +optional
	Mode string `json:"mode,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.provisioning) && has(self.provisioning.warmPoolRef)) == (has(oldSelf.provisioning) && has(oldSelf.provisioning.warmPoolRef))",message="warmPoolRef cannot be added or removed after creation"
type AgentInstanceSpec struct {
	AgentGroupRef AgentGroupReference      `json:"agentGroupRef"`
	DefinitionRef AgentDefinitionReference `json:"definitionRef"`
	// Revision optionally selects a published definition revision. Omitted pins
	// the latest revision on first allocation and on explicit execution replacement.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +optional
	Revision string `json:"revision,omitempty"`
	// +kubebuilder:validation:Enum=Running;Suspended
	// +kubebuilder:default=Running
	// +optional
	OperatingMode string `json:"operatingMode,omitempty"`
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
	Lifecycle *AgentInstanceLifecycle `json:"lifecycle,omitempty"`
	// +optional
	Provisioning *AgentInstanceProvisioning `json:"provisioning,omitempty"`
}

type DefinitionBinding struct {
	Name         string    `json:"name"`
	UID          types.UID `json:"uid"`
	Revision     string    `json:"revision"`
	TemplateName string    `json:"templateName"`
}

type AgentInstanceStatus struct {
	// Definition pins the published configuration independently of later updates.
	// +optional
	Definition *DefinitionBinding `json:"definition,omitempty"`
	// Workspace pins external PVC identity independently of the current execution.
	// +optional
	Workspace *WorkspaceBinding `json:"workspace,omitempty"`
	// ObservedGeneration reports processed intent, not the running Pod's revision.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	SandboxName string `json:"sandboxName,omitempty"`
	// SandboxUID identifies the current execution for fencing caller activity.
	// +optional
	SandboxUID types.UID `json:"sandboxUID,omitempty"`
	// Activity observes the optional caller-provided activity contract.
	// +optional
	Activity *AgentInstanceActivityStatus `json:"activity,omitempty"`
	// VolumeClaims maps logical volumes to the current execution's PVCs, including
	// an externally managed workspace when configured.
	// +optional
	VolumeClaims map[string]string `json:"volumeClaims,omitempty"`
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
	Lifecycle *AgentInstanceLifecycleStatus `json:"lifecycle,omitempty"`
	// +optional
	Provisioning *AgentInstanceProvisioningStatus `json:"provisioning,omitempty"`
	// Timings persist observations for the current execution. Pod readiness is
	// first availability for the observed Pod UID, not an application transaction.
	// +optional
	Timings *AgentInstanceTimings `json:"timings,omitempty"`
	// Ready reports current desired configuration and execution readiness.
	// Available reports current execution health independently of pending updates.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type AgentInstanceActivityStatus struct {
	// +optional
	LastActivityAt *metav1.MicroTime `json:"lastActivityAt,omitempty"`
	// ValidUntil is the activity heartbeat's freshness deadline.
	// +optional
	ValidUntil *metav1.MicroTime `json:"validUntil,omitempty"`
	// +optional
	IdleDeadline *metav1.MicroTime `json:"idleDeadline,omitempty"`
	// Suspended records automatic idle suspension; explicit operatingMode still wins.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
}

type AgentInstanceTimings struct {
	// +optional
	PodCreatedAt *metav1.Time `json:"podCreatedAt,omitempty"`
	// +optional
	PodReadyAt *metav1.Time `json:"podReadyAt,omitempty"`
	// +optional
	SuspensionStartedAt *metav1.Time `json:"suspensionStartedAt,omitempty"`
	// +optional
	SuspendedAt *metav1.Time `json:"suspendedAt,omitempty"`
	// +optional
	CleanupStartedAt *metav1.Time `json:"cleanupStartedAt,omitempty"`
	// +optional
	CleanupCompletedAt *metav1.Time `json:"cleanupCompletedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ari
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=".spec.agentGroupRef.name"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.operatingMode"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=".status.conditions[?(@.type=='Available')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
type AgentInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentInstanceSpec `json:"spec"`
	// +optional
	Status AgentInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentInstance `json:"items"`
}

func init() { SchemeBuilder.Register(&AgentInstance{}, &AgentInstanceList{}) }
