package agentdefinition

import (
	"context"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDefinitionDependencyMappingUsesScopedIndex(t *testing.T) {
	definition := func(namespace, name, group, source string) *v1.AgentDefinition {
		return &v1.AgentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: v1.AgentDefinitionSpec{
			AgentGroupRef: v1.AgentGroupReference{Name: group}, TemplateRef: &v1.SandboxTemplateReference{Name: source},
		}}
	}
	one := definition("first", "one", "team", "source")
	two := definition("second", "two", "team", "source")
	other := definition("first", "other", "other", "elsewhere")
	builder, _ := testutil.ClientBuilder(t)
	c := builder.WithObjects(one, two, other).WithIndex(&v1.AgentDefinition{}, definitionDependencyIndex, definitionDependencyKeys).Build()
	r := &Reconciler{Client: interceptor.NewClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("event mapping performed a dependency read")
			return nil
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if selector := (&client.ListOptions{}).ApplyOptions(options).FieldSelector; selector == nil || selector.Empty() {
				t.Fatal("event mapping scanned unindexed Definitions")
			}
			return c.List(ctx, list, options...)
		},
	})}
	source := sandbox.ExtensionObject("SandboxTemplate")
	source.SetName("source")
	source.SetNamespace("first")
	for _, tc := range []struct {
		event client.Object
		want  map[client.ObjectKey]bool
	}{
		{&v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "team"}}, map[client.ObjectKey]bool{client.ObjectKeyFromObject(one): true, client.ObjectKeyFromObject(two): true}},
		{source, map[client.ObjectKey]bool{client.ObjectKeyFromObject(one): true}},
		{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "first"}}, nil},
	} {
		requests := r.mapDependency(t.Context(), tc.event)
		if len(requests) != len(tc.want) {
			t.Fatalf("incorrect dependent count: %+v", requests)
		}
		for _, request := range requests {
			if !tc.want[request.NamespacedName] {
				t.Fatalf("unrelated Definition enqueued: %+v", request)
			}
		}
	}
}
