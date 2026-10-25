//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

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
		for _, section := range []string{"RuntimeDiscoveryReady", "StorageDiscoveryReady", "NetworkPolicyDiscoveryReady"} {
			if !conditionTrue(report.Status.Conditions, section, report.Generation) {
				return false, client.IgnoreNotFound(err)
			}
		}
		return len(report.Status.Sections) == 3, client.IgnoreNotFound(err)
	})
	if report.Status.DefaultRuntime == nil || report.Status.DefaultRuntime.Status != "Unknown" || report.Status.DefaultRuntime.ImageContract != "Unknown" {
		t.Fatal("CRI identity was mistaken for the default executor", report.Status.DefaultRuntime)
	}
	if report.Status.CatalogRevision == "" || report.Status.NetworkPolicy == nil || report.Status.NetworkPolicy.EnforcementVerified {
		t.Fatal("catalog provenance or passive network contract missing")
	}
	if len(report.Status.Capabilities) == 0 {
		t.Fatal("CRD admission discarded the user-facing capability summary")
	}
	if os.Getenv("E2E_CNI") == "cilium" && report.Status.NetworkPolicy.Status != "Supported" {
		t.Fatal("installed Cilium policy configuration was not recognized", report.Status.NetworkPolicy)
	}
	if os.Getenv("E2E_CNI") == "kindnet" && report.Status.NetworkPolicy.Status != "Unknown" {
		t.Fatal("kindnet-only evidence became a global negative", report.Status.NetworkPolicy)
	}
	for _, condition := range report.Status.Conditions {
		if condition.Type == "SnapshotDiscoveryReady" || condition.Type == "DiscoveryReady" || condition.Type == "DiscoverySucceeded" {
			t.Fatal("retired discovery condition is still published", condition)
		}
	}
	for _, capability := range report.Status.Capabilities {
		if capability.Name == "VolumeSnapshots" {
			t.Fatal("global snapshot inventory is still published")
		}
	}

	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "e2e-native-runtime", Annotations: map[string]string{
		"arally.io/runtime-classification": `{"version":"v1","family":"self-described-runc","category":"Container","imageContract":"LinuxOCI"}`,
	}}, Handler: "runc",
		Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{corev1.LabelOSStable: "linux"}},
		Overhead:   &nodev1.Overhead{PodFixed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}},
	}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "e2e-declared-storage"}, Provisioner: "e2e.unregistered.example.com", AllowVolumeExpansion: ptr.To(true)}
	driver := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: sc.Provisioner}}
	create(t, ctx, kube, rc)
	create(t, ctx, kube, sc)
	create(t, ctx, kube, driver)
	t.Cleanup(func() {
		_ = kube.Delete(ctx, rc)
		_ = kube.Delete(ctx, sc)
		_ = kube.Delete(ctx, driver)
		report.Spec = v1.ClusterCapabilitiesSpec{}
		update(t, ctx, kube, report)
	})
	await(t, "RuntimeClass declaration and native handler coverage", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
			return false, err
		}
		for _, observed := range report.Status.Runtimes {
			if observed.Name != rc.Name || observed.Source != "RuntimeClassDeclared" || observed.Family != "self-described-runc" || observed.HandlerEvidence == nil || observed.Features == nil {
				continue
			}
			evidence := observed.HandlerEvidence
			return evidence.ReportedNodes+evidence.MissingFromReportedInventoryNodes+evidence.NoInventoryNodes == observed.EligibleNodes && observed.EligibleNodes > 0, nil
		}
		return false, nil
	})
	verifyNativeHandlerInventory(t, ctx, kube, report)
	// A malformed declaration cannot silently fall back to the known runc alias.
	rc.Annotations["arally.io/runtime-classification"] = "invalid"
	if err := kube.Update(ctx, rc); err != nil {
		t.Fatal(err)
	}
	await(t, "invalid declaration observed through the RuntimeClass watch", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
			return false, err
		}
		for _, observed := range report.Status.Runtimes {
			if observed.Name == rc.Name {
				return observed.Status == "Unknown" && observed.Reason == "RuntimeDeclarationInvalid", nil
			}
		}
		return false, nil
	})
	report.Spec.RuntimeMappings = []v1.RuntimeMapping{{RuntimeClassName: rc.Name, Family: "custom-runc", Category: "Container", ImageContract: "LinuxOCI"}}
	report.Spec.StorageMappings = []v1.StorageMapping{{StorageClassName: sc.Name, Type: "Filesystem"}}
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
				storageOK = observed.Source == "AdministratorDeclared" && observed.Type == "Filesystem" && observed.Status == "Unknown" && observed.DriverDeclared && !observed.DriverRegistered && observed.AllowVolumeExpansion != nil && *observed.AllowVolumeExpansion
			}
		}
		grouped := false
		for _, capability := range report.Status.Capabilities {
			if capability.Name == "Filesystem" {
				for _, resource := range capability.Resources {
					grouped = grouped || resource.Name == sc.Name && resource.Status == "Unknown" && resource.Source == "AdministratorDeclared"
				}
			}
		}
		return report.Status.ObservedGeneration == report.Generation && runtimeOK && storageOK && grouped, nil
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
	kubectl(t, kubeconfig, "-n", "agentrally-system", "rollout", "restart", "deployment/agentrally-operator")
	kubectl(t, kubeconfig, "-n", "agentrally-system", "rollout", "status", "deployment/agentrally-operator", "--timeout=180s")
	await(t, "replacement manager's fresh direct audit", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report)
		return len(report.Status.Sections) == 3 && report.Status.Sections[0].LastSuccessAt != nil && report.Status.Sections[0].LastSuccessAt.After(before.Time), err
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

// Compare with real Node status, rather than assuming a particular CRI version
// always publishes handler inventory. Missing inventory is a valid old-CRI case.
func verifyNativeHandlerInventory(t *testing.T, ctx context.Context, kube client.Client, report *v1.ClusterCapabilities) {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := kube.List(ctx, nodes); err != nil {
		t.Fatal(err)
	}
	want := map[string]int32{}
	for _, node := range nodes.Items {
		if node.DeletionTimestamp != nil {
			continue
		}
		for _, handler := range node.Status.RuntimeHandlers {
			want[handler.Name]++
		}
	}
	got := map[string]int32{}
	for _, observed := range report.Status.RuntimeHandlers {
		got[observed.Name] = observed.ReportedNodes
		for _, feature := range []v1.RuntimeFeatureObservation{observed.Features.UserNamespaces, observed.Features.RecursiveReadOnlyMounts} {
			if feature.ReportedSupportedNodes+feature.ReportedUnsupportedNodes+feature.UnknownNodes != observed.ReportedNodes {
				t.Fatal("native feature coverage was lost in the published report", observed)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("handler inventory differs from native Node reports: got %v, want %v", got, want)
	}
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
