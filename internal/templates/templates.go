// Package templates contains the built-in Agent environments shipped in the
// Operator binary. These blueprints are instantiated before upstream allocation;
// they are never installed as incomplete SandboxTemplate CRs.
package templates

import (
	"embed"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

//go:embed generic.yaml assistant.yaml coding.yaml
var environments embed.FS

// Resolve returns a fresh native Pod template with the user's application settings.
// Workspace optionally replaces the built-in ephemeral workspace with an existing PVC.
func Resolve(name string, settings *v1.AgentRuntime, workspace *v1.WorkspaceMount) (*corev1.PodTemplateSpec, error) {
	if name != "generic" && name != "assistant" && name != "coding" {
		return nil, invalid("template must be generic, assistant or coding")
	}
	if settings == nil || strings.TrimSpace(settings.Image) == "" || strings.ContainsAny(settings.Image, " \t\r\n") {
		return nil, invalid("built-in templates require runtime.image with a valid nonempty OCI image reference")
	}
	data, err := environments.ReadFile(name + ".yaml")
	if err != nil {
		return nil, err
	}
	pod := &corev1.PodTemplateSpec{}
	if err := yaml.UnmarshalStrict(data, pod); err != nil {
		return nil, err
	}
	if workspace != nil && name != "generic" {
		removeEphemeralWorkspace(pod)
		pod.Spec.Containers[0].WorkingDir = workspace.MountPath
		replaceEnv(&pod.Spec.Containers[0], corev1.EnvVar{Name: "HOME", Value: workspace.MountPath})
	}
	applyRuntime(pod, settings)
	return pod, nil
}

func applyRuntime(pod *corev1.PodTemplateSpec, settings *v1.AgentRuntime) {
	container := &pod.Spec.Containers[0]
	container.Image = settings.Image
	if settings.ImagePullPolicy != "" {
		container.ImagePullPolicy = settings.ImagePullPolicy
	}
	container.Command = append([]string(nil), settings.Command...)
	container.Args = append([]string(nil), settings.Args...)
	for _, env := range settings.Env {
		replaceEnv(container, *env.DeepCopy())
	}
	for _, source := range settings.EnvFrom {
		container.EnvFrom = append(container.EnvFrom, *source.DeepCopy())
	}
	if settings.Resources != nil {
		container.Resources = *settings.Resources.DeepCopy()
	}
	container.Ports = append([]corev1.ContainerPort(nil), settings.Ports...)
	container.ReadinessProbe = settings.ReadinessProbe.DeepCopy()
	container.StartupProbe = settings.StartupProbe.DeepCopy()
	container.LivenessProbe = settings.LivenessProbe.DeepCopy()
	if settings.RuntimeClassName != nil {
		name := *settings.RuntimeClassName
		pod.Spec.RuntimeClassName = &name
	}
	pod.Spec.ImagePullSecrets = append([]corev1.LocalObjectReference(nil), settings.ImagePullSecrets...)
}

func replaceEnv(container *corev1.Container, value corev1.EnvVar) {
	for i := range container.Env {
		if container.Env[i].Name == value.Name {
			container.Env[i] = value
			return
		}
	}
	container.Env = append(container.Env, value)
}

func removeEphemeralWorkspace(pod *corev1.PodTemplateSpec) {
	volumes := pod.Spec.Volumes[:0]
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "arally-workspace" {
			volumes = append(volumes, volume)
		}
	}
	pod.Spec.Volumes = volumes
	container := &pod.Spec.Containers[0]
	mounts := container.VolumeMounts[:0]
	for _, mount := range container.VolumeMounts {
		if mount.Name != "arally-workspace" {
			mounts = append(mounts, mount)
		}
	}
	container.VolumeMounts = mounts
}

func invalid(message string) error {
	return diagnostics.New("InvalidConfiguration", message, nil)
}
