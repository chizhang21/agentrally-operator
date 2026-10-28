package capabilities

import (
	"context"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCapabilitySummaryPreservesChoiceUncertaintyAndMappingEvidence(t *testing.T) {
	available := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-ready"}, Handler: "kata-qemu"}
	unavailable := available.DeepCopy()
	unavailable.Name = "kata-unavailable"
	unavailable.Scheduling = &nodev1.Scheduling{NodeSelector: map[string]string{"missing": "label"}}
	alias := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "custom"}, Handler: "opaque"}
	sc := class("files", "smb.csi.k8s.io")
	kube := testClient(t, linuxNode("worker"), available, unavailable, alias, sc, driver(sc.Provisioner), csiRegistration(sc.Provisioner), daemon("calico-node", "calico-node"))
	report := &v1.ClusterCapabilities{Spec: v1.ClusterCapabilitiesSpec{RuntimeMappings: []v1.RuntimeMapping{
		{RuntimeClassName: alias.Name, Family: "declared-gvisor", Category: "UserSpaceKernel", ImageContract: "LinuxOCI"},
	}}}
	observer := Observer{Reader: kube}
	observer.Refresh(t.Context(), report, All, true)
	find := func(name string) *v1.CapabilityObservation {
		t.Helper()
		for i := range report.Status.Capabilities {
			if report.Status.Capabilities[i].Name == name {
				return &report.Status.Capabilities[i]
			}
		}
		t.Fatalf("capability %s missing", name)
		return nil
	}
	vm := find("VirtualMachineIsolation")
	if vm.Status != "Observed" || len(vm.Resources) != 2 || vm.Resources[0].Status != "Observed" || vm.Resources[1].Status != "Unknown" || vm.Resources[1].Reason != "NoEligibleNodes" {
		t.Fatalf("grouping hid a choice's uncertainty: %+v", vm)
	}
	applicationKernel := find("UserSpaceKernelIsolation")
	if applicationKernel.Resources[0].Source != "AdministratorDeclared" || applicationKernel.Resources[0].Name != alias.Name {
		t.Fatal("summary lost mapping evidence", applicationKernel)
	}
	if files := find("Filesystem"); files.Status != "Observed" || files.Resources[0].Name != sc.Name {
		t.Fatal("SMB was not grouped as file storage", files)
	}
	if find("NetworkPolicy").Status != "Observed" || report.Status.NetworkPolicy.EnforcementVerified || find("DefaultRuntime").Status != "Unknown" {
		t.Fatal("summary promoted passive discovery into verification")
	}
	observer.Reader = interceptor.NewClient(kube, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*nodev1.RuntimeClassList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Group: "node.k8s.io", Resource: "runtimeclasses"}, "", nil)
		}
		return c.List(ctx, list, opts...)
	}})
	observer.Refresh(t.Context(), report, Runtime, true)
	for _, capability := range report.Status.Capabilities {
		if capability.Category == "Isolation" {
			t.Fatal("failed discovery retained previous isolation summary", capability)
		}
	}
	if find("Filesystem").Status != "Observed" {
		t.Fatal("runtime failure invalidated independent file storage evidence")
	}
}
