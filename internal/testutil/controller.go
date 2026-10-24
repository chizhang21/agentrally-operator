package testutil

import (
	"context"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/definition"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func Client(t *testing.T, objects ...client.Object) (client.WithWatch, *runtime.Scheme) {
	builder, scheme := ClientBuilder(t)
	return builder.WithObjects(objects...).Build(), scheme
}

// ClientBuilder shares scheme/status setup while allowing tests to add indexes.
func ClientBuilder(t *testing.T) (*Builder, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme, networkingv1.AddToScheme, v1.AddToScheme, sandbox.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.ClusterCapabilities{}, &v1.AgentGroup{}, &v1.AgentDefinition{}, &v1.AgentInstance{}, &corev1.Namespace{}, &corev1.ResourceQuota{}, sandbox.Object())
	return &Builder{ClientBuilder: builder, t: t, scheme: scheme}, scheme
}

type Builder struct {
	*fake.ClientBuilder
	t      *testing.T
	scheme *runtime.Scheme
}

func (b *Builder) WithObjects(objects ...client.Object) *Builder {
	b.ClientBuilder = b.ClientBuilder.WithObjects(objects...)
	return b
}
func (b *Builder) WithIndex(object client.Object, field string, extract client.IndexerFunc) *Builder {
	b.ClientBuilder = b.ClientBuilder.WithIndex(object, field, extract)
	return b
}
func (b *Builder) Build() client.WithWatch {
	c := b.ClientBuilder.Build()
	PublishDefinitions(b.t, c, b.scheme)
	return c
}

// PublishDefinitions prepares immutable fixtures through the production publisher.
// Definition controller lifecycle and authorization are covered separately.
func PublishDefinitions(t *testing.T, c client.Client, scheme *runtime.Scheme) {
	t.Helper()
	ctx := context.Background()
	objects := &v1.AgentDefinitionList{}
	if err := c.List(ctx, objects); err != nil {
		t.Fatal(err)
	}
	for i := range objects.Items {
		object := &objects.Items[i]
		if object.DeletionTimestamp != nil {
			continue
		}
		base := object.DeepCopy()
		revision, err := definition.Publish(ctx, c, c, scheme, object)
		condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: object.Generation, Reason: "ConfigurationPublished", Message: "Published fixture configuration"}
		if err != nil {
			condition.Status = metav1.ConditionFalse
			condition.Reason, condition.Message = diagnostics.Classify(err)
		} else {
			object.Status.CurrentRevision, object.Status.TemplateName = revision.ID, revision.Template.GetName()
		}
		object.Status.ObservedGeneration = object.Generation
		meta.SetStatusCondition(&object.Status.Conditions, condition)
		if !equality.Semantic.DeepEqual(base.Status, object.Status) {
			if err := c.Status().Update(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func Fixture() (*v1.AgentGroup, *corev1.Namespace, *v1.AgentDefinition, *v1.AgentInstance, *corev1.PodTemplateSpec) {
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "group-uid", Generation: 1}, Spec: v1.AgentGroupSpec{Namespace: "arally-team-agents"}}
	group.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "ResourcesPrepared", LastTransitionTime: metav1.Now()}}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: group.Spec.Namespace, UID: "namespace-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1.GroupVersion.String(), Kind: "AgentGroup", Name: group.Name,
				UID: group.UID, Controller: ptr.To(true),
			}},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	definition := &v1.AgentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "agent-template", Namespace: ns.Name, UID: "definition-uid", Generation: 1}, Spec: v1.AgentDefinitionSpec{AgentGroupRef: v1.AgentGroupReference{Name: group.Name}, TemplateRef: &v1.SandboxTemplateReference{Name: "agent-template"}}}
	agentInstance := &v1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "my-agent", Namespace: ns.Name, UID: "workload-uid", Generation: 1},
		Spec: v1.AgentInstanceSpec{
			AgentGroupRef: v1.AgentGroupReference{Name: group.Name},
			DefinitionRef: v1.AgentDefinitionReference{Name: definition.Name},
		},
	}
	source := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "custom"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "agent", Image: "example/agent:v1",
			Env: []corev1.EnvVar{{Name: "USER_OPTION", Value: "keep"}},
		}}},
	}
	return group, ns, definition, agentInstance, source
}

func Condition(t *testing.T, conditions []metav1.Condition, name string, want metav1.ConditionStatus) {
	t.Helper()
	c := meta.FindStatusCondition(conditions, name)
	if c == nil || c.Status != want {
		t.Fatalf("%s want %s, got %+v", name, want, c)
	}
}
