package agentinstance

import (
	"context"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func indexedClient(t *testing.T, objects ...client.Object) (client.WithWatch, *runtime.Scheme) {
	t.Helper()
	builder, scheme := testutil.ClientBuilder(t)
	builder.WithIndex(&v1.AgentInstance{}, dependencyIndex, instanceDependencyKeys)
	builder.WithIndex(sandbox.ExtensionObject("SandboxTemplate"), dependencyIndex, templateDependencyKeys)
	return builder.WithObjects(objects...).Build(), scheme
}

func TestDependencyEventsUsePinnedTemplatesAndIndexes(t *testing.T) {
	_, _, definition, instance, source := testutil.Fixture()
	source.Spec.Containers[0].Env = []corev1.EnvVar{configReference("settings", "mode", false)}
	source.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
	c, scheme := indexedClient(t, definition, instance, nativeTemplate(definition, source))
	resolved, err := resolveSandboxTemplate(ctx, c, instance)
	if err != nil {
		t.Fatal(err)
	}
	instance.Status.Definition = resolved.Definition
	if err := c.Status().Update(ctx, instance); err != nil {
		t.Fatal(err)
	}
	guard := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("event mapping read an object")
			return nil
		},
		List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, opts ...client.ListOption) error {
			options := &client.ListOptions{}
			options.ApplyOptions(opts)
			if options.FieldSelector == nil || options.FieldSelector.Empty() || options.Namespace == "" {
				t.Fatal("unindexed or cross-namespace scan")
			}
			return c.List(ctx, objects, opts...)
		},
	})
	r := &Reconciler{Client: guard, Scheme: scheme}
	for _, event := range []client.Object{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: instance.Namespace}}, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: instance.Namespace}}} {
		requests := r.mapDependency(ctx, event)
		if len(requests) != 1 || requests[0].Name != instance.Name {
			t.Fatalf("pinned dependency not mapped: %+v", requests)
		}
		event.SetNamespace("other")
		if len(r.mapDependency(ctx, event)) != 0 {
			t.Fatal("event crossed namespace boundary")
		}
	}
	// Editing the reusable source does not change existing instance dependencies.
	source.Spec.Containers[0].Env = nil
	source.Spec.Volumes = nil
	updateTemplate(t, c, instance, source)
	testutil.PublishDefinitions(t, c, scheme)
	if len(r.mapDependency(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: instance.Namespace}})) != 1 {
		t.Fatal("source edit changed a pinned dependency")
	}
	if len(r.mapDependency(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: instance.Namespace}})) != 0 {
		t.Fatal("Secret events must not be processed")
	}
}
