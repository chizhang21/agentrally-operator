package v1alpha1

import (
	"crypto/sha256"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

// InteractionDefaults applies only to Claims requesting the matching route type.
type InteractionDefaults struct {
	// HTTPRoute is ignored by OpenShiftRoute Claims and does not affect Group readiness.
	// +optional
	HTTPRoute *HTTPRouteDefaults `json:"httpRoute,omitempty"`
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
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
