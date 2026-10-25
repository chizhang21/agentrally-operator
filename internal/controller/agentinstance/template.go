package agentinstance

import (
	"context"
	"strings"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/templates"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolvedTemplate is a snapshot of a built-in environment or a read-only native template.
type resolvedTemplate = allocation.Configuration

func resolveSandboxTemplate(ctx context.Context, reader client.Reader, instance *v1.AgentInstance) (*resolvedTemplate, error) {
	revision, err := loadDefinitionRevision(ctx, reader, instance)
	if err != nil {
		return nil, err
	}
	return resolvePublishedTemplate(instance, revision)
}

func resolvePublishedTemplate(instance *v1.AgentInstance, revision *definition.Revision) (*resolvedTemplate, error) {
	invalid := func(reason, message string) (*resolvedTemplate, error) {
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
	claims, err := sandbox.TemplateVolumeClaims(object)
	if err != nil {
		return invalid("InvalidConfiguration", "Definition volumeClaimTemplates are invalid")
	}
	service, _, err := unstructured.NestedBool(object.Object, "spec", "service")
	if err != nil {
		return invalid("InvalidConfiguration", "Definition service setting is invalid")
	}
	workspace := instance.Spec.Workspace
	if workspace == nil {
		workspace = revision.Spec.Workspace
	}
	if revision.Spec.Template != "" && revision.Spec.Template != "generic" && workspace != nil {
		templates.PrepareWorkspace(pod, workspace)
		for _, env := range revision.Spec.Container.Env {
			if env.Name == "HOME" {
				setEnv(&pod.Spec.Containers[0], env)
			}
		}
	}
	return &resolvedTemplate{PodTemplate: pod, Service: service, VolumeClaimTemplates: claims, AgentContainer: revision.Spec.AgentContainer,
		Definition: &v1.DefinitionBinding{Name: instance.Spec.DefinitionRef.Name, UID: revision.DefinitionUID, Revision: revision.ID, TemplateName: object.GetName()}}, nil
}
