package controller

import (
	"fmt"
	"slices"

	v1 "agentrally/operator/api/v1alpha1"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const GroupNetworkPolicyName = "agentrally-group-policy"

// InvocationPeer identifies the installed Operator Pods, not a backend identity.
type InvocationPeer struct {
	Namespace   string
	ReleaseName string
}

func ManagedInvocation(group *v1.AgentGroup) bool {
	return group.Spec.InvocationAccess != nil && group.Spec.InvocationAccess.Mode == "ConnectorOnly"
}

// GroupNetworkPolicy preserves the caller's egress scope. ConnectorOnly selects
// all group Pods, so accepting an old partial selector would silently widen it.
func GroupNetworkPolicy(group *v1.AgentGroup, peer InvocationPeer) (*networkingv1.NetworkPolicySpec, error) {
	policy := group.Spec.NetworkPolicy.DeepCopy()
	if !ManagedInvocation(group) {
		return policy, nil
	}
	if len(validation.IsDNS1123Label(peer.Namespace)) != 0 || peer.ReleaseName == "" || len(validation.IsValidLabelValue(peer.ReleaseName)) != 0 {
		return nil, fmt.Errorf("connector Pod identity is unavailable")
	}
	if policy == nil {
		policy = &networkingv1.NetworkPolicySpec{}
	}
	if len(policy.Ingress) != 0 || len(policy.PodSelector.MatchLabels) != 0 || len(policy.PodSelector.MatchExpressions) != 0 {
		return nil, fmt.Errorf("managed invocation requires an empty podSelector and no explicit ingress")
	}
	// Preserve native defaulting before making policyTypes explicit.
	if len(policy.PolicyTypes) == 0 && len(policy.Egress) != 0 {
		policy.PolicyTypes = append(policy.PolicyTypes, networkingv1.PolicyTypeEgress)
	}
	if !slices.Contains(policy.PolicyTypes, networkingv1.PolicyTypeIngress) {
		policy.PolicyTypes = append(policy.PolicyTypes, networkingv1.PolicyTypeIngress)
	}
	policy.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": peer.Namespace}},
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"app.kubernetes.io/name": "agentrally-operator", "app.kubernetes.io/instance": peer.ReleaseName, "app.kubernetes.io/component": "manager",
		}},
	}}}}
	return policy, nil
}

func IngressPolicy(policy *networkingv1.NetworkPolicy) bool {
	return len(policy.Spec.PolicyTypes) == 0 || slices.Contains(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
}
