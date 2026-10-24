package testutil_test

import (
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestClientConstructionLeavesDefinitionsUnpublished(t *testing.T) {
	_, _, definition, _, _ := testutil.Fixture()
	definition.Spec.TemplateRef = nil
	definition.Spec.Template = "generic"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example/agent:v1"}
	kube, scheme := testutil.Client(t, definition)
	current := &v1.AgentDefinition{}
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(definition), current); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(current.Status, definition.Status) {
		t.Fatal("constructing a client changed the seeded definition status")
	}
	revisions := sandbox.ExtensionList("SandboxTemplate")
	if err := kube.List(t.Context(), revisions); err != nil || len(revisions.Items) != 0 {
		t.Fatal("constructing a client published configuration", err)
	}

	testutil.PublishDefinitions(t, kube, scheme)
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(definition), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.CurrentRevision == "" {
		t.Fatal("explicit fixture publication did not publish the definition")
	}
}
