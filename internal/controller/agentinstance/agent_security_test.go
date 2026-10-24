package agentinstance

import (
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAnyUIDRequiresNamespaceAndDedicatedAccount(t *testing.T) {
	for _, tc := range []struct {
		name, policy, account string
		want                  string
		builtin               bool
	}{
		{"missing-account-name", "baseline", "", "SecurityAccountRequired", false},
		{"default-account", "baseline", "default", "SecurityAccountRequired", false},
		{"restricted-namespace", "restricted", "legacy", "SecurityPolicyDenied", false},
		{"missing-account", "baseline", "missing", "DependencyNotFound", false},
		{"explicit-root", "baseline", "legacy", "", false},
		{"builtin-explicit-root", "baseline", "legacy", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, definition, w, source := testutil.Fixture()
			source.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
			source.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: ptr.To("profiles/agent.json")}}
			if tc.builtin {
				definition.Spec.TemplateRef = nil
				definition.Spec.Template = "generic"
				definition.Spec.Container = &v1.AgentContainerSpec{Image: "example/legacy:v1"}
			}
			definition.Spec.Security = &v1.AgentSecurity{Profile: "AnyUID", ServiceAccountName: tc.account, RunAsUser: ptr.To(int64(0))}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: w.Namespace, Labels: map[string]string{"pod-security.kubernetes.io/enforce": tc.policy}}}
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: w.Namespace}}
			objects := []client.Object{ns, sa}
			if !tc.builtin {
				objects = append(objects, nativeTemplate(definition, source))
			}
			c, _ := clientWithPublishedDefinitions(t, append(objects, definition)...)
			before := w.DeepCopy()
			template, err := desiredPodTemplate(ctx, c, w)
			reason, _ := diagnostics.Classify(err)
			if reason != tc.want {
				t.Fatalf("want %s got %v", tc.want, err)
			}
			if tc.want != "" {
				return
			}
			if template.Spec.ServiceAccountName != "legacy" || *template.Spec.Containers[0].SecurityContext.RunAsNonRoot || *template.Spec.Containers[0].SecurityContext.RunAsUser != 0 {
				t.Fatalf("AnyUID not propagated: %+v", template)
			}
			if len(template.Annotations) != 0 {
				t.Fatal("process identity injected admission annotations")
			}
			if template.Spec.SecurityContext.SeccompProfile == nil || template.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Fatal("UID selection removed the declared seccomp profile")
			}
			if !tc.builtin && !reflect.DeepEqual(template.Spec.Containers[0].SecurityContext.SeccompProfile, source.Spec.Containers[0].SecurityContext.SeccompProfile) {
				t.Fatal("UID selection changed the container's explicit seccomp profile")
			}
			if *template.Spec.AutomountServiceAccountToken {
				t.Fatal("API credentials enabled")
			}
			if !reflect.DeepEqual(before, w) || source.Annotations != nil {
				t.Fatal("claim mutated")
			}
			accounts := &corev1.ServiceAccountList{}
			if err := c.List(ctx, accounts, client.InNamespace(w.Namespace)); err != nil || len(accounts.Items) != 1 {
				t.Fatal("account ownership changed")
			}
		})
	}
}

func TestRestrictedRejectsRoot(t *testing.T) {
	_, _, definition, w, source := testutil.Fixture()
	definition.Spec.Security = &v1.AgentSecurity{Profile: "Restricted", RunAsUser: ptr.To(int64(0))}
	c, _ := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source))
	if _, err := desiredPodTemplate(ctx, c, w); err == nil {
		t.Fatal("root allowed")
	}
}
