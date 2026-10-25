package agentinstance

import (
	"reflect"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func TestBuiltinRuntimePreservesSecretReferencesAndWorkspace(t *testing.T) {
	g, ns, definition, w, _ := testutil.Fixture()
	definition.Spec.TemplateRef, definition.Spec.Template = nil, "assistant"
	definition.Spec.Container = &v1.AgentContainerSpec{
		Image: "example.com/assistant:v2", RuntimeClassName: ptr.To("my-kata-class"),
		Command: []string{"/bin/agent"}, Args: []string{"serve"},
		Env:              []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}, secretReference("credentials", "token", false)},
		EnvFrom:          []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}}}},
		Resources:        &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull-credentials"}},
		Ports:            []corev1.ContainerPort{{Name: "http", ContainerPort: 9000}},
		ReadinessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(9000)}}},
	}
	w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: ptr.To(workspaceVolumeSpec()), MountPath: "/agent-home"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: w.Namespace}, Data: map[string][]byte{"token": []byte("never-inline-this")}}
	pull := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-credentials", Namespace: w.Namespace}}
	c, _ := clientWithPublishedDefinitions(t, definition, g, ns, w, secret, pull)
	before := definition.Spec.Container.DeepCopy()
	resolved, err := desiredExecution(ctx, c, w)
	if err != nil {
		t.Fatal(err)
	}
	pod := resolved.PodTemplate
	container := pod.Spec.Containers[0]
	if container.WorkingDir != "/agent-home" {
		t.Fatal("built-in working directory must follow the persistent workspace path")
	}
	encoded, err := yaml.Marshal(pod)
	if err != nil || strings.Contains(string(encoded), "never-inline-this") {
		t.Fatal("generated template exposed credential contents")
	}
	if !reflect.DeepEqual(before, definition.Spec.Container) || *pod.Spec.RuntimeClassName != "my-kata-class" || container.Resources.Requests.Cpu().String() != "3" || container.ReadinessProbe.HTTPGet.Path != "/ready" {
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
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "agentrally-workspace" {
			t.Fatal("workspace must be provisioned per Sandbox instead of pre-bound in the Pod template")
		}
	}
	if len(resolved.VolumeClaimTemplates) != 1 || resolved.VolumeClaimTemplates[0].Name != "agentrally-workspace" {
		t.Fatal("workspace must have exactly one PVC template")
	}
	r := &Reconciler{Client: c}
	if requests := r.mapDependency(ctx, secret); len(requests) != 0 {
		t.Fatal("Secret changes must be detected by reference polling")
	}
}

func TestBuiltinDependencyFailureBlocksCapacity(t *testing.T) {
	g, ns, definition, w, _ := testutil.Fixture()
	definition.Spec.TemplateRef, definition.Spec.Template = nil, "generic"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example.com/agent:v1", Env: []corev1.EnvVar{secretReference("missing-secret", "token", false)}}
	c, scheme := clientWithPublishedDefinitions(t, definition, g, ns, w)
	reconcileWithUpstreamSimulation(t, &Reconciler{Client: c, Scheme: scheme}, w)
	state := getAgentInstance(t, c, w)
	assertReason(t, state, "Ready", "DependencyNotFound")
	if state.Status.Provisioning != nil {
		t.Fatal("missing credentials must block selection before prewarming")
	}
	for _, kind := range []string{"SandboxWarmPool", "SandboxClaim"} {
		objects := sandbox.ExtensionList(kind)
		if err := c.List(ctx, objects); err != nil || len(objects.Items) != 0 {
			t.Fatalf("missing credentials created %s capacity: %v", kind, err)
		}
	}
}

func TestBuiltinImagesAndTemplatesIsolatePools(t *testing.T) {
	g, ns, definition, w, _ := testutil.Fixture()
	definition.Spec.TemplateRef, definition.Spec.Template = nil, "generic"
	definition.Spec.Container = &v1.AgentContainerSpec{Image: "example.com/agent:v1"}
	c, scheme := clientWithPublishedDefinitions(t, definition, g, ns, w)
	m := allocation.Manager{Client: c, Scheme: scheme}
	selection := func(claim *v1.AgentInstance) string {
		pod, service, err := desiredExecutionForTest(ctx, c, claim)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := configRevision(ctx, c, w.Namespace, allocation.Configuration{PodTemplate: pod, AgentContainer: definition.Spec.AgentContainer})
		if err != nil {
			t.Fatal(err)
		}
		selected, err := m.Select(ctx, claim, allocation.Configuration{PodTemplate: pod, Service: service, AgentContainer: definition.Spec.AgentContainer}, revision)
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
	definition.Spec.Container.Image = "example.com/agent:v2"
	saveDefinition(t, c, definition)
	testutil.PublishDefinitions(t, c, scheme)
	if selection(other) == first {
		t.Fatal("different images shared one pool")
	}
	definition.Spec.Container.Image = "example.com/agent:v1"
	definition.Spec.Template = "assistant"
	saveDefinition(t, c, definition)
	testutil.PublishDefinitions(t, c, scheme)
	if selection(other) == first {
		t.Fatal("different environments shared one pool")
	}
}
