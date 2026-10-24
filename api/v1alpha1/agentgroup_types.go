package v1alpha1

import (
	"crypto/sha256"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HTTPRouteDefaults supplies shared ingress settings without owning the Gateway.
type HTTPRouteDefaults struct {
	// GatewayRef selects an existing Gateway; omitted uses discovery.
	// Namespace defaults to the Group's dedicated namespace.
	// +optional
	GatewayRef *GatewayReference `json:"gatewayRef,omitempty"`
	// Domain is the DNS suffix for generated Agent hostnames. Omitted uses a
	// wildcard hostname from the selected Gateway listener.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$"
	// +optional
	Domain string `json:"domain,omitempty"`
}

// InteractionDefaults applies only to instances requesting the matching route type.
type InteractionDefaults struct {
	// HTTPRoute is ignored by OpenShiftRoute instances and does not affect Group readiness.
	// +optional
	HTTPRoute *HTTPRouteDefaults `json:"httpRoute,omitempty"`
}

// PrewarmingPolicy controls spare capacity for each resolved configuration.
// Pools are discovered through instances; an unseen configuration starts cold.
type PrewarmingPolicy struct {
	// Replicas is the desired number of unclaimed Sandboxes per managed pool.
	// These run real applications and consume compute, storage and credentials.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
	// MaxTotalReplicas caps desired managed spares across configurations.
	// The Group distributes this budget evenly in stable pool-name order.
	// External pools are excluded; ResourceQuota remains the admission boundary.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxTotalReplicas *int32 `json:"maxTotalReplicas,omitempty"`
	// IdleTTLSeconds retains an unused pool before foreground deletion.
	// Zero removes unused capacity immediately. Group deletion bypasses this delay.
	// +kubebuilder:validation:Minimum=0
	// +optional
	IdleTTLSeconds int32 `json:"idleTTLSeconds,omitempty"`
}

// ExecutionPolicy constrains resolved execution Pods independently of capability discovery.
type ExecutionPolicy struct {
	// AllowedRuntimeClasses requires an explicitly selected RuntimeClass from this list.
	// Omitted permits the cluster default or any existing class.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	AllowedRuntimeClasses []string `json:"allowedRuntimeClasses,omitempty"`
	// RequireImageDigest requires SHA-256, SHA-384 or SHA-512 image digests on all
	// regular and init containers. It does not resolve mutable registry tags.
	// +optional
	RequireImageDigest bool `json:"requireImageDigest,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.namespace) == has(oldSelf.namespace)",message="namespace cannot be added or removed after creation"
type AgentGroupSpec struct {
	// Namespace optionally names the dedicated namespace and must start with arally-.
	// Omitted names are derived from the group name. Unrelated namespaces are never adopted.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^arally-[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace is immutable"
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// PodSecurity defaults to Restricted. Baseline is an administrator opt-in
	// for root or fixed-UID images; it does not grant OpenShift SCC permissions.
	// +kubebuilder:validation:Enum=Restricted;Baseline
	// +kubebuilder:default=Restricted
	// +optional
	PodSecurity string `json:"podSecurity,omitempty"`
	// +optional
	Quota *corev1.ResourceQuotaSpec `json:"quota,omitempty"`
	// LimitRange supplies native per-container/Pod/PVC defaults and limits.
	// Policy changes apply when Kubernetes admits new resources.
	// +optional
	LimitRange *corev1.LimitRangeSpec `json:"limitRange,omitempty"`
	// AllocationPaused blocks new Sandboxinstances. Submitted allocations, running
	// executions, suspension and cleanup continue normally.
	// +optional
	AllocationPaused bool `json:"allocationPaused,omitempty"`
	// Prewarming controls only Arally-managed pools. Omitted preserves manual
	// scaling and removes unused pools immediately; external pools are never scaled.
	// +optional
	Prewarming *PrewarmingPolicy `json:"prewarming,omitempty"`
	// ExecutionPolicy supplies administrator constraints for execution Pods.
	// +optional
	ExecutionPolicy *ExecutionPolicy `json:"executionPolicy,omitempty"`
	// NetworkPolicy expresses namespace/group policy, not runtime policy.
	// +optional
	NetworkPolicy *networkingv1.NetworkPolicySpec `json:"networkPolicy,omitempty"`
	// InteractionDefaults supplies optional Group-wide ingress defaults.
	// +optional
	InteractionDefaults *InteractionDefaults `json:"interactionDefaults,omitempty"`
}

// NamespaceName is stable for the lifetime of the group. Long names and names
// containing dots receive a hash suffix to avoid truncation/normalization collisions.
func (g *AgentGroup) NamespaceName() string {
	if g.Spec.Namespace != "" {
		return g.Spec.Namespace
	}
	name := strings.ReplaceAll(g.Name, ".", "-")
	const prefix = "arally-"
	const maxNameLength = 63 - len(prefix)
	const hashSuffixLength = 9
	if len(name) > maxNameLength || name != g.Name {
		sum := sha256.Sum256([]byte(g.Name))
		if len(name) > maxNameLength-hashSuffixLength {
			name = name[:maxNameLength-hashSuffixLength]
		}
		name = strings.TrimRight(name, "-") + fmt.Sprintf("-%x", sum[:4])
	}
	return prefix + name
}

type AgentGroupStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Summary observes instances and capacity in the owned namespace. Counts may
	// overlap (for example Ready and Available) and are eventually consistent.
	// +optional
	Summary *AgentGroupSummary `json:"summary,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type AgentInstanceSummary struct {
	Total     int32 `json:"total"`
	Ready     int32 `json:"ready"`
	Available int32 `json:"available"`
	Suspended int32 `json:"suspended"`
	// Pending counts non-expired, non-deleting instances that are neither Ready nor suspended.
	Pending int32 `json:"pending"`
	Expired int32 `json:"expired"`
}

type AgentPoolSummary struct {
	// Total includes managed and external pools in the namespace.
	Total int32 `json:"total"`
	// DesiredSpareReplicas sums native pool spec.replicas.
	DesiredSpareReplicas int32 `json:"desiredSpareReplicas"`
	// ReadySpareReplicas sums readyReplicas only for current-generation pool status.
	// It excludes terminating/stuck resources and is not total physical usage.
	ReadySpareReplicas int32 `json:"readySpareReplicas"`
}

type AgentStorageSummary struct {
	// PVCs includes all PVCs in the namespace, including externally created ones.
	PVCs int32 `json:"pvcs"`
	// Requested is the sum of PVC storage requests, including pending/deleting PVCs.
	Requested resource.Quantity `json:"requested"`
	// Capacity is reported provisioned capacity, not actual disk usage.
	Capacity resource.Quantity `json:"capacity"`
}

type AgentGroupSummary struct {
	Instances AgentInstanceSummary `json:"instances"`
	Pools     AgentPoolSummary     `json:"pools"`
	Storage   AgentStorageSummary  `json:"storage"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=arg
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=".status.namespace"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
type AgentGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentGroupSpec `json:"spec"`
	// +optional
	Status AgentGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentGroup `json:"items"`
}

func init() { SchemeBuilder.Register(&AgentGroup{}, &AgentGroupList{}) }
