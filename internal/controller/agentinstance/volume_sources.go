package agentinstance

import (
	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/executionconfig"
	"agentrally/operator/internal/sandbox"
	"context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Only a completed execution may retain its initialization sources. Read them
// from the recorded Group-owned snapshot, not a new source or an external pool.
func (r *Reconciler) resolveSnapshotSources(ctx context.Context, workload *v1.AgentInstance, group *v1.AgentGroup, claims []corev1.PersistentVolumeClaimTemplate) (map[string]types.UID, error) {
	var retained map[string]types.UID
	needsSnapshot := false
	for _, claim := range claims {
		name, err := executionconfig.VolumeSnapshotSource(claim.Spec, workload.Namespace)
		if err != nil {
			return nil, err
		}
		needsSnapshot = needsSnapshot || name != ""
	}
	if state := workload.Status.Provisioning; needsSnapshot && state != nil && state.CompletedAt != nil {
		template := sandbox.ExtensionObject("SandboxTemplate")
		if err := r.referenceReader().Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: state.TemplateName}, template); err != nil {
			return nil, diagnostics.ReferenceError("SandboxTemplate", workload.Namespace, state.TemplateName, err)
		}
		if err := controller.CheckOwner(template, group); err != nil {
			return nil, err
		}
		if template.GetAnnotations()["arally.io/source-template"] == workload.Spec.TemplateSource() && template.GetAnnotations()["arally.io/configuration-revision"] == state.ConfigurationRevision {
			var err error
			retained, err = allocation.SnapshotSources(template)
			if err != nil {
				return nil, err
			}
		}
	}
	return executionconfig.SnapshotReferences(ctx, r.referenceReader(), workload.Namespace, claims, retained)
}
