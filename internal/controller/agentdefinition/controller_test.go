package agentdefinition

import (
	"context"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
	"arally/operator/internal/definition"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDefinitionPublishesImmutableRevisionsWithoutExecution(t *testing.T) {
	ctx := context.Background()
	group, ns, object, instance, _ := testutil.Fixture()
	object.Spec.TemplateRef = nil
	object.Spec.Template = "assistant"
	object.Spec.Container = &v1.AgentContainerSpec{Image: "example/assistant:v1"}
	builder, scheme := testutil.ClientBuilder(t)
	// Run the real controller rather than the instance fixture publisher.
	c := builder.ClientBuilder.WithObjects(group, ns, object).Build()
	r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	first := object.Status.CurrentRevision
	if first == "" || !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") {
		t.Fatal("configuration was not published", object.Status)
	}
	for _, kind := range []string{"SandboxWarmPool", "SandboxClaim"} {
		list := sandbox.ExtensionList(kind)
		if err := c.List(ctx, list); err != nil || len(list.Items) != 0 {
			t.Fatal("definition started allocation", kind, err)
		}
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods); err != nil || len(pods.Items) != 0 {
		t.Fatal("definition started a Pod", err)
	}
	object.Spec.Container.Image = "example/assistant:v2"
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if object.Status.CurrentRevision == first {
		t.Fatal("image change did not publish a new revision")
	}
	old, err := definition.Read(ctx, c, object, first)
	if err != nil || old.Spec.Container.Image != "example/assistant:v1" {
		t.Fatal("old revision was mutated", err)
	}
	// Referencing instances protect all revisions from cascading deletion.
	if err := c.Create(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if ready := meta.FindStatusCondition(object.Status.Conditions, "Ready"); ready == nil || ready.Reason != "DefinitionInUse" {
		t.Fatal("deletion was not guarded", object.Status)
	}
	// A stored revision must reject changes even under the same name and UID.
	old.Template.SetAnnotations(map[string]string{definition.SpecAnnotation: `{}`, definition.RevisionAnnotation: first})
	if err := c.Update(ctx, old.Template); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Read(ctx, c, object, first); err == nil {
		t.Fatal("tampered configuration was accepted")
	} else if reason, _ := diagnostics.Classify(err); reason != "DefinitionRevisionConflict" {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionCannotPublishOutsideReadyGroup(t *testing.T) {
	ctx := context.Background()
	group, ns, object, _, _ := testutil.Fixture()
	group.Status.Conditions = []metav1.Condition{}
	builder, scheme := testutil.ClientBuilder(t)
	c := builder.ClientBuilder.WithObjects(group, ns, object).Build()
	r := &Reconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	if ready := meta.FindStatusCondition(object.Status.Conditions, "Ready"); ready == nil || ready.Reason != "GroupNotReady" || object.Status.CurrentRevision != "" {
		t.Fatal("unready Group published configuration", object.Status)
	}
}

func TestDefinitionDeletionDrainsOnlyItsCapacityAfterNativeClaims(t *testing.T) {
	ctx := context.Background()
	group, ns, object, _, _ := testutil.Fixture()
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2, IdleTTLSeconds: 600}
	object.Finalizers = []string{Finalizer}
	template := sandbox.ExtensionObject("SandboxTemplate")
	template.SetName("allocation-template")
	template.SetNamespace(ns.Name)
	template.SetLabels(map[string]string{allocation.ManagedLabel: "true", definition.UIDLabel: string(object.UID)})
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName("allocation-pool")
	pool.SetNamespace(ns.Name)
	pool.SetLabels(template.GetLabels())
	pool.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": template.GetName()}, "replicas": int64(2)}
	nativeClaim := sandbox.ExtensionObject("SandboxClaim")
	nativeClaim.SetName("external-native-caller")
	nativeClaim.SetNamespace(ns.Name)
	nativeClaim.Object["spec"] = map[string]interface{}{"warmPoolRef": map[string]interface{}{"name": pool.GetName()}}
	builder, scheme := testutil.ClientBuilder(t)
	for _, resource := range []client.Object{template, pool} {
		if err := ctrl.SetControllerReference(group, resource, scheme); err != nil {
			t.Fatal(err)
		}
	}
	other := pool.DeepCopy()
	other.SetName("another-definition-pool")
	other.SetLabels(map[string]string{allocation.ManagedLabel: "true", definition.UIDLabel: "another-definition"})
	c := builder.ClientBuilder.WithObjects(group, ns, object, template, pool, nativeClaim, other).Build()
	r := &Reconciler{Client: c, Scheme: scheme}
	if err := c.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal("deleted capacity still used by a native Claim", err)
	}
	if err := c.Delete(ctx, nativeClaim); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		reconcile()
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), pool); !apierrors.IsNotFound(err) {
		t.Fatal("idle retention blocked definition cleanup", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(other), other); err != nil {
		t.Fatal("deleted another definition's capacity", err)
	}
}
