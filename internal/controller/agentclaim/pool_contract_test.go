package agentclaim

import (
	"reflect"
	"strings"
	"testing"

	arallyv1alpha1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

func poolFixture(t *testing.T) (*unstructured.Unstructured, *unstructured.Unstructured, *corev1.PodTemplateSpec) {
	t.Helper()
	pod := &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "agent"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "example/agent:v1", Env: []corev1.EnvVar{{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}, Key: "token"}}}}}}}}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		t.Fatal(err)
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName("warm")
	pool.SetNamespace("agents")
	pool.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": "template"}}
	template := sandbox.ExtensionObject("SandboxTemplate")
	template.SetName("template")
	template.SetNamespace("agents")
	template.Object["spec"] = map[string]interface{}{"podTemplate": fields, "networkPolicyManagement": "Unmanaged"}
	return pool, template, pod
}

func TestPoolClaimContainsOnlyAllocationReference(t *testing.T) {
	w := &arallyv1alpha1.AgentClaim{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 253), Namespace: "agents", UID: "workload-1"}, Spec: arallyv1alpha1.AgentClaimSpec{Provisioning: &arallyv1alpha1.AgentClaimProvisioning{WarmPoolRef: &arallyv1alpha1.WarmPoolReference{Name: "warm"}}}}
	claim := desiredPoolClaim(w)
	if claim.GetNamespace() != w.Namespace || len(validation.IsDNS1035Label(claim.GetName())) > 0 {
		t.Fatalf("invalid claim identity: %s/%s", claim.GetNamespace(), claim.GetName())
	}
	want := map[string]interface{}{"warmPoolRef": map[string]interface{}{"name": "warm"}}
	if !reflect.DeepEqual(claim.Object["spec"], want) {
		t.Fatalf("claim copied configuration: %#v", claim.Object["spec"])
	}
	if desiredPoolClaim(w).GetName() != claim.GetName() {
		t.Fatal("claim name is not deterministic")
	}
	w.UID = "workload-2"
	if desiredPoolClaim(w).GetName() == claim.GetName() {
		t.Fatal("recreated workload reused a previous claim identity")
	}
	if desiredPoolClaim(nil) != nil {
		t.Fatal("nil workload must not produce a claim")
	}
	for _, name := range []string{"agent.with.dots", "123agent", strings.Repeat("a", 50)} {
		w.Name = name
		if errors := validation.IsDNS1035Label(desiredPoolClaim(w).GetName()); len(errors) != 0 {
			t.Fatalf("cold Sandbox cannot create its Service for %q: %v", name, errors)
		}
	}
	// Upgrades keep the already-recorded native Claim identity for observation/cleanup.
	w.Status.Provisioning = &arallyv1alpha1.AgentClaimProvisioningStatus{ClaimName: "legacy.name-claim-1234567890abcdef"}
	if existing := (&Reconciler{}).allocationClaim(w); existing.GetName() != w.Status.Provisioning.ClaimName {
		t.Fatal("recorded allocation name changed during upgrade")
	}
}

func TestPoolTemplateContract(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*unstructured.Unstructured, *unstructured.Unstructured, *corev1.PodTemplateSpec)
		service bool
		reason  string
	}{
		{name: "nil token and absent service are false"},
		{name: "native secret references remain references"},
		{name: "implicit upstream DNS rejected", reason: "PoolTemplateMismatch", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			unstructured.RemoveNestedField(template.Object, "spec", "networkPolicyManagement")
		}},
		{name: "explicit DNS preserves upstream behavior", mutate: func(_, template *unstructured.Unstructured, pod *corev1.PodTemplateSpec) {
			unstructured.RemoveNestedField(template.Object, "spec", "networkPolicyManagement")
			pod.Spec.DNSPolicy = corev1.DNSClusterFirst
			_ = unstructured.SetNestedField(template.Object, string(corev1.DNSClusterFirst), "spec", "podTemplate", "spec", "dnsPolicy")
		}},
		{name: "explicit service enabled", service: true, mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			_ = unstructured.SetNestedField(template.Object, true, "spec", "service")
		}},
		{name: "service mismatch", service: true, reason: "PoolTemplateMismatch"},
		{name: "cross namespace", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			template.SetNamespace("other")
		}, reason: "PoolTemplateMismatch"},
		{name: "wrong template", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) { template.SetName("wrong") }, reason: "PoolTemplateMismatch"},
		{name: "terminating", mutate: func(pool, _ *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			now := metav1.Now()
			pool.SetDeletionTimestamp(&now)
		}, reason: "DependencyTerminating"},
		{name: "PVC allocation rejected", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			_ = unstructured.SetNestedSlice(template.Object, []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "data"}}}, "spec", "volumeClaimTemplates")
		}, reason: "PoolTemplateMismatch"},
		{name: "token true mismatches default false", mutate: func(_, _ *unstructured.Unstructured, pod *corev1.PodTemplateSpec) {
			value := true
			pod.Spec.AutomountServiceAccountToken = &value
		}, reason: "PoolTemplateMismatch"},
		{name: "image mismatch", mutate: func(_, _ *unstructured.Unstructured, pod *corev1.PodTemplateSpec) {
			pod.Spec.Containers[0].Image = "example/agent:v2"
		}, reason: "PoolTemplateMismatch"},
		{name: "extra declared sidecar rejected", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			containers, _, _ := unstructured.NestedSlice(template.Object, "spec", "podTemplate", "spec", "containers")
			containers = append(containers, map[string]interface{}{"name": "extra", "image": "example/sidecar:v1"})
			_ = unstructured.SetNestedSlice(template.Object, containers, "spec", "podTemplate", "spec", "containers")
		}, reason: "PoolTemplateMismatch"},
		{name: "extra declared volume rejected", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			_ = unstructured.SetNestedSlice(template.Object, []interface{}{map[string]interface{}{"name": "extra", "emptyDir": map[string]interface{}{}}}, "spec", "podTemplate", "spec", "volumes")
		}, reason: "PoolTemplateMismatch"},
		{name: "env mismatch", mutate: func(_, _ *unstructured.Unstructured, pod *corev1.PodTemplateSpec) {
			pod.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name = "different"
		}, reason: "PoolTemplateMismatch"},
		{name: "metadata mismatch", mutate: func(_, _ *unstructured.Unstructured, pod *corev1.PodTemplateSpec) { pod.Labels["app"] = "different" }, reason: "PoolTemplateMismatch"},
		{name: "upstream metadata ignored", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			_ = unstructured.SetNestedField(template.Object, "warm", "spec", "podTemplate", "metadata", "labels", "agents.x-k8s.io/pool")
		}},
		{name: "wrong kind", mutate: func(pool, _ *unstructured.Unstructured, _ *corev1.PodTemplateSpec) { pool.SetKind("SandboxClaim") }, reason: "PoolTemplateMismatch"},
		{name: "malformed pod", mutate: func(_, template *unstructured.Unstructured, _ *corev1.PodTemplateSpec) {
			_ = unstructured.SetNestedField(template.Object, "invalid", "spec", "podTemplate")
		}, reason: "PoolTemplateMismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, template, pod := poolFixture(t)
			if tc.mutate != nil {
				tc.mutate(pool, template, pod)
			}
			beforePool, beforeTemplate, beforePod := pool.DeepCopy(), template.DeepCopy(), pod.DeepCopy()
			err := validatePoolTemplate(pool, template, pod, tc.service)
			reason, _ := diagnostics.Classify(err)
			if reason != tc.reason {
				t.Fatalf("got %s (%v), want %s", reason, err, tc.reason)
			}
			if !reflect.DeepEqual(pool, beforePool) || !reflect.DeepEqual(template, beforeTemplate) || !reflect.DeepEqual(pod, beforePod) {
				t.Fatal("preflight mutated inputs")
			}
		})
	}
}

func TestPoolDeclaredConfigurationIsStrict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
	}{
		{"node selector", func(spec *corev1.PodSpec) { spec.NodeSelector = map[string]string{"dedicated": "agents"} }},
		{"toleration", func(spec *corev1.PodSpec) {
			spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
		}},
		{"pod security", func(spec *corev1.PodSpec) {
			uid := int64(1234)
			spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: &uid}
		}},
		{"container security", func(spec *corev1.PodSpec) {
			uid := int64(1234)
			spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &uid}
		}},
		{"node name", func(spec *corev1.PodSpec) { spec.NodeName = "worker-1" }},
		{"sidecar", func(spec *corev1.PodSpec) {
			spec.Containers = append(spec.Containers, corev1.Container{Name: "extra", Image: "example/extra:v1"})
		}},
		{"volume", func(spec *corev1.PodSpec) {
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, template, desired := poolFixture(t)
			actual := desired.DeepCopy()
			tc.mutate(&actual.Spec)
			fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(actual)
			if err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedMap(template.Object, fields, "spec", "podTemplate"); err != nil {
				t.Fatal(err)
			}
			if reason, _ := diagnostics.Classify(validatePoolTemplate(pool, template, desired, false)); reason != "PoolTemplateMismatch" {
				t.Fatalf("preflight accepted declared difference: %s", reason)
			}
			current := &sandbox.Sandbox{Spec: sandbox.Spec{PodTemplate: *actual}}
			if reason, _ := diagnostics.Classify(validateAssignedSandbox(current, desired, false)); reason != "PoolTemplateMismatch" {
				t.Fatalf("assigned spec accepted declared difference: %s", reason)
			}
		})
	}
}

func TestPoolClaimBindingAndOwnership(t *testing.T) {
	claim := sandbox.ExtensionObject("SandboxClaim")
	claim.SetNamespace("agents")
	claim.SetName("claim")
	claim.SetUID(types.UID("claim-current"))
	if name, err := claimSandboxName(claim); name != "" || err != nil {
		t.Fatalf("pending allocation: %q, %v", name, err)
	}
	_ = unstructured.SetNestedField(claim.Object, "allocated", "status", "sandbox", "name")
	if name, err := claimSandboxName(claim); name != "allocated" || err != nil {
		t.Fatalf("binding: %q, %v", name, err)
	}
	object := sandbox.Object()
	object.SetName("allocated")
	object.SetNamespace("agents")
	isController := true
	owner := metav1.OwnerReference{APIVersion: sandbox.ExtensionsGroupVersion.String(), Kind: "SandboxClaim", Name: "claim", UID: "claim-current", Controller: &isController}
	object.SetOwnerReferences([]metav1.OwnerReference{owner})
	if err := checkClaimSandboxOwner(object, claim); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name  string
		apply func(*unstructured.Unstructured, *unstructured.Unstructured)
	}{
		{"stale UID", func(o, _ *unstructured.Unstructured) {
			refs := o.GetOwnerReferences()
			refs[0].UID = "claim-old"
			o.SetOwnerReferences(refs)
		}},
		{"wrong kind", func(o, _ *unstructured.Unstructured) {
			refs := o.GetOwnerReferences()
			refs[0].Kind = "SandboxTemplate"
			o.SetOwnerReferences(refs)
		}},
		{"wrong name", func(o, _ *unstructured.Unstructured) {
			refs := o.GetOwnerReferences()
			refs[0].Name = "other"
			o.SetOwnerReferences(refs)
		}},
		{"wrong API", func(o, _ *unstructured.Unstructured) {
			refs := o.GetOwnerReferences()
			refs[0].APIVersion = "example.io/v1"
			o.SetOwnerReferences(refs)
		}},
		{"not controller", func(o, _ *unstructured.Unstructured) {
			refs := o.GetOwnerReferences()
			value := false
			refs[0].Controller = &value
			o.SetOwnerReferences(refs)
		}},
		{"cross namespace", func(o, _ *unstructured.Unstructured) { o.SetNamespace("other") }},
		{"no UID", func(_, c *unstructured.Unstructured) { c.SetUID("") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			o, c := object.DeepCopy(), claim.DeepCopy()
			change.apply(o, c)
			if reason, _ := diagnostics.Classify(checkClaimSandboxOwner(o, c)); reason != "PoolOwnershipConflict" {
				t.Fatalf("unsafe binding accepted: %s", reason)
			}
		})
	}
	_ = unstructured.SetNestedField(claim.Object, int64(1), "status", "sandbox", "name")
	if _, err := claimSandboxName(claim); err == nil {
		t.Fatal("malformed allocation reference accepted")
	}
	_ = unstructured.SetNestedField(claim.Object, "../other", "status", "sandbox", "name")
	if _, err := claimSandboxName(claim); err == nil {
		t.Fatal("invalid Sandbox name accepted")
	}
}
