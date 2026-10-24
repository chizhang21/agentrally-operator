package agentinstance

import (
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/testutil"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDefinitionVersionSelectionAndIdentity(t *testing.T) {
	group, ns, object, instance, _ := testutil.Fixture()
	object.Spec.TemplateRef = nil
	object.Spec.Template = "generic"
	object.Spec.Container = &v1.AgentContainerSpec{Image: "example/agent:v1"}
	c, scheme := clientWithPublishedDefinitions(t, group, ns, object, instance)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, instance)
	pinned := getAgentInstance(t, c, instance).Status.Definition.DeepCopy()
	object.Spec.Container.Image = "example/agent:v2"
	saveDefinition(t, c, object)
	testutil.PublishDefinitions(t, c, scheme)
	// Explicit revision selects a retained publication even for a new identity.
	old := instance.DeepCopy()
	old.Name, old.UID = "old-version", "old-version-uid"
	old.Spec.Revision = pinned.Revision
	config, err := desiredExecution(ctx, c, old)
	if err != nil || config.PodTemplate.Spec.Containers[0].Image != "example/agent:v1" {
		t.Fatal("explicit old revision was not honored", err)
	}
	old.Spec.Revision = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := desiredExecution(ctx, c, old); err == nil {
		t.Fatal("missing revision was accepted")
	}
	// API names are insufficient to authorize silently switching definition identity.
	if err := c.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	object.UID, object.ResourceVersion = "replacement-definition", ""
	object.Status = v1.AgentDefinitionStatus{}
	if err := c.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
	testutil.PublishDefinitions(t, c, scheme)
	state := getAgentInstance(t, c, instance)
	if _, err := desiredExecution(ctx, c, state); err == nil {
		t.Fatal("recreated definition was silently adopted")
	} else if reason, _ := diagnostics.Classify(err); reason != "DefinitionIdentityChanged" {
		t.Fatal(err)
	}
	// A definition in another Group cannot be used even when an old revision exists.
	fresh := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name}, Spec: v1.AgentInstanceSpec{AgentGroupRef: v1.AgentGroupReference{Name: "other"}, DefinitionRef: instance.Spec.DefinitionRef}}
	if _, err := desiredExecution(ctx, c, fresh); err == nil {
		t.Fatal("cross-Group configuration accepted")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(instance), state); err != nil {
		t.Fatal(err)
	}
	if state.Status.Definition.UID != pinned.UID {
		t.Fatal("failure changed accepted identity")
	}
}
