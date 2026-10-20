package agentclaim

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func TestNativeTemplateIsSingleExecutionSource(t *testing.T) {
	for _, service := range []bool{false, true} {
		g, ns, w, source := testutil.Fixture()
		source.Spec.RestartPolicy = corev1.RestartPolicyNever
		source.Spec.ActiveDeadlineSeconds = ptr.To(int64(321))
		source.Spec.AutomountServiceAccountToken = ptr.To(true)
		source.Spec.Containers = append(source.Spec.Containers, corev1.Container{Name: "sidecar", Image: "sidecar:v1"})
		object := nativeTemplate(w, source)
		object.SetLabels(map[string]string{"arally.io/category": "assistant"})
		_ = unstructured.SetNestedField(object.Object, service, "spec", "service")
		before := object.DeepCopy()
		c, scheme := testutil.Client(t, object, g, ns, w)
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), before); err != nil {
			t.Fatal(err)
		}
		resolved, err := resolveSandboxTemplate(ctx, c, w)
		if err != nil || !reflect.DeepEqual(resolved.PodTemplate, source) || resolved.Service != service {
			t.Fatalf("native template changed by category: %+v %v", resolved, err)
		}
		r := &Reconciler{Client: c, Scheme: scheme}
		reconcileWithUpstreamSimulation(t, r, w)
		native, err := sandbox.Decode(getSandbox(t, c, w))
		if err != nil || native.Spec.Service != service || native.Spec.PodTemplate.Spec.RestartPolicy != corev1.RestartPolicyNever || *native.Spec.PodTemplate.Spec.ActiveDeadlineSeconds != 321 || len(native.Spec.PodTemplate.Spec.Containers) != 2 {
			t.Fatalf("execution did not use native spec: %+v %v", native, err)
		}
		if native.Spec.PodTemplate.Spec.AutomountServiceAccountToken == nil || !*native.Spec.PodTemplate.Spec.AutomountServiceAccountToken {
			t.Fatal("explicit token mount choice was overwritten")
		}
		current := sandbox.ExtensionObject("SandboxTemplate")
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil || !reflect.DeepEqual(before, current) {
			t.Fatalf("Arally mutated template: %v", err)
		}
	}
}

func TestNativeTemplateResolutionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		edit         func(*v1.AgentClaim, *unstructured.Unstructured)
	}{
		{"missing", "DependencyNotFound", func(w *v1.AgentClaim, _ *unstructured.Unstructured) { w.Spec.TemplateRef.Name = "missing" }},
		{"empty-reference", "InvalidConfiguration", func(w *v1.AgentClaim, _ *unstructured.Unstructured) { w.Spec.TemplateRef.Name = "" }},
		{"invalid-reference", "InvalidConfiguration", func(w *v1.AgentClaim, _ *unstructured.Unstructured) { w.Spec.TemplateRef.Name = "Other/Template" }},
		{"cross-namespace", "DependencyNotFound", func(_ *v1.AgentClaim, s *unstructured.Unstructured) { s.SetNamespace("other") }},
		{"terminating", "DependencyTerminating", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			now := metav1.Now()
			s.SetDeletionTimestamp(&now)
			s.SetFinalizers([]string{"test"})
		}},
		{"managed-policy", "TemplatePolicyUnsupported", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "Managed", "spec", "networkPolicyManagement")
		}},
		{"implicit-managed-policy", "TemplatePolicyUnsupported", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			unstructured.RemoveNestedField(s.Object, "spec", "networkPolicyManagement")
		}},
		{"ignored-network-policy", "TemplatePolicyUnsupported", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(s.Object, map[string]interface{}{"ingress": []interface{}{}}, "spec", "networkPolicy")
		}},
		{"allocated-pvc", "TemplatePolicyUnsupported", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(s.Object, []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}, "spec", "volumeClaimTemplates")
		}},
		{"missing-pod", "InvalidConfiguration", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			unstructured.RemoveNestedField(s.Object, "spec", "podTemplate")
		}},
		{"missing-image", "InvalidConfiguration", func(_ *v1.AgentClaim, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(s.Object, []interface{}{map[string]interface{}{"name": "agent"}}, "spec", "podTemplate", "spec", "containers")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, w, source := testutil.Fixture()
			object := nativeTemplate(w, source)
			tc.edit(w, object)
			c, _ := testutil.Client(t, object)
			_, err := resolveSandboxTemplate(ctx, c, w)
			if reason, _ := diagnostics.Classify(err); reason != tc.reason {
				t.Fatalf("want %s got %v", tc.reason, err)
			}
		})
	}
}

func TestTemplateAPIUnavailableBlocksAllocation(t *testing.T) {
	g, ns, w, source := testutil.Fixture()
	w.Spec.Provisioning = &v1.AgentClaimProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "pool"}}
	c, scheme := testutil.Client(t, nativeTemplate(w, source), g, ns, w)
	guard := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if object.GetObjectKind().GroupVersionKind().Kind == "SandboxTemplate" {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: sandbox.ExtensionsGroupVersion.Group, Kind: "SandboxTemplate"}}
		}
		return c.Get(ctx, key, object, opts...)
	}})
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: guard, Scheme: scheme}, w)
	assertReason(t, getAgentClaim(t, c, w), "Ready", "TemplateAPIUnavailable")
	assertNoSandbox(t, c, w)
}

func TestPoolRejectsDifferentTemplateIdentity(t *testing.T) {
	w, c, scheme := setupPooledAgentClaim(t)
	selected := sandbox.ExtensionObject("SandboxTemplate")
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Spec.TemplateRef.Name}, selected); err != nil {
		t.Fatal(err)
	}
	selected.SetName("same-spec-other-template")
	selected.SetResourceVersion("")
	if err := c.Create(ctx, selected); err != nil {
		t.Fatal(err)
	}
	w.Spec.TemplateRef.Name = selected.GetName()
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: c, Scheme: scheme}, w)
	assertReason(t, getAgentClaim(t, c, w), "Ready", "PoolTemplateMismatch")
	claim := desiredPoolClaim(w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), claim); !apierrors.IsNotFound(err) {
		t.Fatalf("allocated mismatched template: %v", err)
	}
}

func TestNativeTemplateExamples(t *testing.T) {
	paths, err := filepath.Glob("../../../examples/*-template.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatal("native template samples are missing")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			object := sandbox.ExtensionObject("SandboxTemplate")
			if err := yaml.Unmarshal(data, object); err != nil {
				t.Fatal(err)
			}
			w := &v1.AgentClaim{ObjectMeta: metav1.ObjectMeta{Namespace: object.GetNamespace()}, Spec: v1.AgentClaimSpec{TemplateRef: &v1.SandboxTemplateReference{Name: object.GetName()}}}
			c, _ := testutil.Client(t, object)
			resolved, err := resolveSandboxTemplate(ctx, c, w)
			if err != nil {
				t.Fatal(err)
			}
			if object.GetLabels()["arally.io/category"] == "" || *resolved.PodTemplate.Spec.AutomountServiceAccountToken {
				t.Fatal("sample missing classification or token protection")
			}
		})
	}
}

func TestAllocatedAgentReportsTemplateIdentityDriftWithoutReplacement(t *testing.T) {
	w, c, scheme := setupPooledAgentClaim(t)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	allocated := assignPoolSandbox(t, c, w, true)
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentClaim(t, c, w)
	if state.Status.Provisioning.CompletedAt == nil {
		t.Fatal("allocation not completed")
	}
	selected := sandbox.ExtensionObject("SandboxTemplate")
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Spec.TemplateRef.Name}, selected); err != nil {
		t.Fatal(err)
	}
	selected.SetName("same-spec-other-template")
	selected.SetResourceVersion("")
	if err := c.Create(ctx, selected); err != nil {
		t.Fatal(err)
	}
	state.Spec.TemplateRef.Name = selected.GetName()

	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentClaim(t, c, w)
	assertReason(t, state, "ConfigurationAccepted", "PoolTemplateMismatch")
	if state.Status.Provisioning.Mode != "Pool" || state.Status.SandboxName != allocated.GetName() {
		t.Fatal("template drift replaced the allocated execution")
	}
	assertNoSandbox(t, c, w)
}

func TestSuspensionSurvivesMissingTemplateAndPreservesService(t *testing.T) {
	g, ns, w, source := testutil.Fixture()
	object := nativeTemplate(w, source)
	c, scheme := testutil.Client(t, object, g, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	if err := c.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	state := getAgentClaim(t, c, w)
	state.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	native, err := sandbox.Decode(getSandbox(t, c, w))
	if err != nil || native.Spec.OperatingMode != sandbox.Suspended || !native.Spec.Service {
		t.Fatalf("suspension changed existing service or failed: %+v %v", native, err)
	}
	assertReason(t, getAgentClaim(t, c, w), "ConfigurationAccepted", "SuspendedUsingExistingConfiguration")
}
