//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Both component upgrades exercise the same existing allocations: a running
// private workspace and a suspended instance using an independently owned PVC.
type componentUpgradeFixture struct {
	group        *v1.AgentGroup
	instances    []*v1.AgentInstance
	identities   []client.Object
	observations []upgradeObservation
}

type upgradeObservation struct {
	instance *v1.AgentInstance
	pod      corev1.Pod
	claim    *unstructured.Unstructured
}

func prepareComponentUpgrade(t *testing.T, kube client.Client, kubeconfig string) *componentUpgradeFixture {
	t.Helper()
	ctx := t.Context()
	f := &componentUpgradeFixture{group: readyGroup(t, ctx, kube, "component-upgrade")}
	definition, running := testAgent(f.group, "running", agentCommand("upgrade"))
	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}},
	}
	running.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: pvcSpec.DeepCopy()}
	external := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "external-workspace", Namespace: running.Namespace}, Spec: pvcSpec}
	create(t, ctx, kube, external)
	suspended := running.DeepCopy()
	suspended.Name = "suspended"
	suspended.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: external.Name}, Exclusive: true}
	create(t, ctx, kube, definition)
	f.instances = []*v1.AgentInstance{running, suspended}
	f.identities = []client.Object{f.group, definition}
	for _, instance := range f.instances {
		create(t, ctx, kube, instance)
		readyAgent(t, ctx, kube, instance)
		execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "sh", "-ec", "printf retained-through-upgrade > /workspace/session; sync")
		pod := &corev1.Pod{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.PodName}, pod); err != nil {
			t.Fatal(err)
		}
		t.Logf("Original %s binding: claim=%s/%s sandbox=%s/%s workspace=%+v volumes=%v", instance.Name,
			instance.Status.Provisioning.ClaimName, instance.Status.Provisioning.ClaimUID,
			instance.Status.SandboxName, instance.Status.SandboxUID, instance.Status.Workspace, instance.Status.VolumeClaims)
		claim := sandbox.ExtensionObject("SandboxClaim")
		claim.SetNamespace(instance.Namespace)
		claim.SetName(instance.Status.Provisioning.ClaimName)
		f.observations = append(f.observations, upgradeObservation{instance.DeepCopy(), *pod, claim})
		workload := sandbox.Object()
		workload.SetNamespace(instance.Namespace)
		workload.SetName(instance.Status.SandboxName)
		f.identities = append(f.identities, instance.DeepCopy(), claim, workload)
	}
	f.identities = append(f.identities, allocatedWorkspace(t, ctx, kube, running), external)
	for _, object := range f.identities {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
		t.Logf("Retain %T %s UID=%s", object, object.GetName(), object.GetUID())
	}
	suspendAgent(t, kube, suspended)
	f.verify(t, kube, kubeconfig)
	return f
}

func (f *componentUpgradeFixture) verifyIdentities(t *testing.T, kube client.Client) {
	t.Helper()
	for _, before := range f.identities {
		after := before.DeepCopyObject().(client.Object)
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(after), after); err != nil {
			t.Fatal(err)
		}
		if after.GetUID() != before.GetUID() {
			t.Fatalf("component upgrade replaced %T %s", before, before.GetName())
		}
		if claim, ok := before.(*unstructured.Unstructured); ok && claim.GetKind() == "SandboxClaim" {
			original, err := sandbox.ClaimSandboxName(claim)
			if err != nil {
				t.Fatal(err)
			}
			current, err := sandbox.ClaimSandboxName(after.(*unstructured.Unstructured))
			if err != nil || current != original {
				t.Fatal("component upgrade redirected the native Claim", err)
			}
		}
	}
	for i, instance := range f.instances {
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(instance), instance); err != nil {
			t.Fatal(err)
		}
		if instance.Status.Definition == nil || instance.Status.Definition.Revision != f.observations[i].instance.Status.Definition.Revision {
			t.Fatal("component upgrade changed the accepted application revision")
		}
		before := f.observations[i].instance
		if instance.Status.Provisioning == nil || before.Status.Provisioning == nil ||
			instance.Status.Provisioning.ClaimName != before.Status.Provisioning.ClaimName ||
			instance.Status.SandboxName != before.Status.SandboxName || instance.Status.SandboxUID != before.Status.SandboxUID ||
			!equality.Semantic.DeepEqual(instance.Status.Workspace, before.Status.Workspace) {
			t.Fatalf("component upgrade changed %s's binding: before provisioning=%+v sandbox=%s/%s workspace=%+v; after provisioning=%+v sandbox=%s/%s workspace=%+v",
				instance.Name, before.Status.Provisioning, before.Status.SandboxName, before.Status.SandboxUID, before.Status.Workspace,
				instance.Status.Provisioning, instance.Status.SandboxName, instance.Status.SandboxUID, instance.Status.Workspace)
		}
		// v0.0.6 did not expose claimUID. If present, the status pin must match
		// the original native Claim, whose live UID is checked above in every version.
		if uid := instance.Status.Provisioning.ClaimUID; uid != "" && uid != f.observations[i].claim.GetUID() {
			t.Fatal("status points at a different Claim UID")
		}
		// Compare actual mounts rather than requiring an identical status summary.
		workload := sandbox.Object()
		if err := kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.SandboxName}, workload); err != nil {
			t.Fatal(err)
		}
		current, err := sandbox.Decode(workload)
		if err != nil {
			t.Fatal(err)
		}
		pod := sandbox.ExecutionPodTemplate(current.Metadata.Name, &current.Spec.PodTemplate, current.Spec.VolumeClaimTemplates)
		for _, volume := range f.observations[i].pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				continue
			}
			found := false
			for _, actual := range pod.Spec.Volumes {
				if actual.Name == volume.Name && equality.Semantic.DeepEqual(actual.PersistentVolumeClaim, volume.PersistentVolumeClaim) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("component upgrade changed PVC mount %s/%s", instance.Name, volume.Name)
			}
		}
	}
}

func (f *componentUpgradeFixture) verify(t *testing.T, kube client.Client, kubeconfig string) {
	t.Helper()
	f.verifyIdentities(t, kube)
	running, suspended := f.instances[0], f.instances[1]
	if running.Status.PodUID != string(f.observations[0].pod.UID) || !conditionTrue(running.Status.Conditions, "Ready", running.Generation) {
		t.Fatal("component upgrade disrupted the running instance")
	}
	if suspended.Spec.OperatingMode != sandbox.Suspended || !conditionTrue(suspended.Status.Conditions, "Suspended", suspended.Generation) {
		t.Fatal("component upgrade changed the suspended instance")
	}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(&f.observations[1].pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("suspended instance has a Pod", err)
	}
	var pods corev1.PodList
	if err := kube.List(t.Context(), &pods, client.InNamespace(running.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].UID != f.observations[0].pod.UID || pods.Items[0].DeletionTimestamp != nil {
		t.Fatal("component upgrade replaced the live running Pod or started a suspended execution")
	}
	if got := agentHTTP(t, kubeconfig, running, "GET", "/", ""); got != "upgrade" {
		t.Fatalf("running application changed: %q", got)
	}
	if got := execPod(t, kubeconfig, running.Namespace, running.Status.PodName, "cat", "/workspace/session"); got != "retained-through-upgrade" {
		t.Fatalf("running workspace changed: %q", got)
	}
}

func (f *componentUpgradeFixture) verifyResume(t *testing.T, kube client.Client, kubeconfig string) {
	t.Helper()
	for i, instance := range f.instances {
		if instance.Spec.OperatingMode != sandbox.Suspended {
			suspendAgent(t, kube, instance)
		}
		instance.Spec.OperatingMode = sandbox.Running
		update(t, t.Context(), kube, instance)
		readyAgent(t, t.Context(), kube, instance)
		if instance.Status.PodUID == string(f.observations[i].pod.UID) {
			t.Fatal("resume did not create a new process")
		}
		if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "sh", "-ec", "cat /workspace/session; printf -- -writable >> /workspace/session; sync; cat /workspace/session"); got != "retained-through-upgraderetained-through-upgrade-writable" {
			t.Fatalf("resumed workspace did not retain readable, writable data: %q", got)
		}
		if got := agentHTTP(t, kubeconfig, instance, "GET", "/", ""); got != "upgrade" {
			t.Fatalf("resumed application changed: %q", got)
		}
	}
	f.verifyIdentities(t, kube)
}

func TestSandboxUpgrade(t *testing.T) {
	path := os.Getenv("E2E_SANDBOX_UPGRADE_MANIFEST")
	if path == "" {
		t.Skip("requires E2E_PROFILE=sandbox-upgrade")
	}
	kube, kubeconfig := e2eClient(t)
	f := prepareComponentUpgrade(t, kube, kubeconfig)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var crds, controllers []unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if object.Object == nil {
			continue
		}
		if object.GetKind() == "CustomResourceDefinition" {
			crds = append(crds, object)
		} else {
			controllers = append(controllers, object)
		}
	}
	if len(crds) == 0 || len(controllers) == 0 {
		t.Fatal("upgrade manifest lacks CRDs or controllers")
	}
	for _, stage := range []struct {
		name    string
		objects []unstructured.Unstructured
	}{{"crds", crds}, {"controllers", controllers}} {
		file := filepath.Join(t.TempDir(), stage.name+".json")
		data, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": stage.objects})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log("Apply upstream upgrade stage:", stage.name)
		kubectl(t, kubeconfig, "apply", "--server-side", "-f", file)
		if stage.name == "crds" {
			for _, crd := range crds {
				kubectl(t, kubeconfig, "wait", "--for=condition=Established", "crd/"+crd.GetName(), "--timeout=60s")
			}
		} else {
			kubectl(t, kubeconfig, "-n", "agent-sandbox-system", "rollout", "status", "deployment/agent-sandbox-controller", "--timeout=240s")
		}
		f.verify(t, kube, kubeconfig)
	}
	f.verifyResume(t, kube, kubeconfig)
}
