package agentinstance

import (
	"context"
	"fmt"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func TestRestartRetainsAcceptedRevisionUntilExplicitUpgrade(t *testing.T) {
	group, ns, definition, instance, _ := testutil.Fixture()
	definition.Spec.TemplateRef = nil
	definition.Spec.Template = "generic"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example/agent:v1"}
	c, scheme := clientWithPublishedDefinitions(t, group, ns, definition, instance)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, instance)
	pinned := getAgentInstance(t, c, instance).Status.Definition.DeepCopy()
	definition.Spec.InvocationProtocol = "HTTPJSONSSEV1"
	definition.Spec.Container.Image = "example/agent:v2"
	saveDefinition(t, c, definition)
	testutil.PublishDefinitions(t, c, scheme)
	if err := c.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
		t.Fatal(err)
	}
	if definition.Status.CurrentRevision == pinned.Revision {
		t.Fatal("publishing a new invocation declaration did not change the revision")
	}
	for _, upgrade := range []bool{false, true} {
		state := getAgentInstance(t, c, instance)
		state.Spec.RestartToken = "restart"
		wantRevision, wantProtocol, wantImage := pinned.Revision, "", "example/agent:v1"
		if upgrade {
			state.Spec.RestartToken = "upgrade"
			state.Spec.Revision = definition.Status.CurrentRevision
			wantRevision, wantProtocol, wantImage = definition.Status.CurrentRevision, "HTTPJSONSSEV1", "example/agent:v2"
		}
		if err := c.Update(ctx, state); err != nil {
			t.Fatal(err)
		}
		for range 4 {
			reconcileWithUpstreamSimulation(t, r, instance)
		}
		state = getAgentInstance(t, c, instance)
		current, err := sandbox.Decode(getSandbox(t, c, instance))
		if err != nil || state.Status.Definition.Revision != wantRevision || state.Status.Definition.InvocationProtocol != wantProtocol || current.Spec.PodTemplate.Spec.Containers[0].Image != wantImage {
			t.Fatalf("upgrade=%v accepted the wrong revision/protocol/image: %+v, %v", upgrade, state.Status.Definition, err)
		}
	}
}

func TestPersistentWorkspaceRetryKeepsFirstRevision(t *testing.T) {
	group, ns, object, instance, _ := testutil.Fixture()
	group.Spec.PersistentWorkspaces, group.Status.NamespaceUID = true, ns.UID
	ns.OwnerReferences = nil
	object.Spec.TemplateRef, object.Spec.Template = nil, "generic"
	object.Spec.Container = &v1.AgentContainerSpec{Image: "example/agent:v1"}
	object.Status.StandaloneRevisions = true
	instance.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", Persistence: &v1.WorkspacePersistence{RetentionSeconds: 3600}, VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	builder, scheme := testutil.ClientBuilder(t)
	failPin := true
	c := builder.WithObjects(group, ns, object, instance).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID("uid-" + obj.GetName()))
			return c.Create(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*v1.AgentWorkspace); ok && failPin {
				failPin = false
				return fmt.Errorf("simulated PVC pin failure")
			}
			return c.SubResource(name).Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	testutil.PublishDefinitions(t, c, scheme)
	r := &Reconciler{Client: c, Scheme: scheme}
	instance.Spec.Workspace.MountPath = "/"
	if _, err := r.prepareExecutionConfiguration(ctx, instance, group, sandbox.Running); err == nil {
		t.Fatal("invalid mount path accepted")
	}
	records, pvcs := &v1.AgentWorkspaceList{}, &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := c.List(ctx, pvcs); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 0 || len(pvcs.Items) != 0 {
		t.Fatal("invalid mount created protected storage before validation")
	}
	instance.Spec.Workspace.MountPath = "/workspace"
	if _, err := r.prepareExecutionConfiguration(ctx, instance, group, sandbox.Running); err == nil {
		t.Fatal("expected interrupted workspace initialization")
	}
	state := getAgentInstance(t, c, instance)
	if state.Status.Definition == nil {
		t.Fatal("first revision was not pinned before storage creation")
	}
	pinned := *state.Status.Definition
	object.Spec.Container.Image = "example/agent:v2"
	saveDefinition(t, c, object)
	testutil.PublishDefinitions(t, c, scheme)
	if _, err := r.prepareExecutionConfiguration(ctx, state, group, sandbox.Running); err != nil {
		t.Fatal(err)
	}
	state = getAgentInstance(t, c, instance)
	if *state.Status.Definition != pinned || state.Status.Workspace.UID == "" {
		t.Fatalf("retry lost the original revision or storage binding: %+v", state.Status)
	}
}
