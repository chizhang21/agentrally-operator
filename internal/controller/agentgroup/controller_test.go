package agentgroup

import (
	"context"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGroupReconcilesPolicyQuotaAndProtectsNamespaceDeletion(t *testing.T) {
	group, ns, _, _, _ := testutil.Fixture()
	group.Status = v1.AgentGroupStatus{}
	group.Spec.Quota = &corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("4")}}
	group.Spec.LimitRange = &corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}}
	group.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}
	c, scheme := testutil.Client(t, group, ns)
	r := &Reconciler{Client: c, Scheme: scheme}
	run := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	current := &v1.AgentGroup{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		t.Fatal(err)
	}
	testutil.Condition(t, current.Status.Conditions, "Ready", metav1.ConditionFalse)
	quota := &corev1.ResourceQuota{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "arally-quota"}, quota); err != nil {
		t.Fatal(err)
	}
	quota.Status.Hard = quota.Spec.Hard.DeepCopy()
	quota.Status.Used = corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("0")}
	if err := c.Status().Update(ctx, quota); err != nil {
		t.Fatal(err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "arally-group-policy"}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	if err := c.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	run()
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		t.Fatal(err)
	}
	testutil.Condition(t, current.Status.Conditions, "Ready", metav1.ConditionTrue)
	limits := &corev1.LimitRange{}
	expectedLimits := group.Spec.LimitRange.DeepCopy()
	expectedLimits.Limits[0].DefaultRequest = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "arally-limits"}, limits); err != nil || !metav1.IsControlledBy(limits, group) || !reflect.DeepEqual(limits.Spec, *expectedLimits) {
		t.Fatalf("native LimitRange not prepared: %+v %v", limits, err)
	}
	current.Spec.LimitRange = nil
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	run()
	if err := c.Get(ctx, client.ObjectKeyFromObject(limits), limits); !apierrors.IsNotFound(err) {
		t.Fatalf("removed LimitRange was not cleaned up: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.Spec, *group.Spec.NetworkPolicy) {
		t.Fatal("policy drift not repaired")
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retain", Namespace: ns.Name}}
	if err := c.Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	run()
	if err := c.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal("namespace deleted with workspace")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(current.Status.Conditions, "Ready").Reason != "GroupInUse" {
		t.Fatal("missing group deletion diagnostic")
	}
	if err := c.Delete(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	run()
	run()
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), current); !apierrors.IsNotFound(err) {
		t.Fatalf("group finalizer stuck: %v", err)
	}
}

var ctx = context.Background()

func TestCapabilitiesWatchMapsPolicyGroups(t *testing.T) {
	group, ns, _, _, _ := testutil.Fixture()
	group.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{}
	c, scheme := testutil.Client(t, group, ns)
	r := &Reconciler{Client: c, Scheme: scheme}
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	requests := r.mapCapabilities(ctx, report)
	if len(requests) != 1 || requests[0].Name != group.Name {
		t.Fatal("group report dependency not mapped", requests)
	}
	report.Name = "other"
	if len(r.mapCapabilities(ctx, report)) != 0 {
		t.Fatal("noncanonical report mapped")
	}
	group.Spec.NetworkPolicy = nil
	if err := c.Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	report.Name = "cluster"
	if len(r.mapCapabilities(ctx, report)) != 0 {
		t.Fatal("group without network policy mapped")
	}
}

func TestGroupReadyDoesNotDependOnUnusedHTTPRouteDefaults(t *testing.T) {
	group, ns, _, _, _ := testutil.Fixture()
	group.Status = v1.AgentGroupStatus{}
	group.Spec.InteractionDefaults = &v1.InteractionDefaults{HTTPRoute: &v1.HTTPRouteDefaults{GatewayRef: &v1.GatewayReference{Name: "missing", Namespace: "gateway-system"}, Domain: "agents.example.com"}}
	c, scheme := testutil.Client(t, group, ns)
	r := &Reconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err != nil {
		t.Fatal(err)
	}
	current := &v1.AgentGroup{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(group), current); err != nil {
		t.Fatal(err)
	}
	testutil.Condition(t, current.Status.Conditions, "Ready", metav1.ConditionTrue)
}
