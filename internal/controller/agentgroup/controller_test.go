package agentgroup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
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
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "agentrally-quota"}, quota); err != nil {
		t.Fatal(err)
	}
	quota.Status.Hard = quota.Spec.Hard.DeepCopy()
	quota.Status.Used = corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("0")}
	if err := c.Status().Update(ctx, quota); err != nil {
		t.Fatal(err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "agentrally-group-policy"}, policy); err != nil {
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
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "agentrally-limits"}, limits); err != nil || !metav1.IsControlledBy(limits, group) || !reflect.DeepEqual(limits.Spec, *expectedLimits) {
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

func TestDeletionGuardUsesCurrentInventory(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			group, ns, _, _, _ := testutil.Fixture()
			group.Finalizers = []string{GroupFinalizer}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retained", Namespace: ns.Name}}
			actual, scheme := testutil.Client(t, group, ns, pvc)
			if err := actual.Delete(ctx, group); err != nil {
				t.Fatal(err)
			}
			cached := interceptor.NewClient(actual, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
				if pvcs, ok := list.(*corev1.PersistentVolumeClaimList); ok {
					pvcs.Items = nil
					return nil
				}
				return c.List(ctx, list, options...)
			}})
			reader := interceptor.NewClient(actual, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
				if _, ok := list.(*corev1.PersistentVolumeClaimList); ok && unavailable {
					return errors.New("inventory unavailable")
				}
				return c.List(ctx, list, options...)
			}})
			r := &Reconciler{Client: cached, Reader: reader, Scheme: scheme}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)})
			if (err != nil) != unavailable {
				t.Fatalf("unexpected inventory result: %v", err)
			}
			if err := actual.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil || ns.DeletionTimestamp != nil {
				t.Fatal("namespace deletion bypassed PVC protection", err)
			}
		})
	}
}

func TestCapacityFailureDoesNotHidePolicyReadiness(t *testing.T) {
	group, ns, _, _, _ := testutil.Fixture()
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2, IdleTTLSeconds: 60}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName("managed")
	pool.SetNamespace(ns.Name)
	pool.SetLabels(map[string]string{allocation.ManagedLabel: "true"})
	pool.SetOwnerReferences(ns.OwnerReferences)
	pool.Object["spec"] = map[string]interface{}{"replicas": int64(0)}
	actual, scheme := testutil.Client(t, group, ns, pool)
	denied := interceptor.NewClient(actual, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if object.GetName() == pool.GetName() {
			return errors.New("pool update unavailable")
		}
		return c.Patch(ctx, object, patch, opts...)
	}})
	r := &Reconciler{Client: denied, Reader: actual, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err == nil {
		t.Fatal("pool failure was lost")
	}
	if err := actual.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil {
		t.Fatal(err)
	}
	testutil.Condition(t, group.Status.Conditions, "Ready", metav1.ConditionTrue)
	testutil.Condition(t, group.Status.Conditions, "CapacityReady", metav1.ConditionFalse)
}

func TestGroupDeletionProtectsPendingNativeAllocation(t *testing.T) {
	group, ns, _, _, _ := testutil.Fixture()
	group.Finalizers = []string{GroupFinalizer}
	claim := sandbox.ExtensionObject("SandboxClaim")
	claim.SetName("pending-native-allocation")
	claim.SetNamespace(ns.Name)
	c, scheme := testutil.Client(t, group, ns, claim)
	if err := c.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil || ns.DeletionTimestamp != nil {
		t.Fatal("pending native allocation did not protect the namespace", err)
	}
}

func TestGroupIgnoresTimingsButObservesAllocationAndSummaryChanges(t *testing.T) {
	_, _, _, instance, _ := testutil.Fixture()
	changed := instance.DeepCopy()
	changed.Status.Timings = &v1.AgentInstanceTimings{PodReadyAt: &metav1.Time{Time: time.Now().UTC()}}
	predicate := groupMemberChanges()
	if predicate.Update(event.UpdateEvent{ObjectOld: instance, ObjectNew: changed}) {
		t.Fatal("timing observation triggered a full Group inventory")
	}
	changed.Status.Provisioning = &v1.AgentInstanceProvisioningStatus{PoolName: "new-pool", TemplateName: "new-template"}
	if !predicate.Update(event.UpdateEvent{ObjectOld: instance, ObjectNew: changed}) {
		t.Fatal("selection was hidden from pool pruning")
	}
	changed = instance.DeepCopy()
	changed.Status.Conditions = []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue, ObservedGeneration: instance.Generation}}
	if !predicate.Update(event.UpdateEvent{ObjectOld: instance, ObjectNew: changed}) {
		t.Fatal("availability change was hidden from Group summary")
	}
	before := &v1.AgentDefinition{Status: v1.AgentDefinitionStatus{Prewarming: &v1.DefinitionPrewarmingStatus{PoolName: "selected", ConfigurationRevision: "configuration"}}}
	after := before.DeepCopy()
	after.Status.Prewarming.ReadyReplicas, after.Status.Prewarming.DesiredReplicas = 1, 2
	if predicate.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("capacity feedback triggered a budget inventory")
	}
	after.Status.Prewarming = nil
	if !predicate.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("deactivating preparation was hidden from pruning")
	}
}

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
