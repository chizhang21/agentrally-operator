package agentclaim

import (
	"context"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBuiltinWorkspaceVolumePermissions(t *testing.T) {
	for _, category := range []string{"generic", "assistant", "coding"} {
		for _, tc := range []struct {
			name, annotation string
			readOnly, noPVC  bool
			wantGroup        bool
		}{
			{name: "writable-pvc", wantGroup: true},
			{name: "read-only-pvc", readOnly: true},
			{name: "ephemeral", noPVC: true},
			{name: "openshift-uid-range", annotation: "openshift.io/sa.scc.uid-range"},
			{name: "openshift-group-range", annotation: "openshift.io/sa.scc.supplemental-groups"},
		} {
			t.Run(category+"/"+tc.name, func(t *testing.T) {
				g, ns, w, _ := testutil.Fixture()
				w.Spec.TemplateRef, w.Spec.Template = nil, category
				w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
				if !tc.noPVC {
					w.Spec.Workspace = &v1.WorkspaceMount{ClaimName: "workspace", MountPath: "/workspace", ReadOnly: tc.readOnly}
				}
				if tc.annotation != "" {
					ns.Annotations = map[string]string{tc.annotation: "1000700000/10000"}
				}
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}
				c, _ := testutil.Client(t, g, ns, w, pvc)
				before := w.DeepCopy()
				pod, _, err := desiredExecution(ctx, c, w)
				if err != nil {
					t.Fatal(err)
				}
				sc := pod.Spec.SecurityContext
				if tc.wantGroup {
					if sc == nil || sc.FSGroup == nil || *sc.FSGroup != 1000 || sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
						t.Fatalf("writable workspace has no usable volume group: %+v", sc)
					}
				} else if sc != nil && (sc.FSGroup != nil || sc.FSGroupChangePolicy != nil) {
					t.Fatalf("volume defaults changed an unaffected workload: %+v", sc)
				}
				if !reflect.DeepEqual(before, w) || len(pod.Spec.InitContainers) != 0 || sc.RunAsUser != nil || !*pod.Spec.Containers[0].SecurityContext.RunAsNonRoot {
					t.Fatal("volume permissions must not change the Claim, process UID or privilege model")
				}
			})
		}
	}
}

func TestWorkspacePermissionsPreserveNativeTemplates(t *testing.T) {
	for _, sc := range []*corev1.PodSecurityContext{nil, {FSGroup: ptr.To(int64(4000)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeAlways)}} {
		g, ns, w, source := testutil.Fixture()
		w.Spec.Workspace = &v1.WorkspaceMount{ClaimName: "workspace", MountPath: "/workspace"}
		source.Spec.SecurityContext = sc
		object := nativeTemplate(w, source)
		before := object.DeepCopy()
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}
		c, _ := testutil.Client(t, g, ns, w, object, pvc)
		pod, _, err := desiredExecution(ctx, c, w)
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

func TestBuiltinWorkspaceNamespaceReadFailure(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "coding"
	w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
	w.Spec.Workspace = &v1.WorkspaceMount{ClaimName: "workspace", MountPath: "/workspace"}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}
	c, _ := testutil.Client(t, g, ns, w, pvc)
	guard := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if _, ok := object.(*corev1.Namespace); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, key.Name, nil)
		}
		return c.Get(ctx, key, object, opts...)
	}})
	_, _, err := desiredExecution(ctx, guard, w)
	if reason, _ := diagnostics.Classify(err); reason != "DependencyAccessDenied" {
		t.Fatalf("unreadable namespace must not silently select a volume group: %v", err)
	}
}

func TestBuiltinWorkspacePermissionsReachAllocation(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "coding"
	w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
	w.Spec.Workspace = &v1.WorkspaceMount{ClaimName: "workspace", MountPath: "/workspace"}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}
	c, scheme := testutil.Client(t, g, ns, w, pvc)
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: c, Scheme: scheme}, w)
	state := getAgentClaim(t, c, w)
	if state.Status.Provisioning == nil {
		t.Fatal("workspace allocation was not created")
	}
	object := sandbox.ExtensionObject("SandboxTemplate")
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: state.Status.Provisioning.TemplateName}, object); err != nil {
		t.Fatal(err)
	}
	group, _, _ := unstructured.NestedInt64(object.Object, "spec", "podTemplate", "spec", "securityContext", "fsGroup")
	if group != 1000 {
		t.Fatal("volume group did not reach the pool's SandboxTemplate")
	}
}

func TestWorkspaceNamespaceDependencyMapping(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "coding"
	w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
	w.Spec.Workspace = &v1.WorkspaceMount{ClaimName: "workspace", MountPath: "/workspace"}
	other := w.DeepCopy()
	other.Name, other.Namespace = "other", "arally-other"
	readOnly := w.DeepCopy()
	readOnly.Name, readOnly.Spec.Workspace.ReadOnly = "read-only", true
	c, scheme := indexedClient(t, g, ns, w, other, readOnly)
	r := &Reconciler{Client: c, Scheme: scheme}
	requests := r.mapDependency(ctx, ns)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(w) {
		t.Fatalf("namespace policy changes did not map to the affected workspace: %+v", requests)
	}
}
