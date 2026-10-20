package agentclaim

import (
	"reflect"
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestBuiltinTemplateCreatesPoolWithoutSourceCR(t *testing.T) {
	for _, category := range []string{"generic", "assistant", "coding"} {
		t.Run(category, func(t *testing.T) {
			g, ns, w, _ := testutil.Fixture()
			w.Spec.TemplateRef = nil
			w.Spec.Template = category
			w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
			c, scheme := testutil.Client(t, g, ns, w)
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			state := getAgentClaim(t, c, w)
			if state.Status.Provisioning == nil || state.Status.Provisioning.TemplateName == "" {
				t.Fatalf("built-in did not select an allocation template: %+v", state.Status)
			}
			template := sandbox.ExtensionObject("SandboxTemplate")
			if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: state.Status.Provisioning.TemplateName}, template); err != nil {
				t.Fatal(err)
			}
			image, _, _ := unstructured.NestedSlice(template.Object, "spec", "podTemplate", "spec", "containers")
			if image[0].(map[string]interface{})["image"] != w.Spec.Runtime.Image || template.GetAnnotations()["arally.io/source-template"] != "builtin/"+category {
				t.Fatal("generated template did not receive the application image and built-in identity")
			}
			ownedExecutionPod(t, c, w)
			reconcileWithUpstreamSimulation(t, r, w)
			testutil.Condition(t, getAgentClaim(t, c, w).Status.Conditions, "Ready", metav1.ConditionTrue)
		})
	}
}

func TestBuiltinRuntimePreservesSecretReferencesAndWorkspace(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "assistant"
	w.Spec.Runtime = &v1.AgentRuntime{
		Image: "example.com/assistant:v2", RuntimeClassName: ptr.To("my-kata-class"),
		Command: []string{"/bin/agent"}, Args: []string{"serve"},
		Env:              []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}, secretReference("credentials", "token", false)},
		EnvFrom:          []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}}}},
		Resources:        &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull-credentials"}},
		Ports:            []corev1.ContainerPort{{Name: "http", ContainerPort: 9000}},
		ReadinessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(9000)}}},
	}
	w.Spec.Workspace = &v1.WorkspaceMount{ClaimName: "workspace", MountPath: "/agent-home"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: w.Namespace}, Data: map[string][]byte{"token": []byte("never-inline-this")}}
	pull := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-credentials", Namespace: w.Namespace}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}
	c, _ := testutil.Client(t, g, ns, w, secret, pull, pvc)
	before := w.Spec.Runtime.DeepCopy()
	pod, _, err := desiredExecution(ctx, c, w)
	if err != nil {
		t.Fatal(err)
	}
	container := pod.Spec.Containers[0]
	if container.WorkingDir != "/agent-home" {
		t.Fatal("built-in working directory must follow the persistent workspace path")
	}
	encoded, err := yaml.Marshal(pod)
	if err != nil || strings.Contains(string(encoded), "never-inline-this") {
		t.Fatal("generated template exposed credential contents")
	}
	if !reflect.DeepEqual(before, w.Spec.Runtime) || *pod.Spec.RuntimeClassName != "my-kata-class" || container.Resources.Requests.Cpu().String() != "3" || container.ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Fatal("runtime settings were lost or caller inputs mutated")
	}
	for _, env := range container.Env {
		if env.Value == "never-inline-this" || (env.Name == "LOG_LEVEL" && env.Value != "debug") || (env.Name == "HOME" && env.Value != "/agent-home") {
			t.Fatal("environment merge exposed a Secret or discarded an override")
		}
	}
	if container.EnvFrom[0].SecretRef.Name != "credentials" || pod.Spec.ImagePullSecrets[0].Name != "pull-credentials" {
		t.Fatal("Secret references were lost")
	}
	count := 0
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "arally-workspace" {
			count++
			if volume.EmptyDir != nil || volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != "workspace" {
				t.Fatal("workspace PVC did not replace the built-in ephemeral workspace")
			}
		}
	}
	if count != 1 {
		t.Fatal("workspace must have exactly one volume")
	}
	r := &Reconciler{Client: c}
	if requests := r.mapDependency(ctx, secret); len(requests) != 0 {
		t.Fatal("Secret changes must be detected by reference polling")
	}
}

func TestBuiltinDependencyFailureBlocksCapacity(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "generic"
	w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1", Env: []corev1.EnvVar{secretReference("missing-secret", "token", false)}}
	c, scheme := testutil.Client(t, g, ns, w)
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: c, Scheme: scheme}, w)
	state := getAgentClaim(t, c, w)
	assertReason(t, state, "Ready", "DependencyNotFound")
	if state.Status.Provisioning != nil {
		t.Fatal("missing credentials must block selection before prewarming")
	}
	for _, kind := range []string{"SandboxTemplate", "SandboxWarmPool", "SandboxClaim"} {
		objects := sandbox.ExtensionList(kind)
		if err := c.List(ctx, objects); err != nil || len(objects.Items) != 0 {
			t.Fatalf("missing credentials created %s capacity: %v", kind, err)
		}
	}
}

func TestBuiltinImageEditRequiresExplicitRestart(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "generic"
	w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
	c, scheme := testutil.Client(t, g, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	allocated := assignPoolSandbox(t, c, w, true)
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentClaim(t, c, w)
	originalPool := state.Status.Provisioning.PoolName
	state.Spec.Runtime.Image = "example.com/agent:v2"
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentClaim(t, c, w)
	assertReason(t, state, "ConfigurationAccepted", "PoolTemplateMismatch")
	if state.Status.Provisioning.PoolName != originalPool || state.Status.SandboxName != allocated.GetName() {
		t.Fatal("runtime image edit replaced the execution without restart")
	}
	state.Spec.RestartToken = "apply-v2"
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		reconcileWithUpstreamSimulation(t, r, w)
		state = getAgentClaim(t, c, w)
		if state.Status.Provisioning != nil && state.Status.Provisioning.PoolName != originalPool {
			break
		}
	}
	state = getAgentClaim(t, c, w)
	if state.Status.Provisioning == nil || state.Status.Provisioning.PoolName == originalPool || state.Status.LastRestartToken != "apply-v2" {
		t.Fatalf("explicit restart did not select updated image configuration: %+v", state.Status)
	}
}

func TestBuiltinImagesAndTemplatesIsolatePools(t *testing.T) {
	g, ns, w, _ := testutil.Fixture()
	w.Spec.TemplateRef, w.Spec.Template = nil, "generic"
	w.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/agent:v1"}
	c, scheme := testutil.Client(t, g, ns, w)
	m := allocation.Manager{Client: c, Scheme: scheme}
	selection := func(claim *v1.AgentClaim) string {
		pod, service, err := desiredExecution(ctx, c, claim)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := configRevision(ctx, c, w.Namespace, pod, w.Spec.AgentContainer)
		if err != nil {
			t.Fatal(err)
		}
		selected, err := m.Select(ctx, claim, service, revision)
		if err != nil {
			t.Fatal(err)
		}
		return selected.PoolName
	}
	first := selection(w)
	other := w.DeepCopy()
	other.Name = "another-agent"
	if selection(other) != first {
		t.Fatal("identical built-in settings should share the group pool")
	}
	other.Spec.Runtime.Image = "example.com/agent:v2"
	if selection(other) == first {
		t.Fatal("different images shared one pool")
	}
	other.Spec.Runtime.Image = w.Spec.Runtime.Image
	other.Spec.Template = "assistant"
	if selection(other) == first {
		t.Fatal("different environments shared one pool")
	}
}
