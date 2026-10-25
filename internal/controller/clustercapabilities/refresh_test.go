package clustercapabilities

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/capabilities"
	"agentrally/operator/internal/testutil"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestRefreshUsesAffectedSectionsWithoutRenewingCacheEvidence(t *testing.T) {
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: 1}}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Provisioner: "rancher.io/local-path"}
	builder, scheme := testutil.ClientBuilder(t)
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := builder.WithObjects(report, sc).Build()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	lists, patches := 0, 0
	failPatch := false
	var duringAudit func()
	reader := interceptor.NewClient(c, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		lists++
		if _, ok := list.(*storagev1.StorageClassList); ok && duringAudit != nil {
			fn := duringAudit
			duringAudit = nil
			fn()
		}
		return c.List(ctx, list, opts...)
	}})
	writer := interceptor.NewClient(c, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		patches++
		if failPatch {
			return apierrors.NewConflict(schema.GroupResource{Group: "arally.io", Resource: "clustercapabilities"}, obj.GetName(), nil)
		}
		return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}})
	r := &Reconciler{Client: writer, Reader: reader, Now: func() time.Time { return now }}
	run := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cluster"}}); err != nil {
			t.Fatal(err)
		}
	}
	get := func() *v1.ClusterCapabilities {
		t.Helper()
		value := &v1.ClusterCapabilities{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "cluster"}, value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	run()
	initialLists, initialPatches := lists, patches
	initial := get()
	now = now.Add(10 * time.Second)
	r.mapDependency(context.Background(), &appsv1.DaemonSet{})
	run()
	if lists != initialLists || patches != initialPatches || !get().Status.ObservedAt.Equal(initial.Status.ObservedAt) {
		t.Fatal("unchanged cache event caused an API audit or renewed evidence")
	}
	sc.AllowVolumeExpansion = new(bool)
	*sc.AllowVolumeExpansion = true
	if err := c.Update(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	r.mapDependency(context.Background(), sc)
	run()
	if lists-initialLists != 3 {
		t.Fatalf("storage change scanned unrelated sections: %d LISTs", lists-initialLists)
	}
	if !get().Status.Sections[0].LastSuccessAt.Equal(initial.Status.Sections[0].LastSuccessAt) {
		t.Fatal("storage event renewed runtime evidence")
	}
	// Direct audit sees newer storage than the intentionally lagging cache.
	currentSC := sc.DeepCopy()
	*sc.AllowVolumeExpansion = false
	if err := c.Update(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(writer, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if out, ok := list.(*storagev1.StorageClassList); ok {
			out.Items = []storagev1.StorageClass{*currentSC}
			return nil
		}
		return c.List(ctx, list, opts...)
	}})
	now = now.Add(capabilities.RefreshInterval)
	run()
	now = now.Add(time.Second)
	r.mapDependency(context.Background(), sc)
	run()
	if *get().Status.StorageClasses[0].AllowVolumeExpansion {
		t.Fatal("lagging cache overwrote newer audited facts")
	}
	r.Client = writer
	// An event delivered after runtime collection must survive the in-flight audit.
	duringAudit = func() {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"kubernetes.io/os": "linux"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{ContainerRuntimeVersion: "containerd://2.1.0"}}}
		if err := c.Create(context.Background(), node); err != nil {
			t.Fatal(err)
		}
		var group sync.WaitGroup
		group.Add(1)
		go func() { defer group.Done(); r.mapDependency(context.Background(), node) }()
		group.Wait()
	}
	r.started = false
	run()
	before := lists
	run()
	if lists-before != 2 || len(get().Status.NodeRuntimes) != 1 {
		t.Fatal("event arriving during audit was lost")
	}
	// A replacement manager audits even a still-fresh persisted report.
	before = lists
	replacement := &Reconciler{Client: writer, Reader: reader, Now: func() time.Time { return now }}
	if _, err := replacement.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cluster"}}); err != nil {
		t.Fatal(err)
	}
	if lists-before < 6 {
		t.Fatal("replacement trusted persisted freshness without an API audit")
	}
	// A failed publication must preserve the drained dependency invalidation.
	now = now.Add(time.Second)
	*sc.AllowVolumeExpansion = true
	if err := c.Update(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	r.mapDependency(context.Background(), sc)
	failPatch = true
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cluster"}}); err == nil {
		t.Fatal("publication failure was hidden")
	}
	failPatch = false
	run()
	if !*get().Status.StorageClasses[0].AllowVolumeExpansion {
		t.Fatal("failed publication lost the dependency change")
	}
}

func TestNetworkWatchFiltersUnrelatedActivity(t *testing.T) {
	old := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "application"}}
	current := old.DeepCopy()
	current.Generation++
	if networkDaemonChanges().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
		t.Fatal("unrelated DaemonSet triggered discovery")
	}
	old.Name = "calico-node"
	current = old.DeepCopy()
	current.Status.NumberAvailable++
	if networkDaemonChanges().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
		t.Fatal("unconsumed DaemonSet status triggered discovery")
	}
	current.Status.NumberReady++
	if !networkDaemonChanges().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) {
		t.Fatal("provider readiness change was ignored")
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config"}, Data: map[string]string{"enable-policy": "default"}}
	updated := cm.DeepCopy()
	updated.Labels = map[string]string{"unrelated": "value"}
	if networkConfigChanges().Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: updated}) {
		t.Fatal("ConfigMap metadata triggered discovery")
	}
	updated.Data["enable-policy"] = "never"
	if !networkConfigChanges().Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: updated}) {
		t.Fatal("CNI configuration change was ignored")
	}
}

func TestNetworkWatchTracksRenamedProviderAndCustomConfiguration(t *testing.T) {
	old := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "custom-agent"}, Spec: appsv1.DaemonSetSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cilium-agent"}}}},
	}}
	current := old.DeepCopy()
	current.Spec.Template.Labels = map[string]string{"k8s-app": "cilium"}
	if !networkDaemonChanges().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: current}) || !networkDaemonChanges().Update(event.UpdateEvent{ObjectOld: current, ObjectNew: old}) {
		t.Fatal("provider label transitions did not invalidate recognition")
	}
	unrelated := current.DeepCopy()
	unrelated.Labels = map[string]string{"unrelated": "value"}
	if networkDaemonChanges().Update(event.UpdateEvent{ObjectOld: current, ObjectNew: unrelated}) {
		t.Fatal("unrelated metadata invalidated the same provider signature")
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "custom-config"}}
	changed := config.DeepCopy()
	changed.Data = map[string]string{"enable-policy": "never"}
	if !networkConfigChanges().Update(event.UpdateEvent{ObjectOld: config, ObjectNew: changed}) {
		t.Fatal("custom referenced configuration change was ignored")
	}
	changed = config.DeepCopy()
	changed.BinaryData = map[string][]byte{"enable-k8s-networkpolicy": []byte("false")}
	if !networkConfigChanges().Update(event.UpdateEvent{ObjectOld: config, ObjectNew: changed}) {
		t.Fatal("binary configuration change did not invalidate previously observed settings")
	}
}
