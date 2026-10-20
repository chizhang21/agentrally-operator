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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) provisioningCondition(ctx context.Context, w *v1.AgentClaim, status metav1.ConditionStatus, reason, message string) error {
	base := w.DeepCopy()
	setClaimCondition(w, controller.ConditionProvisioningReady, status, reason, message)
	return r.patchStatus(ctx, w, base)
}

// recordPoolFailure records diagnostics only; cancellation is explicit at callers.
func (r *Reconciler) recordPoolFailure(ctx context.Context, w *v1.AgentClaim, cause error) error {
	reason, message := diagnostics.Classify(cause)
	if err := r.provisioningCondition(ctx, w, metav1.ConditionFalse, reason, message); err != nil {
		return err
	}
	return cause
}

// expireAllocation persists the terminal attempt before requesting execution cleanup.
func (r *Reconciler) expireAllocation(ctx context.Context, w *v1.AgentClaim, cause error) error {
	base := w.DeepCopy()
	if w.Status.Provisioning == nil {
		w.Status.Provisioning = &v1.AgentClaimProvisioningStatus{Mode: "Pool"}
	}
	if w.Status.Provisioning.TimedOutAt == nil {
		now := metav1.NewTime(r.now())
		w.Status.Provisioning.TimedOutAt = &now
	}
	if err := r.patchStatus(ctx, w, base); err != nil {
		return err
	}
	// Stop upstream allocation as well as the caller's wait. Keep the failed attempt latched.
	if _, err := r.cleanupExecution(ctx, w); err != nil {
		return err
	}
	return r.recordPoolFailure(ctx, w, cause)
}

func (r *Reconciler) poolPreflight(ctx context.Context, w *v1.AgentClaim, config executionConfiguration) error {
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName(allocation.PoolName(w))
	pool.SetNamespace(w.Namespace)
	if err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
		return diagnostics.New("PoolUnavailable", "Cannot read the selected warm pool; install Extensions and check the namespaced pool", err)
	}
	name, err := sandbox.PoolTemplateName(pool)
	managed := pool.GetLabels()[allocation.ManagedLabel] == "true"
	if (managed && pool.GetAnnotations()["arally.io/source-template"] != w.Spec.TemplateSource()) || (!managed && name != w.Spec.TemplateSource()) {
		return poolDiagnostic("PoolTemplateMismatch", "The pool was selected for a different source template; restart the allocation")
	}

	if err != nil || name == "" || name != allocation.TemplateName(w) {
		return poolDiagnostic("PoolTemplateMismatch", "The selected warm pool template differs from the recorded allocation template")
	}
	nativeTemplate := sandbox.ExtensionObject("SandboxTemplate")
	nativeTemplate.SetName(name)
	nativeTemplate.SetNamespace(w.Namespace)
	if err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(nativeTemplate), nativeTemplate); err != nil {
		return diagnostics.New("PoolUnavailable", "Cannot read the warm pool's SandboxTemplate; create it in the same namespace", err)
	}
	return validatePoolTemplate(pool, nativeTemplate, config.Template, config.Service)
}

func (r *Reconciler) provisionFromPool(ctx context.Context, w *v1.AgentClaim, config executionConfiguration, mode string) (*unstructured.Unstructured, *metav1.Condition, error) {
	if err := r.allocationAttemptError(w, mode); err != nil {
		if reason, _ := diagnostics.Classify(err); reason == controller.ReasonPoolClaimTimeout {
			return nil, nil, r.expireAllocation(ctx, w, err)
		}
		return nil, nil, r.recordPoolFailure(ctx, w, err)
	}
	claim := r.allocationClaim(w)
	err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(claim), claim)
	if err != nil && !apierrors.IsNotFound(err) {
		if meta.IsNoMatchError(err) {
			return nil, nil, r.recordPoolFailure(ctx, w, diagnostics.New("PoolUnavailable", "SIGs Extensions APIs are unavailable; install Extensions", err))
		}
		return nil, nil, diagnostics.New("DependencyReadFailed", "Cannot read the SandboxClaim; check API connectivity and operator permissions", err)
	}
	if apierrors.IsNotFound(err) {
		claim, err = r.createPoolClaim(ctx, w, config, mode)
		if err != nil {
			return nil, nil, err
		}
	}
	deadline, err := r.recordPoolClaim(ctx, w, claim)
	if err != nil {
		return nil, nil, err
	}
	object, current, err := r.allocatedSandbox(ctx, w, claim)
	if err != nil {
		return nil, nil, err
	}
	if object != nil {
		issue, err := r.observePoolAllocation(ctx, w, object, current, config, mode)
		if err != nil {
			return nil, nil, err
		}
		return object, issue, nil
	}
	if w.Status.Provisioning.CompletedAt != nil {
		return nil, nil, poolDiagnostic("PoolAllocationLost", "The assigned Sandbox is unavailable; waiting for upstream recovery or an explicit restart")
	}
	if !r.now().Before(deadline) {
		return nil, nil, r.expireAllocation(ctx, w, poolDiagnostic(controller.ReasonPoolClaimTimeout, fmt.Sprintf("No valid Sandbox allocation was observed within %d seconds; change restartToken to retry", allocationTimeout(w))))
	}
	if err := r.poolPreflight(ctx, w, config); err != nil {
		return nil, nil, r.recordPoolFailure(ctx, w, err)
	}
	if err := r.provisioningCondition(ctx, w, metav1.ConditionUnknown, controller.ReasonPoolPending, "Waiting for a valid Sandbox allocation"); err != nil {
		return nil, nil, err
	}
	return nil, nil, poolDiagnostic(controller.ReasonPoolPending, "Waiting for a valid Sandbox allocation")
}

func (r *Reconciler) allocationAttemptError(w *v1.AgentClaim, mode string) error {
	if w.Status.Provisioning != nil && w.Status.Provisioning.TimedOutAt != nil {
		return poolDiagnostic(controller.ReasonPoolClaimTimeout, "The allocation attempt timed out and its Claim is being removed; change restartToken to retry")
	}
	if state := w.Status.Provisioning; state != nil && state.CompletedAt == nil && state.ConfigurationRevision != "" && state.ConfigurationRevision != w.Status.DesiredRevision && mode != sandbox.Suspended {
		if state.StartedAt != nil && !r.now().Before(state.StartedAt.Add(time.Duration(allocationTimeout(w))*time.Second)) {
			return poolDiagnostic(controller.ReasonPoolClaimTimeout, "Allocation did not settle within its budget; change restartToken to retry")
		}
		return poolDiagnostic("PoolTemplateMismatch", "Configuration changed during allocation; change restartToken to use the new configuration")
	}
	return nil
}

// The allocation budget is persisted before creation, so API failures cannot reset it.
func (r *Reconciler) createPoolClaim(ctx context.Context, w *v1.AgentClaim, config executionConfiguration, mode string) (*unstructured.Unstructured, error) {
	if state := w.Status.Provisioning; state != nil && state.StartedAt != nil && state.ClaimName == "" {
		seconds := allocationTimeout(w)
		if !r.now().Before(state.StartedAt.Add(time.Duration(seconds) * time.Second)) {
			return nil, r.expireAllocation(ctx, w, poolDiagnostic(controller.ReasonPoolClaimTimeout, "Claim creation did not complete within its allocation budget; change restartToken to retry"))
		}
	}
	if mode == sandbox.Suspended {
		return nil, poolDiagnostic(controller.ReasonAllocationSuspended, "Allocation is paused until operatingMode is Running")
	}
	if w.Status.Provisioning != nil && w.Status.Provisioning.ClaimName != "" {
		return nil, r.recordPoolFailure(ctx, w, poolDiagnostic("PoolAllocationLost", "The recorded Claim disappeared; change restartToken to request another allocation"))
	}
	if err := r.ensureAllocationPool(ctx, w, config); err != nil {
		return nil, r.recordPoolFailure(ctx, w, err)
	}
	if err := r.poolPreflight(ctx, w, config); err != nil {
		return nil, r.recordPoolFailure(ctx, w, err)
	}
	base := w.DeepCopy()
	now := metav1.NewTime(r.now())
	if w.Status.Provisioning == nil {
		w.Status.Provisioning = &v1.AgentClaimProvisioningStatus{StartedAt: &now, Mode: "Pool"}
	} else {
		w.Status.Provisioning.Mode = "Pool"
		if w.Status.Provisioning.StartedAt == nil {
			w.Status.Provisioning.StartedAt = &now
		}
	}
	if err := r.patchStatus(ctx, w, base); err != nil {
		return nil, err
	}
	claim := r.allocationClaim(w)
	if err := ctrl.SetControllerReference(w, claim, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.interactionClient().Create(ctx, claim); err != nil {
		// Recover a creation failure without resetting the budget or claiming success.
		return nil, diagnostics.New("DependencyReadFailed", "Cannot create the SandboxClaim; check API connectivity and permissions", err)
	}
	return claim, nil
}

func (r *Reconciler) recordPoolClaim(ctx context.Context, w *v1.AgentClaim, claim *unstructured.Unstructured) (time.Time, error) {
	if err := controller.CheckOwner(claim, w); err != nil {
		return time.Time{}, diagnostics.New("PoolOwnershipConflict", "The SandboxClaim is controlled by another workload; refusing adoption", err)
	}
	ref, err := sandbox.ClaimPoolName(claim)
	if err != nil || ref != allocation.PoolName(w) {
		return time.Time{}, poolDiagnostic("PoolOwnershipConflict", "The owned Claim references a different warm pool; refusing to rewrite it")
	}
	if claim.GetDeletionTimestamp() != nil {
		return time.Time{}, poolDiagnostic(controller.ReasonPoolPending, "Waiting for the SandboxClaim to finish deletion")
	}
	base := w.DeepCopy()
	if w.Status.Provisioning == nil {
		w.Status.Provisioning = &v1.AgentClaimProvisioningStatus{}
	}
	status := w.Status.Provisioning
	status.Mode = "Pool"
	status.ClaimName = claim.GetName()
	created := claim.GetCreationTimestamp()
	if status.StartedAt == nil && !created.IsZero() {
		status.StartedAt = &created
	} else if status.StartedAt == nil {
		now := metav1.NewTime(r.now())
		status.StartedAt = &now
	}
	if err := r.patchStatus(ctx, w, base); err != nil {
		return time.Time{}, err
	}
	timeout := allocationTimeout(w)
	if timeout < 1 || timeout > 3600 {
		return time.Time{}, poolDiagnostic("InvalidConfiguration", "claimTimeoutSeconds must be between 1 and 3600")
	}
	return status.StartedAt.Add(time.Duration(timeout) * time.Second), nil
}

func (r *Reconciler) allocatedSandbox(ctx context.Context, w *v1.AgentClaim, claim *unstructured.Unstructured) (*unstructured.Unstructured, *sandbox.Sandbox, error) {
	name, err := claimSandboxName(claim)
	if err != nil {
		return nil, nil, err
	}
	var object *unstructured.Unstructured
	if name != "" {
		object = sandbox.Object()
		object.SetName(name)
		object.SetNamespace(w.Namespace)
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, nil, err
			}
			object = nil
		}
	}
	if object != nil {
		if err := checkClaimSandboxOwner(object, claim); err != nil {
			return nil, nil, err
		}
		current, err := sandbox.Decode(object)
		if err != nil {
			return nil, nil, err
		}
		return object, current, nil
	}
	return nil, nil, nil
}

func (r *Reconciler) observePoolAllocation(
	ctx context.Context, w *v1.AgentClaim, object *unstructured.Unstructured, current *sandbox.Sandbox,
	config executionConfiguration, mode string,
) (*metav1.Condition, error) {
	if err := r.completePoolAllocation(ctx, w, current); err != nil {
		return nil, r.recordPoolFailure(ctx, w, err)
	}
	// Existing execution keeps its creation configuration until explicit restart.
	// Requested mode changes apply even when the new desired template differs.
	if err := r.updateClaimedMode(ctx, object, mode); err != nil {
		return nil, err
	}
	mismatch := validateAssignedSandbox(current, config.Template, config.Service)
	if mismatch == nil && mode != sandbox.Suspended {
		mismatch = r.poolPreflight(ctx, w, config)
	}
	var issue *metav1.Condition
	if mismatch != nil {
		reason, message := diagnostics.Classify(mismatch)
		issue = &metav1.Condition{
			Type: controller.ConditionConfigurationAccepted, Status: metav1.ConditionFalse,
			Reason: reason, Message: message + "; change restartToken to replace this execution",
		}
	}
	if err := r.provisioningCondition(ctx, w, metav1.ConditionTrue, controller.ReasonPoolAllocated,
		"The Claim allocated an owned Sandbox; Core controls application startup"); err != nil {
		return nil, err
	}
	return issue, nil
}

func (r *Reconciler) updateClaimedMode(ctx context.Context, object *unstructured.Unstructured, mode string) error {
	current, _, err := unstructured.NestedString(object.Object, "spec", "operatingMode")
	if err != nil {
		return err
	}
	if current == mode {
		return nil
	}
	base := object.DeepCopy()
	if err := unstructured.SetNestedField(object.Object, mode, "spec", "operatingMode"); err != nil {
		return err
	}
	return r.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
