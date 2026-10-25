//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNetworkPolicyTraffic(t *testing.T) {
	if os.Getenv("E2E_CNI") != "cilium" {
		t.Skip("enable with E2E_CNI=cilium make e2e")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	group := readyGroup(t, ctx, kube, "network-probe")
	serverDefinition, server := testAgent(group, "server", "mkdir -p /tmp/www; printf probe-ok > /tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www")
	serverDefinition.Spec.Container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1}
	callerDefinition, caller := testAgent(group, "caller", "sleep 3600")
	create(t, ctx, kube, serverDefinition)
	create(t, ctx, kube, server)
	create(t, ctx, kube, callerDefinition)
	create(t, ctx, kube, caller)
	readyAgent(t, ctx, kube, server)
	readyAgent(t, ctx, kube, caller)
	pod := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: server.Namespace, Name: server.Status.PodName}, pod); err != nil {
		t.Fatal(err)
	}
	url := "http://" + pod.Status.PodIP + ":8080/"
	probe := func(want string, consecutive int) {
		t.Helper()
		matches := 0
		await(t, "traffic "+want, func() (bool, error) {
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			command := exec.CommandContext(probeCtx, "kubectl", "--kubeconfig", kubeconfig, "-n", caller.Namespace, "exec", caller.Status.PodName, "--",
				"sh", "-c", `if wget -q -T 1 -O /tmp/probe-response "$1" 2>/dev/null; then cat /tmp/probe-response; else printf denied; fi`, "probe", url)
			out, err := command.CombinedOutput()
			if err != nil {
				return false, fmt.Errorf("test client exec: %w: %s", err, out)
			}
			if strings.TrimSpace(string(out)) == want {
				matches++
			} else {
				matches = 0
			}
			return matches >= consecutive, nil
		})
	}
	t.Log("Baseline: application traffic succeeds before isolation")
	probe("probe-ok", 1)
	group.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}
	update(t, ctx, kube, group)
	t.Log("Deny: the Group policy blocks three consecutive new connections")
	probe("denied", 3)
	group.Spec.NetworkPolicy.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}, Ports: []networkingv1.NetworkPolicyPort{{Port: ptr.To(intstr.FromInt32(8080))}}}}
	update(t, ctx, kube, group)
	t.Log("Allow: the updated Group policy permits the same application path")
	probe("probe-ok", 3)
	report := &v1.ClusterCapabilities{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
		t.Fatal(err)
	}
	if report.Status.NetworkPolicy == nil || report.Status.NetworkPolicy.Status != "Supported" || report.Status.NetworkPolicy.EnforcementVerified {
		t.Fatal("passive report did not preserve the configuration/probe boundary", report.Status.NetworkPolicy)
	}
	for _, claim := range []*v1.AgentInstance{server, caller} {
		if err := kube.Delete(ctx, claim); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, claim)
	}
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func TestSnapshotWorkspaceRestore(t *testing.T) {
	if os.Getenv("E2E_SNAPSHOTS") != "true" {
		t.Skip("enable with E2E_SNAPSHOTS=true make e2e")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "e2e-hostpath"}, Provisioner: "hostpath.csi.k8s.io"}
	create(t, ctx, kube, sc)
	group := readyGroup(t, ctx, kube, "snapshot-probe")
	sourceDefinition, source := testAgent(group, "source", "printf snapshot-seed > /workspace/marker; sync; sleep 3600")
	source.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{
		StorageClassName: &sc.Name, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}},
	}}
	create(t, ctx, kube, sourceDefinition)
	create(t, ctx, kube, source)
	readyAgent(t, ctx, kube, source)
	if got := execPod(t, kubeconfig, source.Namespace, source.Status.PodName, "cat", "/workspace/marker"); got != "snapshot-seed" {
		t.Fatal("source data was not written", got)
	}
	pvc := allocatedWorkspace(t, ctx, kube, source)
	source.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, source)
	await(t, "quiesced source workspace", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(source), source)
		return conditionTrue(source.Status.Conditions, "Suspended", source.Generation), err
	})
	snapshot := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshot",
		"metadata": map[string]interface{}{"name": "workspace-seed", "namespace": source.Namespace},
		"spec":     map[string]interface{}{"volumeSnapshotClassName": "csi-hostpath-snapclass", "source": map[string]interface{}{"persistentVolumeClaimName": pvc.Name}},
	}}
	create(t, ctx, kube, snapshot)
	await(t, "CSI snapshot readyToUse", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot); err != nil {
			return false, err
		}
		ready, _, err := unstructured.NestedBool(snapshot.Object, "status", "readyToUse")
		return ready, err
	})
	restoredDefinition, restored := testAgent(group, "restored", "test -f /workspace/marker; sleep 3600")
	restored.Spec.Workspace = source.Spec.Workspace.DeepCopy()
	restored.Spec.Workspace.VolumeClaimTemplate.DataSource = &corev1.TypedLocalObjectReference{APIGroup: ptr.To("snapshot.storage.k8s.io"), Kind: "VolumeSnapshot", Name: snapshot.GetName()}
	create(t, ctx, kube, restoredDefinition)
	create(t, ctx, kube, restored)
	readyAgent(t, ctx, kube, restored)
	if got := execPod(t, kubeconfig, restored.Namespace, restored.Status.PodName, "cat", "/workspace/marker"); got != "snapshot-seed" {
		t.Fatal("restored workspace lost snapshot data", got)
	}
	restoredPVC := allocatedWorkspace(t, ctx, kube, restored)
	if restoredPVC.UID == pvc.UID || restoredPVC.Spec.VolumeName == pvc.Spec.VolumeName {
		t.Fatal("restore reused the source volume")
	}
	// Native CSI registration remains part of storage discovery; snapshot source
	// validation and this restore test work without global snapshot inventory.
	report := &v1.ClusterCapabilities{}
	await(t, "CSI driver registration", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
			return false, err
		}
		for _, driver := range report.Status.CSIDriverDetails {
			if driver.Name == sc.Provisioner && driver.RegisteredNodes > 0 {
				return true, nil
			}
		}
		return false, nil
	})
	sandboxName, podName := restored.Status.SandboxName, restored.Status.PodName
	restored.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, restored)
	await(t, "suspended restored workspace", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(restored), restored)
		return conditionTrue(restored.Status.Conditions, "Suspended", restored.Generation), err
	})
	if err := kube.Delete(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, snapshot)
	restored.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, restored)
	readyAgent(t, ctx, kube, restored)
	if restored.Status.SandboxName != sandboxName {
		t.Fatal("snapshot deletion replaced the restored execution")
	}
	if current := allocatedWorkspace(t, ctx, kube, restored); current.UID != restoredPVC.UID || current.Spec.VolumeName != restoredPVC.Spec.VolumeName {
		t.Fatal("resume replaced the restored workspace")
	}
	if got := execPod(t, kubeconfig, restored.Namespace, restored.Status.PodName, "cat", "/workspace/marker"); got != "snapshot-seed" {
		t.Fatal("resume after snapshot deletion lost restored data", got)
	}
	t.Logf("Restored execution %s resumed after source deletion; workspace %s retained (previous Pod %s)", sandboxName, restoredPVC.Name, podName)
	for _, claim := range []*v1.AgentInstance{source, restored} {
		if err := kube.Delete(ctx, claim); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, claim)
	}
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
	if err := kube.Delete(ctx, sc); err != nil {
		t.Fatal(err)
	}
}
