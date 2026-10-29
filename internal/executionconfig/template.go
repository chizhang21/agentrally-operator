package executionconfig

import (
	"strings"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/templates"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ResolveTemplate(name string, workspace *v1.WorkspaceMount, revision *definition.Revision) (*allocation.Configuration, error) {
	invalid := func(reason, message string) (*allocation.Configuration, error) {
		return nil, diagnostics.New(reason, message, nil)
	}
	object := revision.Template
	pod, found, err := sandbox.TemplatePod(object)
	if err != nil || !found || len(pod.Spec.Containers) == 0 {
		return invalid("InvalidConfiguration", "Published definition requires a valid native Pod template")
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "" || strings.TrimSpace(container.Image) == "" {
			return invalid("InvalidConfiguration", "Definition containers require name and image")
		}
	}
	index := podconfig.AgentIndex(pod.Spec.Containers, revision.Spec.AgentContainer)
	if index < 0 {
		return invalid("InvalidConfiguration", "The accepted revision's Agent container is missing")
	}
	claims, err := sandbox.TemplateVolumeClaims(object)
	if err != nil {
		return invalid("InvalidConfiguration", "Definition volumeClaimTemplates are invalid")
	}
	service, _, err := unstructured.NestedBool(object.Object, "spec", "service")
	if err != nil {
		return invalid("InvalidConfiguration", "Definition service setting is invalid")
	}
	if workspace == nil {
		workspace = revision.Spec.Workspace
	}
	if revision.Spec.Template != "" && revision.Spec.Template != "generic" && workspace != nil {
		templates.PrepareWorkspace(pod, workspace)
		for _, env := range revision.Spec.Container.Env {
			if env.Name == "HOME" {
				podconfig.SetEnv(&pod.Spec.Containers[0], env)
			}
		}
	}
	return &allocation.Configuration{PodTemplate: pod, Service: service, VolumeClaimTemplates: claims, AgentContainer: revision.Spec.AgentContainer,
		Definition: &v1.DefinitionBinding{Name: name, UID: revision.DefinitionUID, Revision: revision.ID, TemplateName: object.GetName(),
			AgentContainer: pod.Spec.Containers[index].Name, InvocationProtocol: revision.Spec.InvocationProtocol}}, nil
}
