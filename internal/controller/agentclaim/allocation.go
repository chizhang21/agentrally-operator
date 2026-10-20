package agentclaim

import (
	"context"
	"fmt"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Persist selection before creating shared capacity so Group cleanup can observe
// in-flight claims and keep their pool and immutable template protected.
func (r *Reconciler) ensureAllocationPool(ctx context.Context, w *v1.AgentClaim, config executionConfiguration) error {
	pools := allocation.Manager{Client: r.interactionClient(), Reader: r.referenceReader(), Scheme: r.Scheme}
	selection, err := pools.Select(ctx, w, config.Service, config.Revision)
	if err != nil {
		return err
	}
	base := w.DeepCopy()
	if w.Status.Provisioning == nil {
		w.Status.Provisioning = &v1.AgentClaimProvisioningStatus{}
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
	return pools.Ensure(ctx, selection, config.Template, config.VolumeClaimTemplates, config.AgentContainer)
}

// Observe a completed assignment before checking its budget. Reconciliation may
// resume late; an already validated allocation is no longer waiting for capacity.
// This path deliberately reads the selected snapshot, not a new desired template.
func (r *Reconciler) reconcileAllocationDeadline(ctx context.Context, w *v1.AgentClaim) error {
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
	err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(claim), claim)
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
func (r *Reconciler) completePoolAllocation(ctx context.Context, w *v1.AgentClaim, current *sandbox.Sandbox) error {
	if w.Status.Provisioning.CompletedAt != nil {
		return nil
	}
	selected := sandbox.ExtensionObject("SandboxTemplate")
	name := allocation.TemplateName(w)
	if err := r.interactionClient().Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, selected); err != nil {
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
	if err := validateAssignedSandbox(current, pod, service, claims); err != nil {
		return err
	}
	base := w.DeepCopy()
	now := metav1.NewTime(r.now())
	w.Status.Provisioning.CompletedAt = &now
	setClaimCondition(w, controller.ConditionProvisioningReady, metav1.ConditionTrue, controller.ReasonPoolAllocated,
		"The Claim allocated an owned, matching Sandbox; Core controls application startup")
	return r.patchStatus(ctx, w, base)
}
