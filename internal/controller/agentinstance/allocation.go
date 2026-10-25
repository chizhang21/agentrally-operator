package agentinstance

import (
	"context"
	"fmt"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Persist selection before creating shared capacity so Group cleanup can observe
// in-flight claims and keep their pool and immutable template protected.
func (r *Reconciler) ensureAllocationPool(ctx context.Context, w *v1.AgentInstance, config executionConfiguration) error {
	pools := allocation.Manager{Client: r.directClient(), Reader: r.referenceReader(), Scheme: r.Scheme}
	selection, err := pools.Select(ctx, w, config.Configuration, config.Revision)
	if err != nil {
		return err
	}
	base := w.DeepCopy()
	if w.Status.Provisioning == nil {
		w.Status.Provisioning = &v1.AgentInstanceProvisioningStatus{}
	}
	state := w.Status.Provisioning
	state.Mode, state.PoolName, state.TemplateName, state.ConfigurationRevision = "Pool", selection.PoolName, selection.TemplateName, config.Revision
	if state.StartedAt == nil {
		now := metav1.NewTime(r.now())
		state.StartedAt = &now
	}
	if err := r.patchStatus(ctx, w, base); err != nil {
		return err
	}
	return pools.Ensure(ctx, selection, config.Configuration)
}

// Observe a completed assignment before checking its budget. Reconciliation may
// resume late; an already validated allocation is no longer waiting for capacity.
// This path deliberately reads the selected snapshot, not a new desired template.
func (r *Reconciler) reconcileAllocationDeadline(ctx context.Context, w *v1.AgentInstance) error {
	state := w.Status.Provisioning
	if state == nil {
		return nil
	}
	timedOut := func() error {
		return r.expireAllocation(ctx, w, poolDiagnostic(controller.ReasonPoolClaimTimeout,
			fmt.Sprintf("No valid Sandbox allocation was observed within %d seconds; change restartToken to retry", allocationTimeout(w))))
	}
	if state.TimedOutAt != nil {
		return timedOut()
	}
	if state.StartedAt == nil || state.CompletedAt != nil {
		return nil
	}
	claim := r.allocationClaim(w)
	err := r.directClient().Get(ctx, client.ObjectKeyFromObject(claim), claim)
	if err == nil {
		_, err = r.recordPoolClaim(ctx, w, claim)
	}
	if err == nil {
		var object *unstructured.Unstructured
		var current *sandbox.Sandbox
		object, current, err = r.allocatedSandbox(ctx, w, claim)
		if err == nil && object != nil {
			err = r.completePoolAllocation(ctx, w, current)
			if err == nil {
				return nil
			}
		}
	}
	if !r.now().Before(state.StartedAt.Add(time.Duration(allocationTimeout(w)) * time.Second)) {
		return timedOut()
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// CompletedAt describes allocation, not application startup. Only an owned
// Sandbox matching the selected configuration can end the allocation budget.
func (r *Reconciler) completePoolAllocation(ctx context.Context, w *v1.AgentInstance, current *sandbox.Sandbox) error {
	if w.Status.Provisioning.CompletedAt != nil {
		return nil
	}
	selected := sandbox.ExtensionObject("SandboxTemplate")
	name := allocation.TemplateName(w)
	if err := r.directClient().Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, selected); err != nil {
		return diagnostics.ReferenceError("SandboxTemplate", w.Namespace, name, err)
	}
	pod, found, err := sandbox.TemplatePod(selected)
	if err != nil || !found {
		return poolDiagnostic("PoolTemplateMismatch", "The selected allocation snapshot has no valid Pod template")
	}
	service, _, err := unstructured.NestedBool(selected.Object, "spec", "service")
	if err != nil {
		return poolDiagnostic("PoolTemplateMismatch", "The selected allocation snapshot has an invalid service setting")
	}
	claims, err := sandbox.TemplateVolumeClaims(selected)
	if err != nil {
		return poolDiagnostic("PoolTemplateMismatch", "The selected allocation snapshot has invalid volumeClaimTemplates")
	}
	if err := validateAssignedSandbox(current, allocation.Configuration{PodTemplate: pod, Service: service, VolumeClaimTemplates: claims}); err != nil {
		return err
	}
	group := &v1.AgentGroup{}
	if err := r.referenceReader().Get(ctx, client.ObjectKey{Name: w.Spec.AgentGroupRef.Name}, group); err != nil {
		return err
	}
	config := allocation.Configuration{PodTemplate: pod.DeepCopy(), Service: service, VolumeClaimTemplates: claims}
	config.SnapshotSources, err = allocation.SnapshotSources(selected)
	if err != nil {
		return err
	}
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == podconfig.RevisionEnv {
				config.AgentContainer = container.Name
			}
		}
	}
	podconfig.StripMarkers(&config.PodTemplate.Spec)
	if err := allocation.ValidateSnapshot(selected, group, selected.GetAnnotations()["arally.io/source-template"], w.Status.Provisioning.ConfigurationRevision, config); err != nil {
		return err
	}
	assigned := current.Spec.PodTemplate.DeepCopy()
	// Extensions adds its own blueprint metadata. Requested metadata was checked
	// above; retain exact creation markers and native Pod input for assignment.
	assigned.ObjectMeta = *pod.ObjectMeta.DeepCopy()
	if !podconfig.MatchesSnapshot(pod, assigned) {
		return poolDiagnostic("PoolTemplateMismatch", "The allocated Sandbox does not carry the selected snapshot's creation revision")
	}
	base := w.DeepCopy()
	now := metav1.NewTime(r.now())
	w.Status.Provisioning.CompletedAt = &now
	launch := current.Metadata.Labels["agents.x-k8s.io/launch-type"]
	if launch == "warm" || launch == "cold" {
		w.Status.Provisioning.LaunchType = launch
	}
	setInstanceCondition(w, controller.ConditionProvisioningReady, metav1.ConditionTrue, controller.ReasonPoolAllocated,
		"The Claim allocated an owned, matching Sandbox; Core controls application startup")
	return r.patchStatus(ctx, w, base)
}
