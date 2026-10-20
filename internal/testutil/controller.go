package testutil

import (
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
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
func ClientBuilder(t *testing.T) (*fake.ClientBuilder, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme, networkingv1.AddToScheme, v1.AddToScheme, sandbox.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.ClusterCapabilities{}, &v1.AgentGroup{}, &v1.AgentClaim{}, &corev1.Namespace{}, &corev1.ResourceQuota{}, sandbox.Object()), scheme
}

func Fixture() (*v1.AgentGroup, *corev1.Namespace, *v1.AgentClaim, *corev1.PodTemplateSpec) {
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
	agentClaim := &v1.AgentClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-agent", Namespace: ns.Name, UID: "workload-uid", Generation: 1},
		Spec: v1.AgentClaimSpec{
			AgentGroupRef: v1.AgentGroupReference{Name: group.Name},
			TemplateRef:   &v1.SandboxTemplateReference{Name: "agent-template"},
		},
	}
	source := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "custom"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "agent", Image: "example/agent:v1",
			Env: []corev1.EnvVar{{Name: "USER_OPTION", Value: "keep"}},
		}}},
	}
	return group, ns, agentClaim, source
}

func Condition(t *testing.T, conditions []metav1.Condition, name string, want metav1.ConditionStatus) {
	t.Helper()
	c := meta.FindStatusCondition(conditions, name)
	if c == nil || c.Status != want {
		t.Fatalf("%s want %s, got %+v", name, want, c)
	}
}
