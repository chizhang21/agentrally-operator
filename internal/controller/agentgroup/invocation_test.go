package agentgroup

import (
	"reflect"
	"slices"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/testutil"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestConnectorPolicyPreservesEgressAndReportsConflicts(t *testing.T) {
	group, namespace, _, instance, _ := testutil.Fixture()
	group.Spec.InvocationAccess = &v1.InvocationAccess{Mode: "ConnectorOnly"}
	group.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{Egress: []networkingv1.NetworkPolicyEgressRule{{}}}
	peer := controller.InvocationPeer{Namespace: "operator-system", ReleaseName: "operator"}
	policy, err := controller.GroupNetworkPolicy(group, peer)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.Egress, group.Spec.NetworkPolicy.Egress) || len(policy.PolicyTypes) != 2 || len(policy.Ingress) != 1 || len(policy.Ingress[0].From) != 1 {
		t.Fatal(policy)
	}
	from := policy.Ingress[0].From[0]
	if from.PodSelector == nil || from.NamespaceSelector == nil {
		t.Fatal("namespace and pod identity must be ANDed in one peer")
	}
	for _, scenario := range []struct {
		name     string
		policy   *networkingv1.NetworkPolicySpec
		isolated bool
	}{
		{name: "no-user-policy"},
		{name: "deny-all-egress", policy: &networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}, isolated: true},
		{name: "udp-egress", policy: &networkingv1.NetworkPolicySpec{Egress: []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}}}}}, isolated: true},
	} {
		input := group.DeepCopy()
		input.Spec.NetworkPolicy = scenario.policy
		before := input.DeepCopy()
		effective, err := controller.GroupNetworkPolicy(input, peer)
		if err != nil {
			t.Fatal(err)
		}
		if effective == nil || len(effective.Ingress) != 1 || slices.Contains(effective.PolicyTypes, networkingv1.PolicyTypeEgress) != scenario.isolated {
			t.Fatal(scenario.name, "ConnectorOnly changed native egress isolation", effective)
		}
		if scenario.policy != nil && !reflect.DeepEqual(effective.Egress, scenario.policy.Egress) || !reflect.DeepEqual(input.Spec, before.Spec) {
			t.Fatal(scenario.name, "ConnectorOnly changed egress rules or user intent")
		}
	}
	c, scheme := testutil.Client(t, group, namespace)
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme, InvocationPeer: peer}
	if ready, _, _, err := r.prepare(ctx, group); err != nil || !ready {
		t.Fatal(ready, err)
	}
	if err := r.invocationStatus(ctx, group, true); err != nil {
		t.Fatal(err)
	}
	testutil.Condition(t, group.Status.Conditions, "InvocationAccessReady", metav1.ConditionTrue)
	testutil.Condition(t, group.Status.Conditions, "IngressEnforcementVerified", metav1.ConditionUnknown)
	instance.Spec.Interaction = &v1.InteractionEndpoint{Type: "OpenShiftRoute", Port: 8080}
	if err := c.Create(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := r.invocationStatus(ctx, group, true); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(group.Status.Conditions, "InvocationAccessReady"); condition.Reason != "InteractionConflict" {
		t.Fatal(condition)
	}
	extra := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: namespace.Name}, Spec: networkingv1.NetworkPolicySpec{Ingress: []networkingv1.NetworkPolicyIngressRule{{}}}}
	if err := c.Create(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err := r.invocationStatus(ctx, group, true); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(group.Status.Conditions, "InvocationAccessReady"); condition.Reason != "PolicyConflict" {
		t.Fatal(condition)
	}
	requests := r.mapNamespacePolicy(ctx, extra)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(group) {
		t.Fatal(requests)
	}
	before := instance.DeepCopy()
	instance.Status.Interaction = &v1.InteractionEndpointStatus{}
	if !groupMemberChanges().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: instance}) {
		t.Fatal("interaction cleanup was hidden by predicate")
	}
}
