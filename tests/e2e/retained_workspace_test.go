//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRetainedWorkspaceUpgrade(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	group := readyGroup(t, ctx, kube, "retained-workspace")
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2}
	update(t, ctx, kube, group)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "assistant-state", Namespace: group.Status.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}}}}
	if os.Getenv("E2E_SNAPSHOTS") == "true" {
		sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "retained-hostpath"}, Provisioner: "hostpath.csi.k8s.io"}
		create(t, ctx, kube, sc)
		pvc.Spec.StorageClassName = ptr.To(sc.Name)
		t.Cleanup(func() {
			if err := kube.Delete(ctx, sc); err != nil {
				t.Error(err)
			}
		})
	}
	create(t, ctx, kube, pvc)
	pvcUID := pvc.UID
	definition, instance := testAgent(group, "assistant", agentCommand("v1"))
	definition.Spec.Template = "assistant"
	definition.Spec.Security.RunAsUser = ptr.To(int64(2345))
	definition.Spec.Container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}
	instance.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", ExistingClaimRef: &v1.PersistentVolumeClaimReference{Name: pvc.Name}, Exclusive: true}
	create(t, ctx, kube, definition)
	create(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	competitor := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "competing-assistant", Namespace: instance.Namespace}, Spec: *instance.Spec.DeepCopy()}
	create(t, ctx, kube, competitor)
	await(t, "exclusive workspace rejection", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(competitor), competitor)
		condition := meta.FindStatusCondition(competitor.Status.Conditions, "Ready")
		return condition != nil && condition.ObservedGeneration == competitor.Generation && condition.Reason == "WorkspaceInUse" && competitor.Status.Provisioning == nil, err
	})
	if err := kube.Delete(ctx, competitor); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, competitor)
	pod := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.PodName}, pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil || *pod.Spec.SecurityContext.FSGroup != 2345 {
		t.Fatal("explicit identity did not reach the workspace volume group")
	}
	if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "id", "-u"); got != "2345" {
		t.Fatal("execution did not use the selected process UID", got)
	}
	previousRevision, previousPod := instance.Status.Definition.Revision, instance.Status.PodUID
	execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "sh", "-ec", "printf retained-user-state > /workspace/session; sync")
	definition.Spec.Container.Args = []string{agentCommand("v2")}
	update(t, ctx, kube, definition)
	await(t, "new retained assistant configuration", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != previousRevision && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	instance.Spec.Revision, instance.Spec.RestartToken = definition.Status.CurrentRevision, "upgrade-v2"
	update(t, ctx, kube, instance)
	await(t, "retained assistant upgrade", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return instance.Status.Definition != nil && instance.Status.Definition.Revision == definition.Status.CurrentRevision && instance.Status.PodUID != previousPod && conditionTrue(instance.Status.Conditions, "Ready", instance.Generation), err
	})
	if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "cat", "/workspace/session"); got != "retained-user-state" {
		t.Fatal("upgrade lost state", got)
	}
	if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "cat", "/tmp/www/index.html"); got != "v2" {
		t.Fatal("upgrade kept old application", got)
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.Provisioning.PoolName}, pool); err != nil {
		t.Fatal(err)
	}
	if count, _, err := unstructured.NestedInt64(pool.Object, "spec", "replicas"); err != nil || count != 0 {
		t.Fatal("retained workspace received spares", count, err)
	}
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, instance)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil || pvc.UID != pvcUID || len(pvc.OwnerReferences) != 0 {
		t.Fatal("execution deletion changed retained storage", err)
	}
	if err := kube.Delete(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, pvc)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}
