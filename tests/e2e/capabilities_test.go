//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestClusterCapabilities(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	report := &v1.ClusterCapabilities{}
	await(t, "canonical report with independent section audits", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report)
		return conditionTrue(report.Status.Conditions, "DiscoveryReady", report.Generation) && len(report.Status.Sections) == 4, client.IgnoreNotFound(err)
	})
	if report.Status.DefaultRuntime == nil || report.Status.DefaultRuntime.Status != "Unknown" || report.Status.DefaultRuntime.ImageContract != "Unknown" {
		t.Fatal("CRI identity was mistaken for the default executor", report.Status.DefaultRuntime)
	}
	if report.Status.CatalogRevision == "" || report.Status.NetworkPolicy == nil || report.Status.NetworkPolicy.EnforcementVerified {
		t.Fatal("catalog provenance or passive network contract missing")
	}
	if os.Getenv("E2E_CNI") == "kindnet" && report.Status.NetworkPolicy.Status != "Unknown" {
		t.Fatal("kindnet-only evidence became a global negative", report.Status.NetworkPolicy)
	}
	wantSnapshots := "Unavailable"
	if os.Getenv("E2E_SNAPSHOTS") == "true" {
		wantSnapshots = "Available"
	}
	if report.Status.Snapshots == nil || report.Status.Snapshots.ClassAPIStatus != wantSnapshots {
		t.Fatalf("snapshot class API observation: %#v, want %s", report.Status.Snapshots, wantSnapshots)
	}

	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "e2e-native-runtime"}, Handler: "runc",
		Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{corev1.LabelOSStable: "linux"}},
		Overhead:   &nodev1.Overhead{PodFixed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}},
	}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "e2e-declared-storage"}, Provisioner: "example.invalid/unregistered", AllowVolumeExpansion: ptr.To(true)}
	create(t, ctx, kube, rc)
	create(t, ctx, kube, sc)
	t.Cleanup(func() {
		_ = kube.Delete(ctx, rc)
		_ = kube.Delete(ctx, sc)
		report.Spec = v1.ClusterCapabilitiesSpec{}
		update(t, ctx, kube, report)
	})
	report.Spec.RuntimeMappings = []v1.RuntimeMapping{{RuntimeClassName: rc.Name, Family: "custom-runc", Category: "Container", ImageContract: "LinuxOCI"}}
	report.Spec.StorageMappings = []v1.StorageMapping{{StorageClassName: sc.Name, Type: "NFS"}}
	update(t, ctx, kube, report)
	await(t, "administrator mappings preserving native evidence", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
			return false, err
		}
		runtimeOK, storageOK := false, false
		for _, observed := range report.Status.Runtimes {
			if observed.Name == rc.Name {
				runtimeOK = observed.Source == "AdministratorDeclared" && observed.Family == "custom-runc" && observed.EligibleNodes > 0 &&
					reflect.DeepEqual(observed.Scheduling, rc.Scheduling) && observed.Overhead.Memory().Cmp(resource.MustParse("16Mi")) == 0
			}
		}
		for _, observed := range report.Status.StorageClasses {
			if observed.Name == sc.Name {
				storageOK = observed.Source == "AdministratorDeclared" && observed.Type == "NFS" && observed.Status == "Unknown" && !observed.DriverRegistered && observed.AllowVolumeExpansion != nil && *observed.AllowVolumeExpansion
			}
		}
		return report.Status.ObservedGeneration == report.Generation && runtimeOK && storageOK, nil
	})
	// This is a real runc execution check, scoped to this handler and image.
	group := readyGroup(t, ctx, kube, "runtime-probe")
	definition, claim := testAgent(group, "runtime", "sleep 3600")
	definition.Spec.Container.RuntimeClassName = &rc.Name
	create(t, ctx, kube, definition)
	create(t, ctx, kube, claim)
	readyAgent(t, ctx, kube, claim)
	if got := execPod(t, kubeconfig, claim.Namespace, claim.Status.PodName, "uname", "-s"); got != "Linux" {
		t.Fatal("runtime probe did not execute", got)
	}
	before := report.Status.Sections[0].LastSuccessAt.DeepCopy()
	kubectl(t, kubeconfig, "-n", "arally-system", "rollout", "restart", "deployment/arally-operator")
	kubectl(t, kubeconfig, "-n", "arally-system", "rollout", "status", "deployment/arally-operator", "--timeout=180s")
	await(t, "replacement manager's fresh direct audit", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report)
		return len(report.Status.Sections) == 4 && report.Status.Sections[0].LastSuccessAt != nil && report.Status.Sections[0].LastSuccessAt.After(before.Time), err
	})
	if err := kube.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, claim)
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func readyGroup(t *testing.T, ctx context.Context, kube client.Client, name string) *v1.AgentGroup {
	t.Helper()
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
	create(t, ctx, kube, group)
	await(t, name+" Group readiness", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	return group
}

func testAgent(group *v1.AgentGroup, name, command string) (*v1.AgentDefinition, *v1.AgentInstance) {
	definition := &v1.AgentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: group.Status.Namespace}, Spec: v1.AgentDefinitionSpec{AgentGroupRef: v1.AgentGroupReference{Name: group.Name}, Template: "generic", Security: &v1.AgentSecurity{RunAsUser: ptr.To(int64(1000))}, Container: &v1.AgentContainerSpec{Image: "docker.io/library/busybox:1.37.0", Command: []string{"sh", "-ec"}, Args: []string{command}}}}

	return definition, &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: group.Status.Namespace}, Spec: v1.AgentInstanceSpec{
		AgentGroupRef: v1.AgentGroupReference{Name: group.Name}, DefinitionRef: v1.AgentDefinitionReference{Name: definition.Name},
	}}
}

func readyAgent(t *testing.T, ctx context.Context, kube client.Client, claim *v1.AgentInstance) {
	t.Helper()
	await(t, claim.Name+" Agent readiness", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(claim), claim)
		return conditionTrue(claim.Status.Conditions, "Ready", claim.Generation), err
	})
}
