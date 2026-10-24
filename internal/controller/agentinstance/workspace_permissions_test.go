package agentinstance

import (
	"context"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBuiltinWorkspaceVolumePermissions(t *testing.T) {
	for _, category := range []string{"generic", "assistant", "coding"} {
		for _, tc := range []struct {
			name            string
			uid             *int64
			readOnly, noPVC bool
			wantGroup       bool
		}{
			{name: "explicit-uid", uid: ptr.To(int64(1000)), wantGroup: true},
			{name: "different-uid", uid: ptr.To(int64(2345)), wantGroup: true},
			{name: "admission-default"},
			{name: "read-only-pvc", uid: ptr.To(int64(2345)), readOnly: true},
			{name: "ephemeral", uid: ptr.To(int64(2345)), noPVC: true},
		} {
			t.Run(category+"/"+tc.name, func(t *testing.T) {
				g, ns, definition, w, _ := testutil.Fixture()
				definition.Spec.TemplateRef, definition.Spec.Template = nil, category
				definition.Spec.Container = &v1.AgentContainerSpec{Image: "example.com/agent:v1"}
				if !tc.noPVC {
					w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: ptr.To(workspaceVolumeSpec()), MountPath: "/workspace", ReadOnly: tc.readOnly}
				}
				definition.Spec.Security = &v1.AgentSecurity{RunAsUser: tc.uid}
				c, _ := testutil.Client(t, definition, g, ns, w)
				before := w.DeepCopy()
				guard := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1.Namespace); ok {
						t.Fatal("workspace permissions read namespace-specific policy")
					}
					return c.Get(ctx, key, object, opts...)
				}})
				pod, _, err := desiredExecutionForTest(ctx, guard, w)
				if err != nil {
					t.Fatal(err)
				}
				sc := pod.Spec.SecurityContext
				if tc.wantGroup {
					if sc == nil || sc.FSGroup == nil || *sc.FSGroup != *tc.uid || sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
						t.Fatalf("writable workspace has no usable volume group: %+v", sc)
					}
				} else if sc != nil && (sc.FSGroup != nil || sc.FSGroupChangePolicy != nil) {
					t.Fatalf("volume defaults changed an unaffected workload: %+v", sc)
				}
				if !reflect.DeepEqual(before, w) || len(pod.Spec.InitContainers) != 0 || !reflect.DeepEqual(sc.RunAsUser, tc.uid) || !*pod.Spec.Containers[0].SecurityContext.RunAsNonRoot {
					t.Fatal("volume permissions must not change the Claim, process UID or privilege model")
				}
			})
		}
	}
}

func TestWorkspacePermissionsPreserveNativeTemplates(t *testing.T) {
	for _, sc := range []*corev1.PodSecurityContext{nil, {FSGroup: ptr.To(int64(4000)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeAlways)}} {
		g, ns, definition, w, source := testutil.Fixture()
		w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: ptr.To(workspaceVolumeSpec()), MountPath: "/workspace"}
		source.Spec.SecurityContext = sc
		object := nativeTemplate(definition, source)
		before := object.DeepCopy()
		c, _ := testutil.Client(t, definition, g, ns, w, object)
		pod, _, err := desiredExecutionForTest(ctx, c, w)
		if err != nil || !reflect.DeepEqual(pod.Spec.SecurityContext, sc) {
			t.Fatalf("native volume permissions changed: %+v %v", pod, err)
		}
		current := sandbox.ExtensionObject("SandboxTemplate")
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before.Object["spec"], current.Object["spec"]) {
			t.Fatal("source SandboxTemplate was mutated")
		}
	}
}

func TestBuiltinWorkspacePermissionsReachAllocation(t *testing.T) {
	g, ns, definition, w, _ := testutil.Fixture()
	definition.Spec.TemplateRef, definition.Spec.Template = nil, "coding"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example.com/agent:v1"}
	definition.Spec.Security = &v1.AgentSecurity{RunAsUser: ptr.To(int64(2345))}
	w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: ptr.To(workspaceVolumeSpec()), MountPath: "/workspace"}
	c, scheme := testutil.Client(t, definition, g, ns, w)
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: c, Scheme: scheme}, w)
	state := getAgentInstance(t, c, w)
	if state.Status.Provisioning == nil {
		t.Fatal("workspace allocation was not created")
	}
	object := sandbox.ExtensionObject("SandboxTemplate")
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: state.Status.Provisioning.TemplateName}, object); err != nil {
		t.Fatal(err)
	}
	group, _, _ := unstructured.NestedInt64(object.Object, "spec", "podTemplate", "spec", "securityContext", "fsGroup")
	if group != 2345 {
		t.Fatal("volume group did not reach the pool's SandboxTemplate")
	}
}
