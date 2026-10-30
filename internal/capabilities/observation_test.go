package capabilities

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

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
	sc.Parameters = map[string]string{"csi.storage.k8s.io/fstype": "ext4"}
	sc.AllowVolumeExpansion = ptr.To(true)
	sc.AllowedTopologies = []corev1.TopologySelectorTerm{{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: "zone", Values: []string{"a"}}}}}
	d := driver(sc.Provisioner)
	d.Spec.FSGroupPolicy = ptr.To(storagev1.FileFSGroupPolicy)
	d.Spec.VolumeLifecycleModes = []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent}
	node := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: d.Name, NodeID: "must-not-publish-node-id", TopologyKeys: []string{"zone"}}}}}
	c := testClient(t, linuxNode("worker"), rc, sc, d, node)
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Spec: v1.ClusterCapabilitiesSpec{
		RuntimeMappings: []v1.RuntimeMapping{{RuntimeClassName: rc.Name, Family: "custom-kata", Category: "VirtualMachine", ImageContract: "LinuxOCI"}, {RuntimeClassName: "missing", Family: "invented", Category: "Container", ImageContract: "LinuxOCI"}},
		StorageMappings: []v1.StorageMapping{{StorageClassName: sc.Name, Type: "Block"}},
	}}
	beforeRC, beforeSC := rc.DeepCopy(), sc.DeepCopy()
	observer := Observer{Reader: c}
	observer.Refresh(context.Background(), report, All, true)
	r := findRuntime(t, report.Status, rc.Name)
	s := findStorage(t, report.Status, sc.Name)
	if len(report.Status.Runtimes) != 1 || r.Source != "AdministratorDeclared" || r.Family != "custom-kata" || !reflect.DeepEqual(r.Scheduling, rc.Scheduling) || !reflect.DeepEqual(r.Overhead, rc.Overhead.PodFixed) {
		t.Fatal("runtime mapping lost native facts", r)
	}
	if s.Source != "AdministratorDeclared" || s.Type != "Block" || s.AllowVolumeExpansion == nil || !*s.AllowVolumeExpansion || !reflect.DeepEqual(s.AllowedTopologies, sc.AllowedTopologies) {
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
	if err := c.Delete(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	observer.Refresh(context.Background(), report, Storage, true)
	if s := findStorage(t, report.Status, sc.Name); s.Status != "Unknown" || s.DriverRegistered {
		t.Fatal("mapping bypassed missing native registration", s)
	}
	if err := c.Delete(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	observer.Refresh(t.Context(), report, Storage, true)
	if s := findStorage(t, report.Status, sc.Name); s.Status != "Unknown" || s.Type != "Block" || s.CSIDriver != "" || s.Reason == "" {
		t.Fatal("administrator classification invented a driver or lost its uncertainty reason", s)
	}
}

func TestDiscoveryUsesOnlyCurrentSectionsAndSkipsUnchangedAudits(t *testing.T) {
	base := testClient(t, linuxNode("worker"), csiRegistration("custom.example"))
	snapshotReads := 0
	reader := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if object, ok := list.(*unstructured.UnstructuredList); ok && object.GetKind() == "VolumeSnapshotClassList" {
			snapshotReads++
			return apierrors.NewForbidden(schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshotclasses"}, "", nil)
		}
		return c.List(ctx, list, opts...)
	}})
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	report := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	observer := Observer{Reader: reader, Now: func() time.Time { return now }}
	observer.Refresh(context.Background(), report, 0, false)
	if snapshotReads != 0 || len(report.Status.Sections) != 3 || len(report.Status.Conditions) != 3 {
		t.Fatal("discovery queried a snapshot API or published an unexpected section", snapshotReads, report.Status)
	}
	for _, section := range []Section{Runtime, Storage, Network} {
		if !SectionCurrent(report, section.Condition(), now) {
			t.Fatal("section audit did not renew native evidence", section)
		}
	}
	for _, capability := range report.Status.Capabilities {
		if capability.Name == "VolumeSnapshots" {
			t.Fatal("unrelated snapshot capability was published")
		}
	}
	before := report.Status.DeepCopy()
	observer.Reader = interceptor.NewClient(base, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		t.Fatal("an unchanged report forced a repeated audit")
		return nil
	}})
	observer.Refresh(t.Context(), report, 0, false)
	if !reflect.DeepEqual(before, &report.Status) {
		t.Fatal("an unchanged report renewed section evidence")
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

func TestNetworkCandidatesDoNotStartOptionalInformersOrRenewEvidence(t *testing.T) {
	ds := generatedCiliumDaemon()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}}
	base := testClient(t, ds, cm)
	optionalReads := 0
	api := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if list.GetObjectKind().GroupVersionKind().Kind == "CiliumNodeConfigList" {
			optionalReads++
		}
		return c.List(ctx, list, opts...)
	}})
	cache := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, optional := list.(*unstructured.UnstructuredList); optional {
			t.Fatal("cache candidate attempted to start an optional component informer")
		}
		return c.List(ctx, list, opts...)
	}})
	now := time.Now().UTC().Truncate(time.Second)
	report := &v1.ClusterCapabilities{}
	observer := Observer{Reader: api, Cache: cache, Now: func() time.Time { return now }}
	observer.Refresh(t.Context(), report, All, true)
	observed := report.Status.ObservedAt.DeepCopy()
	reads := optionalReads
	now = now.Add(time.Second)
	observer.Refresh(t.Context(), report, Network, false)
	if optionalReads != reads+1 || !report.Status.ObservedAt.Equal(observed) || report.Status.NetworkPolicy.Status != "Supported" {
		t.Fatal("unchanged network candidate renewed evidence or skipped optional override checks", report.Status.NetworkPolicy, optionalReads)
	}
}

func TestRuntimeCandidatesIgnoreHandlerOrderButRefreshFeatures(t *testing.T) {
	node := linuxNode("worker")
	node.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{{Name: "runc", Features: &corev1.NodeRuntimeHandlerFeatures{UserNamespaces: ptr.To(true)}}, {Name: "opaque"}}
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, Handler: "runc"}
	cache := testClient(t, node, rc)
	reads := 0
	api := interceptor.NewClient(cache, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		switch list.(type) {
		case *corev1.NodeList, *nodev1.RuntimeClassList:
			reads++
		}
		return c.List(ctx, list, opts...)
	}})
	now := time.Now().UTC().Truncate(time.Second)
	report := &v1.ClusterCapabilities{}
	observer := Observer{Reader: api, Cache: cache, Now: func() time.Time { return now }}
	observer.Refresh(t.Context(), report, All, true)
	before, beforeReads := report.Status.DeepCopy(), reads
	node.Status.RuntimeHandlers[0], node.Status.RuntimeHandlers[1] = node.Status.RuntimeHandlers[1], node.Status.RuntimeHandlers[0]
	if err := cache.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	observer.Refresh(t.Context(), report, Runtime, false)
	if reads != beforeReads || !reflect.DeepEqual(before, &report.Status) {
		t.Fatal("handler ordering caused an API audit or renewed evidence")
	}
	node.Status.RuntimeHandlers[1].Features.UserNamespaces = ptr.To(false)
	if err := cache.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	observer.Refresh(t.Context(), report, Runtime, false)
	if reads != beforeReads+2 || findRuntime(t, report.Status, rc.Name).Features.UserNamespaces.ReportedUnsupportedNodes != 1 {
		t.Fatal("handler feature change did not receive a direct audit")
	}
}
