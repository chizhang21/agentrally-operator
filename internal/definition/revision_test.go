package definition_test

import (
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPublishedRevisionRoundTrip(t *testing.T) {
	object := &v1.AgentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "agentrally-team", UID: "definition-uid"},
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
	stored, err := definition.Read(t.Context(), kube, object, published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(stored.Spec, object.Spec) || stored.Template.GetName() != published.Template.GetName() {
		t.Fatal("reading the published revision changed its configuration or identity")
	}
	// An upstream CRD upgrade defaults existing templates on read. That must
	// neither change their publication identity nor permit a real policy change.
	stored.Template.Object["spec"].(map[string]interface{})["failedPodPolicy"] = "Retain"
	if err := kube.Update(t.Context(), stored.Template); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Read(t.Context(), kube, object, published.ID); err != nil {
		t.Fatal("upstream default invalidated an accepted revision", err)
	}
	republished, err := definition.Publish(t.Context(), kube, kube, scheme, object)
	if err != nil || republished.ID != published.ID {
		t.Fatal("upstream default changed publication", err)
	}
	stored.Template.Object["spec"].(map[string]interface{})["failedPodPolicy"] = "Recreate"
	if _, err := definition.Verify(object, stored.Template, published.ID); err == nil {
		t.Fatal("a non-default policy change bypassed revision integrity")
	}
}
