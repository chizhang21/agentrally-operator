//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestSharedStorage(t *testing.T) {
	if os.Getenv("E2E_PROFILE") != "shared-storage" {
		t.Skip("requires the shared-storage Kind profile")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	group := &v1.AgentGroup{}
	readExample(t, "02-group.yaml", group)
	create(t, ctx, kube, group)
	await(t, "example Group", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	native := sandbox.ExtensionObject("SandboxTemplate")
	readExample(t, "sandbox-template.yaml", native)
	create(t, ctx, kube, native)
	definition := &v1.AgentDefinition{}
	readExample(t, "03-definition.yaml", definition)
	definition.Spec.Template, definition.Spec.Container = "", nil
	definition.Spec.TemplateRef = &v1.SandboxTemplateReference{Name: native.GetName()}
	definition.Spec.Security.RunAsUser = ptr.To(int64(1000))
	create(t, ctx, kube, definition)

	a, b := &v1.AgentInstance{}, &v1.AgentInstance{}
	readExample(t, "04-instance-full.yaml", a)
	readExample(t, "04-instance-basic.yaml", b)
	a.Name, b.Name = "private-a", "private-b"
	a.Spec.Workspace.VolumeClaimTemplate.StorageClassName = ptr.To("agentrally-test-nfs")
	a.Spec.Workspace.VolumeClaimTemplate.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("32Mi")
	b.Spec.Workspace = a.Spec.Workspace.DeepCopy()
	b.Spec.Workspace.VolumeClaimTemplate = nil
	b.Spec.Workspace.ExistingClaimRef = &v1.PersistentVolumeClaimReference{Name: "retained-private-b"}
	retained := nfsClaim(t, ctx, kube, group.Status.Namespace, "retained-private-b")

	t.Log("Missing shared PVC is diagnosed; completing the dependency resumes the original request")
	create(t, ctx, kube, a)
	await(t, "missing shared PVC diagnosis", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(a), a); err != nil {
			return false, err
		}
		if meta.IsStatusConditionTrue(a.Status.Conditions, "Ready") {
			return false, fmt.Errorf("instance became ready without its required shared PVC")
		}
		for _, condition := range a.Status.Conditions {
			if condition.ObservedGeneration == a.Generation && condition.Reason == "PVCNotFound" && strings.Contains(condition.Message, "shared-content") {
				return true, nil
			}
		}
		return false, nil
	})
	shared := nfsClaim(t, ctx, kube, group.Status.Namespace, "shared-content")
	publisher := storagePublisher(t, ctx, kube, kubeconfig, native, retained.Name)
	create(t, ctx, kube, b)
	for _, instance := range []*v1.AgentInstance{a, b} {
		readyAgent(t, ctx, kube, instance)
		execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "python", "-c", "from pathlib import Path; import sys; Path('/workspace/session').write_text(sys.argv[1])", instance.Name)
	}
	identities := map[string]storageIdentity{a.Name: storageIdentityOf(t, ctx, kube, a), b.Name: storageIdentityOf(t, ctx, kube, b)}
	check := func(instance *v1.AgentInstance, version string) {
		t.Helper()
		if actual := storageIdentityOf(t, ctx, kube, instance); actual != identities[instance.Name] {
			t.Fatalf("%s storage identity changed: %#v", instance.Name, actual)
		}
		pod := &corev1.Pod{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.PodName}, pod); err != nil {
			t.Fatal(err)
		}
		if string(pod.UID) != instance.Status.PodUID {
			t.Fatal("Instance status refers to a different Pod", instance.Name)
		}
		own := identities[instance.Name].pvcName
		claims := map[string]bool{}
		for _, volume := range pod.Spec.Volumes {
			if ref := volume.PersistentVolumeClaim; ref != nil {
				claims[ref.ClaimName] = true
				if ref.ClaimName != own && ref.ClaimName != shared.Name {
					t.Fatal("an unrelated private PVC was injected", ref.ClaimName)
				}
			}
		}
		if len(claims) != 2 || !claims[own] || !claims[shared.Name] {
			t.Fatal("private and shared PVC mounts were not both present", claims)
		}
		// Check actual errno after each process replacement, not just readOnly fields.
		got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "python", "-c", `import errno, os, pathlib, sys
private = pathlib.Path('/workspace/session')
assert private.read_text() == sys.argv[1]
private.write_text(sys.argv[1])
public = pathlib.Path(os.environ['SHARED_CONTENT_PATH']) / 'skills.txt'
assert public.read_text() == sys.argv[2]
for action, expected in [(lambda: public.open('a'), errno.EROFS), (lambda: pathlib.Path('/shared/publisher-only').read_text(), errno.EACCES)]:
    try:
        action()
    except OSError as error:
        assert error.errno == expected, (error, expected)
    else:
        raise AssertionError('storage access was unexpectedly permitted')
print('verified')`, instance.Name, version)
		if got != "verified" {
			t.Fatal("storage probe did not complete", got)
		}
	}
	t.Log("Each instance writes its own files; both read v1 and reject shared writes and unauthorized reads")
	for _, instance := range []*v1.AgentInstance{a, b} {
		check(instance, "v1")
		suspendAgent(t, kube, instance)
		instance.Spec.OperatingMode = sandbox.Running
		update(t, ctx, kube, instance)
		readyAgent(t, ctx, kube, instance)
		check(instance, "v1")
	}

	t.Log("Only A selects v2; B keeps its original process and shared-content version")
	oldRevision := b.Status.Definition.Revision
	bPod := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Status.PodName}, bPod); err != nil {
		t.Fatal(err)
	}
	bPodUID := bPod.UID
	base := native.DeepCopy()
	containers, _, err := unstructured.NestedSlice(native.Object, "spec", "podTemplate", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, item := range containers {
		container := item.(map[string]interface{})
		if container["name"] != "agent" {
			continue
		}
		env, _, err := unstructured.NestedSlice(container, "env")
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range env {
			variable := item.(map[string]interface{})
			if variable["name"] == "SHARED_CONTENT_PATH" {
				variable["value"], changed = "/shared/v2", true
			}
		}
		container["env"] = env
	}
	if !changed {
		t.Fatal("example has no shared-content version selector")
	}
	if err := unstructured.SetNestedSlice(native.Object, containers, "spec", "podTemplate", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	if err := kube.Patch(ctx, native, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	await(t, "shared-content v2 revision", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != oldRevision && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	a.Spec.Revision = definition.Status.CurrentRevision
	update(t, ctx, kube, a)
	readyAgent(t, ctx, kube, a)
	if a.Status.Definition.Revision != definition.Status.CurrentRevision {
		t.Fatal("A did not accept the requested content revision")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Status.PodName}, bPod); err != nil {
		t.Fatal(err)
	}
	if bPod.UID != bPodUID || b.Status.Definition.Revision != oldRevision {
		t.Fatal("upgrading A changed B")
	}
	check(a, "v2")
	check(b, "v1")

	t.Log("Instance deletion preserves external private/shared PVCs and clears the template-owned PVC")
	if err := kube.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, b)
	checkExternalStorage := func() {
		t.Helper()
		for _, claim := range []*corev1.PersistentVolumeClaim{retained, shared} {
			uid := claim.UID
			if err := kube.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			if claim.UID != uid || claim.DeletionTimestamp != nil || len(claim.OwnerReferences) != 0 {
				t.Fatal("Operator adopted, deleted or replaced external storage", claim.Name)
			}
		}
		if got := execPod(t, kubeconfig, publisher.Namespace, publisher.Name, "python", "-c", "from pathlib import Path; print(Path('/retained/session').read_text()); print(Path('/shared/v1/skills.txt').read_text())"); got != "private-b\nv1" {
			t.Fatal("external storage files were not retained", got)
		}
	}
	checkExternalStorage()
	check(a, "v2")
	owned := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: identities[a.Name].pvcName}}
	if err := kube.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, a)
	awaitDeleted(t, ctx, kube, owned)
	checkExternalStorage()

	verifyNFSMountFailure(t, ctx, kube, shared)
	if err := kube.Delete(ctx, publisher); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, publisher)
	for _, claim := range []*corev1.PersistentVolumeClaim{retained, shared} {
		if err := kube.Delete(ctx, claim); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, claim)
	}
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

type storageIdentity struct {
	instance, claim, sandbox, pvc, shared types.UID
	pvcName                               string
}

func storageIdentityOf(t *testing.T, ctx context.Context, kube client.Client, instance *v1.AgentInstance) storageIdentity {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.VolumeClaims["agentrally-workspace"]}, pvc); err != nil {
		t.Fatal(err)
	}
	shared := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.VolumeClaims["shared-content"]}, shared); err != nil {
		t.Fatal(err)
	}
	execution := sandbox.Object()
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.SandboxName}, execution); err != nil {
		t.Fatal(err)
	}
	claim := sandbox.ExtensionObject("SandboxClaim")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.Provisioning.ClaimName}, claim); err != nil {
		t.Fatal(err)
	}
	return storageIdentity{instance.UID, claim.GetUID(), execution.GetUID(), pvc.UID, shared.UID, pvc.Name}
}

func readExample(t *testing.T, name string, object client.Object) {
	t.Helper()
	data, err := os.ReadFile("../../examples/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(data, object); err != nil {
		t.Fatal(err)
	}
}

func nfsClaim(t *testing.T, ctx context.Context, kube client.Client, namespace, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: corev1.PersistentVolumeClaimSpec{
		StorageClassName: ptr.To("agentrally-test-nfs"), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}},
	}}
	create(t, ctx, kube, claim)
	await(t, "NFS PVC binding", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(claim), claim)
		return claim.Status.Phase == corev1.ClaimBound, err
	})
	return claim
}

func storagePublisher(t *testing.T, ctx context.Context, kube client.Client, kubeconfig string, native *unstructured.Unstructured, retainedName string) *corev1.Pod {
	t.Helper()
	template, _, err := sandbox.TemplatePod(native)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "content-publisher", Namespace: native.GetNamespace()}, Spec: *template.Spec.DeepCopy()}
	pod.Spec.SecurityContext.RunAsUser = ptr.To(int64(2000))
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "shared-content" {
			pod.Spec.Volumes[i].PersistentVolumeClaim.ReadOnly = false
		}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "retained", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: retainedName, ReadOnly: true}}})
	container := &pod.Spec.Containers[0]
	container.Command, container.Args, container.ReadinessProbe = []string{"sleep", "3600"}, nil, nil
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == "shared-content" {
			container.VolumeMounts[i].ReadOnly = false
		}
	}
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "retained", MountPath: "/retained", ReadOnly: true})
	create(t, ctx, kube, pod)
	kubectl(t, kubeconfig, "-n", pod.Namespace, "wait", "pod/"+pod.Name, "--for=condition=Ready", "--timeout=120s")
	execPod(t, kubeconfig, pod.Namespace, pod.Name, "python", "-c", `from pathlib import Path
for version in ('v1', 'v2'):
    directory = Path('/shared') / version
    directory.mkdir()
    public = directory / 'skills.txt'
    public.write_text(version)
    public.chmod(0o666)
restricted = Path('/shared/publisher-only')
restricted.write_text('publisher data')
restricted.chmod(0o600)`)
	return pod
}

func verifyNFSMountFailure(t *testing.T, ctx context.Context, kube client.Client, shared *corev1.PersistentVolumeClaim) {
	t.Helper()
	t.Log("An unexported NFS path produces a real FailedMount and an Instance diagnosis")
	backing := &corev1.PersistentVolume{}
	if err := kube.Get(ctx, client.ObjectKey{Name: shared.Spec.VolumeName}, backing); err != nil {
		t.Fatal(err)
	}
	if backing.Spec.NFS == nil {
		t.Fatal("fixture did not provide an NFS-backed volume")
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "unexported-nfs"}, Spec: corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, MountOptions: []string{"vers=4.1", "retrans=2", "timeo=30"},
		PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: backing.Spec.NFS.Server, Path: "/not-exported", ReadOnly: true}},
	}}
	create(t, ctx, kube, pv)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "unexported-nfs", Namespace: shared.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{
		StorageClassName: ptr.To(""), VolumeName: pv.Name, AccessModes: pv.Spec.AccessModes,
		Resources: corev1.VolumeResourceRequirements{Requests: pv.Spec.Capacity},
	}}
	create(t, ctx, kube, pvc)
	native := sandbox.ExtensionObject("SandboxTemplate")
	readExample(t, "sandbox-template.yaml", native)
	native.SetName("unexported-nfs")
	if err := unstructured.SetNestedSlice(native.Object, []interface{}{map[string]interface{}{"name": "shared-content", "persistentVolumeClaim": map[string]interface{}{"claimName": pvc.Name, "readOnly": true}}}, "spec", "podTemplate", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	create(t, ctx, kube, native)
	definition := &v1.AgentDefinition{}
	readExample(t, "03-definition.yaml", definition)
	definition.Name, definition.Spec.Template, definition.Spec.Container = "mount-failure", "", nil
	definition.Spec.TemplateRef = &v1.SandboxTemplateReference{Name: native.GetName()}
	create(t, ctx, kube, definition)
	instance := &v1.AgentInstance{}
	readExample(t, "04-instance-basic.yaml", instance)
	instance.Name, instance.Spec.DefinitionRef.Name = "mount-failure", definition.Name
	create(t, ctx, kube, instance)
	await(t, "real mount failure diagnosis", func() (bool, error) {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
			return false, err
		}
		condition := meta.FindStatusCondition(instance.Status.Conditions, "Available")
		if condition == nil || condition.ObservedGeneration != instance.Generation || condition.Reason != "VolumeMountFailed" || condition.Status != metav1.ConditionFalse || meta.IsStatusConditionTrue(instance.Status.Conditions, "Ready") {
			return false, nil
		}
		events := &corev1.EventList{}
		if err := kube.List(ctx, events, client.InNamespace(instance.Namespace)); err != nil {
			return false, err
		}
		for _, event := range events.Items {
			if string(event.InvolvedObject.UID) == instance.Status.PodUID && event.Reason == "FailedMount" && strings.Contains(event.Message, pv.Name) {
				return true, nil
			}
		}
		return false, nil
	})
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, instance)
	if err := kube.Delete(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, pvc)
	if err := kube.Delete(ctx, pv); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, pv)
}
