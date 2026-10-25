package agentinstance

import (
	"context"
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNativeTemplateIsSingleExecutionSource(t *testing.T) {
	for _, service := range []bool{false, true} {
		g, ns, definition, w, source := testutil.Fixture()
		source.Spec.RestartPolicy = corev1.RestartPolicyNever
		source.Spec.ActiveDeadlineSeconds = ptr.To(int64(321))
		source.Spec.AutomountServiceAccountToken = ptr.To(true)
		source.Spec.Containers = append(source.Spec.Containers, corev1.Container{Name: "sidecar", Image: "sidecar:v1"})
		object := nativeTemplate(definition, source)
		object.SetLabels(map[string]string{"arally.io/category": "assistant"})
		_ = unstructured.SetNestedField(object.Object, service, "spec", "service")
		before := object.DeepCopy()
		c, scheme := clientWithPublishedDefinitions(t, definition, object, g, ns, w)
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
			t.Fatalf("AgentRally mutated template: %v", err)
		}
	}
}

func TestNativeTemplateResolutionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		edit         func(*v1.AgentDefinition, *unstructured.Unstructured)
	}{
		{"missing", "DependencyNotFound", func(definition *v1.AgentDefinition, _ *unstructured.Unstructured) {
			definition.Spec.TemplateRef.Name = "missing"
		}},
		{"empty-reference", "InvalidConfiguration", func(definition *v1.AgentDefinition, _ *unstructured.Unstructured) {
			definition.Spec.TemplateRef.Name = ""
		}},
		{"invalid-reference", "InvalidConfiguration", func(definition *v1.AgentDefinition, _ *unstructured.Unstructured) {
			definition.Spec.TemplateRef.Name = "Other/Template"
		}},
		{"cross-namespace", "DependencyNotFound", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) { s.SetNamespace("other") }},
		{"terminating", "DependencyTerminating", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			now := metav1.Now()
			s.SetDeletionTimestamp(&now)
			s.SetFinalizers([]string{"test"})
		}},
		{"managed-policy", "TemplatePolicyUnsupported", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "Managed", "spec", "networkPolicyManagement")
		}},
		{"implicit-managed-policy", "TemplatePolicyUnsupported", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			unstructured.RemoveNestedField(s.Object, "spec", "networkPolicyManagement")
		}},
		{"ignored-network-policy", "TemplatePolicyUnsupported", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(s.Object, map[string]interface{}{"ingress": []interface{}{}}, "spec", "networkPolicy")
		}},
		{"invalid-volume-template", "InvalidConfiguration", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "invalid", "spec", "volumeClaimTemplates")
		}},
		{"missing-pod", "InvalidConfiguration", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			unstructured.RemoveNestedField(s.Object, "spec", "podTemplate")
		}},
		{"missing-image", "InvalidConfiguration", func(_ *v1.AgentDefinition, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(s.Object, []interface{}{map[string]interface{}{"name": "agent"}}, "spec", "podTemplate", "spec", "containers")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, definition, w, source := testutil.Fixture()
			object := nativeTemplate(definition, source)
			tc.edit(definition, object)
			c, _ := clientWithPublishedDefinitions(t, definition, object)
			_, err := resolveSandboxTemplate(ctx, c, w)
			if reason, _ := diagnostics.Classify(err); reason != tc.reason {
				t.Fatalf("want %s got %v", tc.reason, err)
			}
		})
	}
}

func TestTemplateAPIUnavailableBlocksAllocation(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	w.Spec.Provisioning = &v1.AgentInstanceProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "pool"}}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w)
	guard := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if object.GetObjectKind().GroupVersionKind().Kind == "SandboxTemplate" {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: sandbox.ExtensionsGroupVersion.Group, Kind: "SandboxTemplate"}}
		}
		return c.Get(ctx, key, object, opts...)
	}})
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: guard, Scheme: scheme}, w)
	assertReason(t, getAgentInstance(t, c, w), "Ready", "TemplateAPIUnavailable")
	assertNoSandbox(t, c, w)
}

func TestSuspensionSurvivesMissingTemplateAndPreservesService(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	object := nativeTemplate(definition, source)
	c, scheme := clientWithPublishedDefinitions(t, definition, object, g, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	if err := c.Delete(ctx, object); err != nil {
		t.Fatal(err)
	}
	state := getAgentInstance(t, c, w)
	state.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	native, err := sandbox.Decode(getSandbox(t, c, w))
	if err != nil || native.Spec.OperatingMode != sandbox.Suspended || !native.Spec.Service {
		t.Fatalf("suspension changed existing service or failed: %+v %v", native, err)
	}
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "ConfigurationAccepted", metav1.ConditionTrue)
}
