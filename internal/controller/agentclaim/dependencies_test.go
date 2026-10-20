package agentclaim

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
	builder.WithIndex(&v1.AgentClaim{}, dependencyIndex, claimDependencyKeys)
	builder.WithIndex(sandbox.ExtensionObject("SandboxTemplate"), dependencyIndex, templateDependencyKeys)
	return builder.WithObjects(objects...).Build(), scheme
}

func TestDependencyEventsUseIndexesWithoutReadingTemplates(t *testing.T) {
	_, _, native, source := testutil.Fixture()
	source.Spec.InitContainers = []corev1.Container{{Name: "init", EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}}}}}
	source.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
	builtin := native.DeepCopy()
	builtin.Name, builtin.Spec.TemplateRef, builtin.Spec.Template = "builtin", nil, "generic"
	builtin.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1", Env: []corev1.EnvVar{configReference("settings", "mode", false)}}
	other := builtin.DeepCopy()
	other.Name, other.Namespace = "other-namespace", "arally-other"
	unrelated := builtin.DeepCopy()
	unrelated.Name, unrelated.Spec.Runtime.Env = "unrelated", nil
	c, scheme := indexedClient(t, nativeTemplate(native, source), native, builtin, other, unrelated)
	lists := 0
	guard := interceptor.NewClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("event handler read an object instead of using cache indexes")
			return nil
		},
		List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, opts ...client.ListOption) error {
			options := &client.ListOptions{}
			options.ApplyOptions(opts)
			if options.FieldSelector == nil || options.FieldSelector.Empty() || options.Namespace != native.Namespace {
				t.Fatal("unindexed or cross-namespace dependency scan")
			}
			lists++
			return c.List(ctx, objects, opts...)
		},
	})
	r := &Reconciler{Client: guard, Reader: guard, Scheme: scheme}
	requests := r.mapDependency(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: native.Namespace}})
	if len(requests) != 2 || lists != 3 {
		t.Fatalf("expected two dependents using three indexed lookups, got %+v (%d lookups)", requests, lists)
	}
	requests = r.mapDependency(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: native.Namespace}})
	if len(requests) != 1 || requests[0].Name != native.Name {
		t.Fatalf("native PVC reference not mapped: %+v", requests)
	}
	if len(r.mapDependency(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: native.Namespace}})) != 0 {
		t.Fatal("Secret events must not be processed")
	}
}

func TestDependencyIndexTracksClaimAndTemplateUpdates(t *testing.T) {
	_, _, w, source := testutil.Fixture()
	source.Spec.Containers[0].Env = []corev1.EnvVar{configReference("old", "mode", false)}
	c, scheme := indexedClient(t, w, nativeTemplate(w, source))
	r := &Reconciler{Client: c, Scheme: scheme}
	event := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: w.Namespace}}
	if len(r.mapDependency(ctx, event)) != 1 {
		t.Fatal("initial template reference not indexed")
	}
	source.Spec.Containers[0].Env[0] = configReference("new", "mode", false)
	updateTemplate(t, c, w, source)
	if len(r.mapDependency(ctx, event)) != 0 {
		t.Fatal("old template dependency remained indexed")
	}
	event.Name = "new"
	if len(r.mapDependency(ctx, event)) != 1 {
		t.Fatal("new template dependency not indexed")
	}
	w.Spec.TemplateRef.Name = "different"
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	if len(r.mapDependency(ctx, event)) != 0 {
		t.Fatal("old Claim template reference remained indexed")
	}
}
