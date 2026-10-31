package agentinstance

import (
	"context"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/executionconfig"
	"agentrally/operator/internal/podconfig"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func desiredExecution(ctx context.Context, reader client.Reader, instance *v1.AgentInstance) (*allocation.Configuration, error) {
	revision, err := loadDefinitionRevision(ctx, reader, instance)
	if err != nil {
		return nil, err
	}
	effective := instance.DeepCopy()
	if effective.Spec.Workspace == nil {
		effective.Spec.Workspace = revision.Spec.Workspace.DeepCopy()
	}
	resolved, err := executionconfig.Resolve(ctx, reader, instance.Namespace, instance.Spec.DefinitionRef.Name, effective.Spec.Workspace, revision)
	if err != nil {
		return nil, err
	}
	index := podconfig.AgentIndex(resolved.PodTemplate.Spec.Containers, resolved.AgentContainer)
	if err := applyWorkspace(ctx, reader, effective, resolved, &resolved.PodTemplate.Spec.Containers[index]); err != nil {
		return nil, err
	}
	applyBuiltinWorkspacePermissions(effective, revision.Spec.Template != "", resolved.PodTemplate)
	if err := validateVolumeClaimTemplates(resolved.VolumeClaimTemplates, &resolved.PodTemplate.Spec); err != nil {
		return nil, err
	}
	return resolved, nil
}
