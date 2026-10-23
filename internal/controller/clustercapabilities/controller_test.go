package clustercapabilities

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/capabilities"
	"arally/operator/internal/testutil"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestClusterCapabilitiesPublishesStatusWithoutMutatingInputs(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1.AddToScheme, corev1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: 3, Labels: map[string]string{"user": "keep"}}}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: "nfs.csi.k8s.io"}
	driver := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: sc.Provisioner}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(report).WithObjects(report, sc, driver).Build()
	patches := 0
	guarded := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("discovery created a native resource")
			return nil
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("discovery mutated spec/metadata")
			return nil
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Fatal("discovery deleted a native resource")
			return nil
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			t.Fatal("discovery patched spec/metadata")
			return nil
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := o.(*v1.ClusterCapabilities); !ok || name != "status" {
				t.Fatal("unexpected discovery write")
			}
			patches++
			return c.SubResource(name).Patch(ctx, o, p, opts...)
		},
	})
	r := &Reconciler{Client: guarded, Reader: guarded}
	run := func() {
		t.Helper()
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: report.Name}})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter <= 0 || result.RequeueAfter > capabilities.RefreshInterval {
			t.Fatal("periodic refresh missing")
		}
	}
	get := func() *v1.ClusterCapabilities {
		t.Helper()
		current := &v1.ClusterCapabilities{}
		if err := base.Get(ctx, client.ObjectKeyFromObject(report), current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	run()
	current := get()
	testutil.Condition(t, current.Status.Conditions, "DiscoveryReady", metav1.ConditionTrue)
	if current.Status.ObservedGeneration != 3 || current.Status.ObservedAt == nil || len(current.Status.StorageClasses) != 1 || current.Status.StorageClasses[0].Status != "Observed" {
		t.Fatalf("status not published: %+v", current.Status)
	}
	if !reflect.DeepEqual(current.Labels, report.Labels) || len(current.Finalizers) != 0 || !reflect.DeepEqual(current.Spec, report.Spec) {
		t.Fatal("discovery took ownership of desired state")
	}
	for _, c := range current.Status.Conditions {
		if c.ObservedGeneration != 3 {
			t.Fatal("condition generation missing")
		}
	}
	transition := meta.FindStatusCondition(current.Status.Conditions, "DiscoveryReady").LastTransitionTime
	stale := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	current.Status.ObservedAt = &stale
	for i := range current.Status.Sections {
		current.Status.Sections[i].LastAttemptAt = &stale
	}
	if err := base.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	run()
	current = get()
	if !current.Status.ObservedAt.After(stale.Time) || !meta.FindStatusCondition(current.Status.Conditions, "DiscoveryReady").LastTransitionTime.Equal(&transition) {
		t.Fatal("freshness/transition semantics wrong")
	}
	if err := base.Delete(ctx, driver); err != nil {
		t.Fatal(err)
	}
	r.mapDependency(ctx, driver)
	run()
	current = get()
	if len(current.Status.CSIDrivers) != 0 || current.Status.StorageClasses[0].Status != "Unknown" || current.Status.StorageClasses[0].DriverRegistered {
		t.Fatal("removed driver retained in status")
	}
	r.Reader = interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*storagev1.StorageClassList); ok {
			return errors.New("forbidden")
		}
		return c.List(ctx, list, opts...)
	}})
	r.Now = func() time.Time { return time.Now().Add(capabilities.RefreshInterval) }
	r.mapDependency(ctx, sc)
	run()
	current = get()
	testutil.Condition(t, current.Status.Conditions, "StorageDiscoveryReady", metav1.ConditionFalse)
	testutil.Condition(t, current.Status.Conditions, "RuntimeDiscoveryReady", metav1.ConditionTrue)
	if len(current.Status.StorageClasses) != 0 || len(current.Status.CSIDrivers) != 0 {
		t.Fatal("failed scan left old success")
	}
	if requests := r.mapDependency(ctx, sc); len(requests) != 1 || requests[0].Name != report.Name {
		t.Fatal("dependency changes do not enqueue report")
	}
	// Noncanonical legacy reports do not trigger native scans or status writes.
	before := patches
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "user-report"}})
	if err != nil || result.RequeueAfter != 0 || patches != before {
		t.Fatal("noncanonical report was reconciled")
	}

}

var ctx = context.Background()

func TestNodeDiscoveryIgnoresHeartbeats(t *testing.T) {
	old := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	for _, tc := range []struct {
		name    string
		mutate  func(*corev1.Node)
		enqueue bool
	}{
		{"heartbeat", func(n *corev1.Node) { n.Status.Conditions[0].LastHeartbeatTime = metav1.Now() }, false},
		{"labels", func(n *corev1.Node) { n.Labels = map[string]string{"pool": "agents"} }, true},
		{"taints", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
		}, true},
		{"readiness", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse }, true},
		{"runtime", func(n *corev1.Node) { n.Status.NodeInfo.ContainerRuntimeVersion = "containerd://2.1.0" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := old.DeepCopy()
			tc.mutate(current)
			if nodeDiscoveryChanges().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) != tc.enqueue {
				t.Fatal("incorrect discovery trigger")
			}
		})
	}
}
