package agentclaim

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type executionConfiguration struct {
	Template             *corev1.PodTemplateSpec
	VolumeClaimTemplates []corev1.PersistentVolumeClaimTemplate
	Service              bool
	AgentContainer       string
	Revision             string
	Fallback             bool
	Issue                *metav1.Condition
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Reconciler) referenceReader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// Persist intent before any deletion, and finish cleanup before accepting a new execution.
func (r *Reconciler) reconcileRestart(ctx context.Context, w *v1.AgentClaim) (bool, ctrl.Result, error) {
	phase := meta.FindStatusCondition(w.Status.Conditions, controller.ConditionRestarting)
	if w.Spec.RestartToken != w.Status.LastRestartToken {
		base := w.DeepCopy()
		w.Status.LastRestartToken = w.Spec.RestartToken
		setClaimCondition(w, controller.ConditionReady, metav1.ConditionFalse, controller.ReasonRestarting, "Explicit restart is replacing the execution")
		setClaimCondition(w, controller.ConditionAvailable, metav1.ConditionUnknown, controller.ReasonRestarting, "Execution availability is being rechecked during replacement")
		setClaimCondition(w, controller.ConditionConfigurationApplied, metav1.ConditionUnknown, controller.ReasonRestarting, "Waiting for the replacement execution")
		if hasInteraction(w) {
			setClaimCondition(w, controller.ConditionInteractionReady, metav1.ConditionFalse, controller.ReasonRestarting, "The interaction endpoint is unavailable during replacement")
		}
		setClaimCondition(w, controller.ConditionRestarting, metav1.ConditionTrue, controller.ReasonRestarting, "Accepted explicit restart; waiting for owned execution cleanup")
		if err := r.patchStatus(ctx, w, base); err != nil {
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if phase == nil || phase.Status != metav1.ConditionTrue {
		return false, ctrl.Result{}, nil
	}
	done, err := r.cleanupExecution(ctx, w)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !done {
		return true, ctrl.Result{RequeueAfter: time.Second}, nil
	}
	base := w.DeepCopy()
	w.Status.Lifecycle = nil
	w.Status.Provisioning = nil
	w.Status.SandboxName = ""
	w.Status.VolumeClaims = nil
	w.Status.ServiceName = ""
	w.Status.ServiceFQDN = ""
	w.Status.Interaction = nil
	w.Status.PodName = ""
	w.Status.PodUID = ""
	w.Status.AppliedRevision = ""
	for _, kind := range []string{
		controller.ConditionReady, controller.ConditionAvailable, controller.ConditionConfigurationAccepted, controller.ConditionConfigurationApplied,
		controller.ConditionProvisioningReady, controller.ConditionPodScheduled, controller.ConditionFinished,
		controller.ConditionSuspended, controller.ConditionExpired, controller.ConditionInteractionReady,
	} {
		meta.RemoveStatusCondition(&w.Status.Conditions, kind)
	}
	setClaimCondition(w, controller.ConditionRestarting, metav1.ConditionFalse, controller.ReasonRestartCompleted, "Owned execution cleanup completed; the next execution can be provisioned")
	return true, ctrl.Result{RequeueAfter: time.Millisecond}, r.patchStatus(ctx, w, base)
}

func (r *Reconciler) cleanupExecution(ctx context.Context, w *v1.AgentClaim) (bool, error) {
	// Request execution shutdown before waiting for optional route finalizers.
	claim := r.allocationClaim(w)
	done, err := controller.DeleteOwned(ctx, r.interactionClient(), claim, w)
	if meta.IsNoMatchError(err) && (w.Status.Provisioning == nil || w.Status.Provisioning.ClaimName == "") {
		done, err = true, nil
	}
	if hasInteraction(w) {
		interactionDone, interactionErr := r.cleanupInteraction(ctx, w, "")
		return done && interactionDone, errors.Join(err, interactionErr)
	}
	return done, err
}

func (r *Reconciler) reconcileExpiry(ctx context.Context, w *v1.AgentClaim) (bool, ctrl.Result, error) {
	now := r.now()
	decision, err := evaluateExecutionLifecycle(w, nil, now)
	finished := meta.FindStatusCondition(w.Status.Conditions, controller.ConditionFinished)
	unconfirmed := finished != nil && finished.Status == metav1.ConditionTrue &&
		(w.Status.Lifecycle == nil || w.Status.Lifecycle.FinishedAt == nil)
	completionExpired := decision.Expired && decision.Status.Reason == "FinishedTTLReached"
	absoluteExpired := false
	if policy := w.Spec.Lifecycle; policy != nil && policy.ShutdownTime != nil && !policy.ShutdownTime.IsZero() {
		absoluteExpired = !now.Before(retentionDeadline(policy.ShutdownTime.Time, 0))
	}
	if err == nil && !absoluteExpired && (unconfirmed || completionExpired) && (w.Status.Lifecycle == nil || w.Status.Lifecycle.ExpiredAt == nil) {
		// Observe completion even when configuration is blocked, and recheck
		// before TTL cleanup in case a resumed Pod invalidated the old anchor.
		var pod *corev1.Pod
		object, observeErr := r.existingExecution(ctx, w)
		if observeErr == nil {
			pod, observeErr = r.executionPod(ctx, object)
		}
		if observeErr != nil && !apierrors.IsNotFound(observeErr) {
			return true, ctrl.Result{}, observeErr
		}
		decision, err = evaluateExecutionLifecycle(w, pod, now)
	}
	if err != nil {
		result, statusErr := r.pending(ctx, w, "InvalidConfiguration", err.Error())
		return true, result, statusErr
	}
	base := w.DeepCopy()
	w.Status.Lifecycle = decision.Status
	if !decision.Expired {
		return false, ctrl.Result{}, r.patchStatus(ctx, w, base)
	}
	setClaimCondition(w, controller.ConditionExpired, metav1.ConditionTrue, decision.Status.Reason, "Execution lifetime ended; owned execution resources are being removed")
	setClaimCondition(w, controller.ConditionReady, metav1.ConditionFalse, controller.ReasonExpired, "Execution lifetime ended")
	setClaimCondition(w, controller.ConditionAvailable, metav1.ConditionUnknown, controller.ReasonExpired, "Execution cleanup is in progress")
	if hasInteraction(w) {
		setClaimCondition(w, controller.ConditionInteractionReady, metav1.ConditionFalse, controller.ReasonExpired, "Execution lifetime ended")
	}

	if err := r.patchStatus(ctx, w, base); err != nil {
		return true, ctrl.Result{}, err
	}
	done, err := r.cleanupExecution(ctx, w)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if !done {
		return true, ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if w.Spec.Lifecycle != nil && w.Spec.Lifecycle.ShutdownPolicy == "Delete" {
		err := r.Delete(ctx, w, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(w.UID)}})
		return true, ctrl.Result{RequeueAfter: time.Second}, client.IgnoreNotFound(err)
	}
	base = w.DeepCopy()
	w.Status.ServiceName = ""
	w.Status.ServiceFQDN = ""
	w.Status.Interaction = nil
	w.Status.PodName = ""
	w.Status.PodUID = ""
	w.Status.AppliedRevision = ""
	setClaimCondition(w, controller.ConditionAvailable, metav1.ConditionFalse, controller.ReasonExpired, "Owned execution cleanup completed")
	w.Status.VolumeClaims = nil
	for _, kind := range []string{controller.ConditionReady, controller.ConditionConfigurationAccepted, controller.ConditionConfigurationApplied} {
		setClaimCondition(w, kind, metav1.ConditionFalse, controller.ReasonExpired, "Execution expired; change restartToken and use a future deadline to start another execution")
	}
	for _, kind := range []string{controller.ConditionPodScheduled, controller.ConditionSuspended, controller.ConditionInteractionReady, controller.ConditionProvisioningReady} {
		meta.RemoveStatusCondition(&w.Status.Conditions, kind)
	}
	w.Status.ObservedGeneration = w.Generation
	return true, ctrl.Result{}, r.patchStatus(ctx, w, base)
}

func (r *Reconciler) allocationClaim(w *v1.AgentClaim) *unstructured.Unstructured {
	claim := desiredPoolClaim(w)
	if w.Status.Provisioning != nil && w.Status.Provisioning.ClaimName != "" {
		claim = sandbox.ExtensionObject("SandboxClaim")
		claim.SetName(w.Status.Provisioning.ClaimName)
		claim.SetNamespace(w.Namespace)
	}
	return claim
}

func (r *Reconciler) existingExecution(ctx context.Context, w *v1.AgentClaim) (*unstructured.Unstructured, error) {
	claim := r.allocationClaim(w)
	if err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		return nil, err
	}
	if err := controller.CheckOwner(claim, w); err != nil {
		return nil, err
	}
	name, err := claimSandboxName(claim)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, poolDiagnostic(controller.ReasonPoolPending, "Waiting for a Sandbox allocation")
	}
	object := sandbox.Object()
	object.SetName(name)
	object.SetNamespace(w.Namespace)
	if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return nil, err
	}
	return object, checkClaimSandboxOwner(object, claim)
}

// Core's annotation also handles warm Pod names that differ from Sandbox names.
func (r *Reconciler) executionPod(ctx context.Context, object *unstructured.Unstructured) (*corev1.Pod, error) {
	name := object.GetAnnotations()["agents.x-k8s.io/pod-name"]
	if name == "" {
		name = object.GetName()
	}
	pod := &corev1.Pod{}
	if err := r.referenceReader().Get(ctx, client.ObjectKey{Namespace: object.GetNamespace(), Name: name}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	ref := metav1.GetControllerOf(pod)
	if object.GetUID() == "" || ref == nil || ref.UID != object.GetUID() || ref.Name != object.GetName() || ref.Kind != "Sandbox" || ref.APIVersion != sandbox.GroupVersion.String() {
		return nil, fmt.Errorf("execution Pod does not belong to the current Sandbox")
	}
	return pod, nil
}
