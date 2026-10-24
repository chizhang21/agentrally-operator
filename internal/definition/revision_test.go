package definition_test

import (
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/definition"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRevisionIdentityCompatibility(t *testing.T) {
	object := &v1.AgentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "arally-team", UID: "definition-uid"},
		Spec: v1.AgentDefinitionSpec{
			AgentGroupRef: v1.AgentGroupReference{Name: "team"},
			TemplateRef:   &v1.SandboxTemplateReference{Name: "source"},
		},
	}
	source := sandbox.ExtensionObject("SandboxTemplate")
	source.SetName("source")
	source.SetNamespace(object.Namespace)
	source.Object["spec"] = map[string]interface{}{
		"networkPolicyManagement": "Unmanaged",
		"podTemplate": map[string]interface{}{"spec": map[string]interface{}{
			"containers": []interface{}{map[string]interface{}{"name": "agent", "image": "example/agent:v1"}},
		}},
	}
	kube, scheme := testutil.Client(t, object, source)
	published, err := definition.Publish(t.Context(), kube, kube, scheme, object)
	if err != nil {
		t.Fatal(err)
	}
	// This persisted identity predates the shared hashing helper. Keep publication
	// and reads compatible with the existing serialized revision format.
	const revision = "8f9aa1f87a8c2692c8c154f98d94229ea08d10c202eb97d3938a167262175460"
	if published.ID != revision {
		t.Fatalf("publication identity changed: got %s, want %s", published.ID, revision)
	}
	stored, err := definition.Read(t.Context(), kube, object, revision)
	if err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(stored.Spec, object.Spec) || stored.Template.GetName() != published.Template.GetName() {
		t.Fatal("reading the existing revision changed its configuration or identity")
	}
}
