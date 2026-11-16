//go:build e2e

package e2e_test

import (
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDefinitionRevisionsAndDeletion(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "definition-revisions")
	definition, first := testAgent(group, "coding", "exec sleep 3600")
	definition.Spec.Template = "coding"
	definition.Spec.Container.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi"),
	}}
	definition.Spec.Container.Env = []corev1.EnvVar{{Name: "RELEASE", Value: "v1"}}
	create(t, ctx, kube, definition)
	create(t, ctx, kube, first)
	readyAgent(t, ctx, kube, first)
	oldRevision, oldPod := first.Status.Definition.Revision, first.Status.PodUID
	definition.Spec.Container.Env[0].Value = "v2"
	update(t, ctx, kube, definition)
	await(t, "second immutable publication", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != oldRevision && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	newRevision := definition.Status.CurrentRevision
	_, latest := testAgent(group, "latest", "")
	latest.Spec.DefinitionRef = first.Spec.DefinitionRef
	historical := latest.DeepCopy()
	historical.Name, historical.Spec.Revision = "historical", oldRevision
	for _, instance := range []*v1.AgentInstance{latest, historical} {
		create(t, ctx, kube, instance)
		readyAgent(t, ctx, kube, instance)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(first), first); err != nil {
		t.Fatal(err)
	}
	if first.Status.PodUID != oldPod || first.Status.Definition.Revision != oldRevision || latest.Status.Definition.Revision != newRevision || historical.Status.Definition.Revision != oldRevision {
		t.Fatal("publication changed a pinned instance or ignored revision selection")
	}
	for _, instance := range []*v1.AgentInstance{first, historical, latest} {
		want := "v1"
		if instance == latest {
			want = "v2"
		}
		if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "printenv", "RELEASE"); got != want {
			t.Fatalf("%s executed %s, want %s", instance.Name, got, want)
		}
	}

	t.Log("Ordinary restart preserves the accepted revision after a new publication")
	first.Spec.RestartToken = "same-revision"
	update(t, ctx, kube, first)
	await(t, "restart on original revision", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(first), first)
		return first.Status.PodUID != oldPod && conditionTrue(first.Status.Conditions, "Ready", first.Generation), err
	})
	if first.Status.Definition.Revision != oldRevision || execPod(t, kubeconfig, first.Namespace, first.Status.PodName, "printenv", "RELEASE") != "v1" {
		t.Fatal("ordinary restart implicitly upgraded the accepted definition")
	}
	t.Log("Selecting a revision upgrades the process without a new restart token")
	for _, step := range []struct{ operation, revision, release string }{
		{"upgrade", newRevision, "v2"}, {"rollback", oldRevision, "v1"},
	} {
		previousPod := first.Status.PodUID
		first.Spec.Revision = step.revision
		update(t, ctx, kube, first)
		await(t, step.operation+" execution", func() (bool, error) {
			err := kube.Get(ctx, client.ObjectKeyFromObject(first), first)
			return first.Status.Definition != nil && first.Status.Definition.Revision == step.revision && first.Status.PodUID != previousPod &&
				conditionTrue(first.Status.Conditions, "Ready", first.Generation), err
		})
		if got := execPod(t, kubeconfig, first.Namespace, first.Status.PodName, "printenv", "RELEASE"); got != step.release {
			t.Fatalf("%s executed %s, want %s", step.operation, got, step.release)
		}
	}

	t.Log("A deleting definition retains revisions used by live instances")
	if err := kube.Delete(ctx, definition); err != nil {
		t.Fatal(err)
	}
	await(t, "definition deletion protection", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		ready := meta.FindStatusCondition(definition.Status.Conditions, "Ready")
		return definition.DeletionTimestamp != nil && ready != nil && ready.Reason == "DefinitionInUse", err
	})
	for _, instance := range []*v1.AgentInstance{first, latest, historical} {
		if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "printenv", "RELEASE"); got == "" {
			t.Fatal("definition deletion stopped a referencing execution")
		}
		if err := kube.Delete(ctx, instance); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, instance)
	}
	awaitDeleted(t, ctx, kube, definition)
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
	awaitDeleted(t, ctx, kube, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: group.Status.Namespace}})
}
