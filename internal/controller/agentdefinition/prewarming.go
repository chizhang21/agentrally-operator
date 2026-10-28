package agentdefinition

import (
	"context"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/executionconfig"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) prepareCapacity(ctx context.Context, reader client.Reader, object *v1.AgentDefinition, group *v1.AgentGroup, revision *definition.Revision) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	blocked := func(reason, message string) error {
		return r.preparationStatus(ctx, object, nil, metav1.ConditionFalse, reason, message)
	}
	failed := func(err error) error {
		reason, message := diagnostics.Classify(err)
		if statusErr := blocked(reason, message); statusErr != nil {
			return statusErr
		}
		return err
	}
	policy := object.Spec.Prewarming
	if policy == nil || policy.Replicas == 0 {
		return blocked("PrewarmingDisabled", "No proactive capacity is requested")
	}
	if policy.Replicas < 0 {
		return blocked("InvalidConfiguration", "prewarming.replicas must be nonnegative")
	}
	if group.Spec.Prewarming == nil {
		return blocked("PrewarmingDisabled", "Enable AgentGroup prewarming to authorize proactive capacity")
	}
	if group.Spec.AllocationPaused {
		return blocked("AllocationPaused", "The Group has paused proactive capacity; submitted upstream work may finish")
	}
	if revision.Spec.Workspace != nil {
		return blocked("PrewarmingStorageUnsupported", "Definition workspace defaults require cold allocation; persistent PVC handoff is unavailable")
	}
	config, err := executionconfig.Resolve(ctx, reader, object.Namespace, object.Name, nil, revision)
	if err != nil {
		return failed(err)
	}
	if err := validatePreparation(*config); err != nil {
		reason, message := diagnostics.Classify(err)
		return blocked(reason, message)
	}
	validation := executionconfig.ValidateCapabilities(ctx, reader, object.Namespace, object.Generation, group, &config.PodTemplate.Spec, config.VolumeClaimTemplates)
	if validation.Status == metav1.ConditionFalse {
		return blocked(validation.Reason, validation.Message)
	}
	fingerprint, err := executionconfig.Revision(ctx, reader, object.Namespace, *config)
	if err != nil {
		return failed(err)
	}
	manager := &allocation.Manager{Client: r.Client, Reader: reader, Scheme: r.Scheme}
	selection, err := manager.SelectDefinition(ctx, object, group, *config, fingerprint)
	if err != nil {
		return failed(err)
	}
	state := &v1.DefinitionPrewarmingStatus{ObservedGeneration: object.Generation, Revision: revision.ID,
		ConfigurationRevision: fingerprint, PoolName: selection.PoolName, TemplateName: selection.TemplateName, RequestedReplicas: policy.Replicas}
	if !allocation.SamePreparationSelection(object.Status.Prewarming, state) {
		if err := r.preparationStatus(ctx, object, state, metav1.ConditionFalse, "CapacityPending", "Waiting for the Group to grant and prepare capacity"); err != nil {
			return err
		}
	}
	selection.ProtectDefinition(object)
	if err := manager.Ensure(ctx, selection, *config); err != nil {
		return failed(err)
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := reader.Get(ctx, client.ObjectKey{Namespace: object.Namespace, Name: selection.PoolName}, pool); err != nil {
		return failed(err)
	}
	if err := controller.CheckOwner(pool, group); err != nil {
		return failed(err)
	}
	poolTemplate, refErr := sandbox.PoolTemplateName(pool)
	if refErr != nil || poolTemplate != selection.TemplateName || pool.GetLabels()[allocation.ManagedLabel] != "true" ||
		pool.GetLabels()[definition.UIDLabel] != string(object.UID) || pool.GetAnnotations()["arally.io/source-template"] != "definition/"+object.Name {
		return failed(diagnostics.New("PoolTemplateMismatch", "The proactive pool no longer references the selected configuration", nil))
	}
	template := sandbox.ExtensionObject("SandboxTemplate")
	if err := reader.Get(ctx, client.ObjectKey{Namespace: object.Namespace, Name: selection.TemplateName}, template); err != nil {
		return failed(err)
	}
	if err := allocation.ValidateSnapshot(template, group, "definition/"+object.Name, fingerprint, *config); err != nil {
		return failed(err)
	}
	desired, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	observed, found, _ := unstructured.NestedInt64(pool.Object, "status", "observedGeneration")
	ready, _, _ := unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
	state.DesiredReplicas = int32(min(max(desired, 0), int64(policy.Replicas)))
	if found && observed == pool.GetGeneration() && pool.GetDeletionTimestamp() == nil {
		state.ReadyReplicas = int32(min(max(ready, 0), int64(state.DesiredReplicas)))
	}
	status, reason, message := metav1.ConditionFalse, "PoolRefilling", "Waiting for current-generation probe-checked spare capacity"
	if state.DesiredReplicas < state.RequestedReplicas {
		reason, message = "BudgetLimited", "The Group has granted fewer spares than requested"
	} else if state.ReadyReplicas >= state.RequestedReplicas {
		status, reason, message = metav1.ConditionTrue, "CapacityPrepared", "Matching, probe-checked capacity is ready for allocation"
	}
	return r.preparationStatus(ctx, object, state, status, reason, message)
}

func (r *Reconciler) preparationStatus(ctx context.Context, object *v1.AgentDefinition, state *v1.DefinitionPrewarmingStatus, status metav1.ConditionStatus, reason, message string) error {
	base := object.DeepCopy()
	object.Status.Prewarming = state
	meta.SetStatusCondition(&object.Status.Conditions, metav1.Condition{Type: "Prepared", Status: status,
		ObservedGeneration: object.Generation, Reason: reason, Message: message})
	if equality.Semantic.DeepEqual(base.Status, object.Status) {
		return nil
	}
	return r.Status().Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func validatePreparation(config allocation.Configuration) error {
	invalid := func(reason, message string) error { return diagnostics.New(reason, message, nil) }
	pod := &config.PodTemplate.Spec
	if pod.AutomountServiceAccountToken != nil && *pod.AutomountServiceAccountToken {
		return invalid("PrewarmingCredentialsUnsupported", "Unassigned capacity cannot receive Kubernetes service account tokens")
	}
	if pod.RestartPolicy != "" && pod.RestartPolicy != corev1.RestartPolicyAlways {
		return invalid("PrewarmingTaskUnsupported", "Only long-running applications can be proactively prepared")
	}
	index := podconfig.AgentIndex(pod.Containers, config.AgentContainer)
	if index < 0 || pod.Containers[index].ReadinessProbe == nil {
		return invalid("PrewarmingProbeRequired", "Configure an Agent readinessProbe before enabling proactive capacity")
	}
	if len(config.VolumeClaimTemplates) > 0 || config.Workspace != nil {
		return invalid("PrewarmingStorageUnsupported", "PVC-backed capacity requires cold allocation until safe handoff is implemented")
	}
	for _, volume := range pod.Volumes {
		switch {
		case volume.EmptyDir != nil, volume.Secret != nil, volume.ConfigMap != nil, volume.DownwardAPI != nil:
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.Secret == nil && source.ConfigMap == nil && source.DownwardAPI == nil {
					return invalid("PrewarmingCredentialsUnsupported", "Unassigned capacity only permits projected Secret, ConfigMap and downward API sources")
				}
			}
		default:
			return invalid("PrewarmingStorageUnsupported", "Unassigned capacity only permits ephemeral workspace and shared configuration volumes")
		}
	}
	return nil
}
