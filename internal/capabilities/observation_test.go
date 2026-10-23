package capabilities

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMappingsPreserveNativeEvidenceAndDoNotInventCapabilities(t *testing.T) {
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "custom"}, Handler: "opaque", Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "agents"}}, Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}}}
	sc := class("workspace", "custom.example")
	sc.AllowVolumeExpansion = ptr.To(true)
	sc.AllowedTopologies = []corev1.TopologySelectorTerm{{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: "zone", Values: []string{"a"}}}}}
	d := driver(sc.Provisioner)
	d.Spec.FSGroupPolicy = ptr.To(storagev1.FileFSGroupPolicy)
	d.Spec.VolumeLifecycleModes = []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent}
	node := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: d.Name, NodeID: "must-not-publish-node-id", TopologyKeys: []string{"zone"}}}}}
	c := testClient(t, linuxNode("worker"), rc, sc, d, node)
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Spec: v1.ClusterCapabilitiesSpec{
		RuntimeMappings: []v1.RuntimeMapping{{RuntimeClassName: rc.Name, Family: "custom-kata", Category: "VirtualMachine", ImageContract: "LinuxOCI"}, {RuntimeClassName: "missing", Family: "invented", Category: "Container", ImageContract: "LinuxOCI"}},
		StorageMappings: []v1.StorageMapping{{StorageClassName: sc.Name, Type: "NFS"}},
	}}
	beforeRC, beforeSC := rc.DeepCopy(), sc.DeepCopy()
	observer := Observer{Reader: c}
	observer.Refresh(context.Background(), report, All, true)
	r := findRuntime(t, report.Status, rc.Name)
	s := findStorage(t, report.Status, sc.Name)
	if len(report.Status.Runtimes) != 1 || r.Source != "AdministratorDeclared" || r.Family != "custom-kata" || !reflect.DeepEqual(r.Scheduling, rc.Scheduling) || !reflect.DeepEqual(r.Overhead, rc.Overhead.PodFixed) {
		t.Fatal("runtime mapping lost native facts", r)
	}
	if s.Source != "AdministratorDeclared" || s.Type != "NFS" || s.AllowVolumeExpansion == nil || !*s.AllowVolumeExpansion || !reflect.DeepEqual(s.AllowedTopologies, sc.AllowedTopologies) {
		t.Fatal("storage mapping lost native facts", s)
	}
	details := report.Status.CSIDriverDetails[0]
	if details.RegisteredNodes != 1 || !reflect.DeepEqual(details.TopologyKeys, []string{"zone"}) || details.FSGroupPolicy == nil || *details.FSGroupPolicy != storagev1.FileFSGroupPolicy {
		t.Fatal("CSI registration/behavior lost", details)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rc), rc); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sc), sc); err != nil {
		t.Fatal(err)
	}
	beforeRC.ResourceVersion = rc.ResourceVersion
	beforeSC.ResourceVersion = sc.ResourceVersion
	if !reflect.DeepEqual(beforeRC, rc) || !reflect.DeepEqual(beforeSC, sc) {
		t.Fatal("discovery changed native configuration")
	}
	if err := c.Delete(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	observer.Refresh(context.Background(), report, Storage, true)
	if s := findStorage(t, report.Status, sc.Name); s.Status != "Unknown" || s.DriverRegistered {
		t.Fatal("mapping bypassed missing native registration", s)
	}
}

func TestOptionalSnapshotAPIAndSectionFailureIsolation(t *testing.T) {
	base := testClient(t, linuxNode("worker"))
	snapshotState := "missing"
	reader := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if object, ok := list.(*unstructured.UnstructuredList); ok && object.GetKind() == "VolumeSnapshotClassList" {
			switch snapshotState {
			case "missing":
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "snapshot.storage.k8s.io", Kind: "VolumeSnapshotClass"}}
			case "forbidden":
				return apierrors.NewForbidden(schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshotclasses"}, "", nil)
			default:
				object.Items = []unstructured.Unstructured{{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "seed", "annotations": map[string]interface{}{"snapshot.storage.kubernetes.io/is-default-class": "true"}}, "driver": "custom.example", "deletionPolicy": "Retain"}}}
				return nil
			}
		}
		return c.List(ctx, list, opts...)
	}})
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	observer := Observer{Reader: reader, Now: func() time.Time { return now }}
	observer.Refresh(context.Background(), report, All, true)
	if report.Status.Snapshots.ClassAPIStatus != "Unavailable" || !SectionCurrent(report, Storage.Condition(), now) {
		t.Fatal("optional API prevented native discovery", report.Status)
	}
	snapshotState = "installed"
	now = now.Add(RefreshInterval)
	observer.Refresh(context.Background(), report, 0, false)
	if report.Status.Snapshots.ClassAPIStatus != "Available" || len(report.Status.Snapshots.Classes) != 1 || !report.Status.Snapshots.Classes[0].Default {
		t.Fatal("late installation did not recover", report.Status.Snapshots)
	}
	snapshotState = "forbidden"
	observer.Refresh(context.Background(), report, Snapshots, true)
	if report.Status.Snapshots.ClassAPIStatus != "Unknown" || len(report.Status.Snapshots.Classes) != 0 || meta.FindStatusCondition(report.Status.Conditions, Snapshots.Condition()).Reason != "PermissionDenied" || !SectionCurrent(report, Runtime.Condition(), now) {
		t.Fatal("snapshot failure leaked stale success or invalidated unrelated sections")
	}
	// A refreshed storage audit cannot renew runtime evidence.
	now = now.Add(capabilityMaxAge + time.Second)
	observer.Refresh(context.Background(), report, Storage, false)
	// Refresh also audits overdue sections. Exercise consumer isolation explicitly.
	for i := range report.Status.Sections {
		if report.Status.Sections[i].Name == Runtime.Condition() {
			report.Status.Sections[i].LastSuccessAt = ptr.To(metav1.NewTime(now.Add(-3 * time.Minute)))
		}
	}
	if SectionCurrent(report, Runtime.Condition(), now) || !SectionCurrent(report, Storage.Condition(), now) {
		t.Fatal("another section's timestamp renewed stale runtime evidence")
	}
}

func TestSectionTimeoutDoesNotStarveLaterSections(t *testing.T) {
	base := testClient(t)
	reader := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.NodeList); ok {
			<-ctx.Done()
			return ctx.Err()
		}
		if _, ok := list.(*storagev1.StorageClassList); ok {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < sectionTimeout-time.Second {
				t.Fatal("runtime timeout consumed storage's read budget")
			}
		}
		return c.List(ctx, list, opts...)
	}})
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	delay := (Observer{Reader: reader}).Refresh(context.Background(), report, All, true)
	if meta.FindStatusCondition(report.Status.Conditions, Runtime.Condition()).Reason != "DiscoveryTimeout" ||
		!SectionCurrent(report, Storage.Condition(), time.Now()) || !SectionCurrent(report, Network.Condition(), time.Now()) {
		t.Fatal("one timed-out section prevented later observations", report.Status.Conditions)
	}
	if delay > retryInterval+time.Second {
		t.Fatal("time spent scanning extended the failed section's retry delay", delay)
	}
}
