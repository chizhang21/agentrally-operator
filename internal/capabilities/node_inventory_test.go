package capabilities

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNodeInventoryPreservesPlatformsAndDeclaredResources(t *testing.T) {
	a, b, arm, windows, unknown, deleted := linuxNode("a"), linuxNode("b"), linuxNode("arm"), linuxNode("windows"), linuxNode("unknown"), linuxNode("deleted")
	for _, node := range []*corev1.Node{a, b, arm, windows, deleted} {
		node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.Architecture = "linux", "amd64"
	}
	b.Spec.Unschedulable = true
	b.Status.Conditions[0].Status = corev1.ConditionFalse
	arm.Status.NodeInfo.Architecture = "arm64"
	windows.Status.NodeInfo.OperatingSystem = "windows"
	// Platform comes from NodeInfo even when labels disagree or only labels exist.
	arm.Labels[corev1.LabelArchStable] = "amd64"
	a.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi"),
		"hugepages-2Mi": resource.MustParse("1Gi"), "kubernetes.io/example": resource.MustParse("9"),
		"nvidia.com/gpu": resource.MustParse("4"), "example.com/license": resource.MustParse("8"),
	}
	a.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("3")}
	b.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}
	b.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("0")}
	windows.Status.Allocatable = corev1.ResourceList{"vendor.example/accelerator": resource.MustParse("0")}
	deletion := metav1.Now()
	deleted.DeletionTimestamp, deleted.Finalizers = &deletion, []string{"keep"}
	deleted.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("100")}
	kube := testClient(t, windows, b, unknown, deleted, arm, a)
	status := Discover(t.Context(), kube)
	wantPlatforms := []v1.NodePlatformObservation{
		{OS: "", Architecture: "", Nodes: 1},
		{OS: "linux", Architecture: "amd64", Nodes: 2},
		{OS: "linux", Architecture: "arm64", Nodes: 1},
		{OS: "windows", Architecture: "amd64", Nodes: 1},
	}
	if !reflect.DeepEqual(status.Platforms, wantPlatforms) {
		t.Fatalf("node platforms were inferred, dropped or miscounted: %+v", status.Platforms)
	}
	wantResources := []v1.NodeResourceObservation{
		{Name: "example.com/license", Nodes: 1, Capacity: resource.MustParse("8"), Allocatable: resource.MustParse("0")},
		{Name: "nvidia.com/gpu", Nodes: 2, Capacity: resource.MustParse("6"), Allocatable: resource.MustParse("3")},
		{Name: "vendor.example/accelerator", Nodes: 1, Capacity: resource.MustParse("0"), Allocatable: resource.MustParse("0")},
	}
	if len(status.ExtendedResources) != len(wantResources) {
		t.Fatalf("extended resources were lost or included native resources: %+v", status.ExtendedResources)
	}
	for i, want := range wantResources {
		got := status.ExtendedResources[i]
		if got.Name != want.Name || got.Nodes != want.Nodes || got.Capacity.Cmp(want.Capacity) != 0 || got.Allocatable.Cmp(want.Allocatable) != 0 {
			t.Fatalf("declared quantities changed: got %+v, want %+v", got, want)
		}
	}
	// A complete discovery refresh must not accumulate previous totals.
	if next := Discover(t.Context(), kube); !reflect.DeepEqual(status.Platforms, next.Platforms) || !reflect.DeepEqual(status.ExtendedResources, next.ExtendedResources) {
		t.Fatal("unchanged discovery changed node inventory")
	}
}

func TestNodeInventoryChangesAuditAndClearEvidence(t *testing.T) {
	node := linuxNode("worker")
	node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.Architecture = "linux", "amd64"
	node.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), "nvidia.com/gpu": resource.MustParse("4")}
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("3")}
	kube := testClient(t, node)
	reads, failed := 0, false
	api := interceptor.NewClient(kube, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		switch list.(type) {
		case *corev1.NodeList, *nodev1.RuntimeClassList:
			reads++
			if failed {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
			}
		}
		return c.List(ctx, list, opts...)
	}})
	now := time.Now().UTC().Truncate(time.Second)
	observer := Observer{Reader: api, Cache: kube, Now: func() time.Time { return now }}
	report := &v1.ClusterCapabilities{}
	observer.Refresh(t.Context(), report, All, true)
	for _, test := range []struct {
		name   string
		mutate func()
		audit  bool
	}{
		{"native-resource", func() { node.Status.Capacity[corev1.ResourceCPU] = resource.MustParse("16") }, false},
		{"architecture", func() { node.Status.NodeInfo.Architecture = "arm64" }, true},
		{"os", func() { node.Status.NodeInfo.OperatingSystem = "windows" }, true},
		{"capacity", func() { node.Status.Capacity["nvidia.com/gpu"] = resource.MustParse("6") }, true},
		{"allocatable", func() { node.Status.Allocatable["nvidia.com/gpu"] = resource.MustParse("0") }, true},
		{"remove-resource", func() {
			delete(node.Status.Capacity, "nvidia.com/gpu")
			delete(node.Status.Allocatable, "nvidia.com/gpu")
		}, true},
		{"add-resource", func() { node.Status.Capacity["example.com/device"] = resource.MustParse("1") }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, beforeReads := report.Status.DeepCopy(), reads
			test.mutate()
			if err := kube.Status().Update(t.Context(), node); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Second)
			observer.Refresh(t.Context(), report, Runtime, false)
			if test.audit {
				if reads != beforeReads+2 || !report.Status.ObservedAt.After(before.ObservedAt.Time) {
					t.Fatal("changed node inventory did not receive a direct audit")
				}
			} else if reads != beforeReads || !reflect.DeepEqual(before, &report.Status) {
				t.Fatal("unrelated node resource changed the report or renewed evidence")
			}
		})
	}
	if len(report.Status.Platforms) != 1 || report.Status.Platforms[0].OS != "windows" || report.Status.Platforms[0].Architecture != "arm64" ||
		len(report.Status.ExtendedResources) != 1 || report.Status.ExtendedResources[0].Name != "example.com/device" {
		t.Fatal("node changes did not replace previous observations", report.Status)
	}
	lastSuccess := sectionObservation(report.Status, Runtime).LastSuccessAt.DeepCopy()
	failed = true
	now = now.Add(RefreshInterval)
	observer.Refresh(t.Context(), report, Runtime, false)
	if len(report.Status.Platforms) != 0 || len(report.Status.ExtendedResources) != 0 ||
		meta.FindStatusCondition(report.Status.Conditions, Runtime.Condition()).Status != metav1.ConditionFalse ||
		!sectionObservation(report.Status, Runtime).LastSuccessAt.Equal(lastSuccess) {
		t.Fatal("failed node audit retained inventory or renewed successful evidence", report.Status)
	}
}
