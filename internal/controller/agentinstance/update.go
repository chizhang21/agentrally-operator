package agentinstance

import (
	"context"
	"reflect"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func updateRequested(w *v1.AgentInstance, config executionConfiguration) bool {
	return w.Spec.RestartToken != w.Status.LastRestartToken ||
		(config.Definition != nil && w.Status.Definition != nil && config.Definition.Revision != w.Status.Definition.Revision)
}

func checkClaimIdentity(w *v1.AgentInstance, claim *unstructured.Unstructured) error {
	if state := w.Status.Provisioning; state != nil && state.ClaimUID != "" && state.ClaimUID != claim.GetUID() {
		return diagnostics.New("PoolAllocationLost", "The allocated Claim was replaced; refusing a new allocation for an existing instance", nil)
	}
	return nil
}

func checkSandboxIdentity(w *v1.AgentInstance, object *unstructured.Unstructured) error {
	if w.Status.SandboxUID != "" && w.Status.SandboxUID != object.GetUID() {
		return diagnostics.New("PoolAllocationLost", "The allocated Sandbox was replaced; refusing to treat a new execution as the original allocation", nil)
	}
	if object.GetDeletionTimestamp() != nil {
		return diagnostics.New("DependencyTerminating", "The allocated Sandbox is terminating", nil)
	}
	return nil
}

func allowedClaimPool(w *v1.AgentInstance, name string) bool {
	return name == configuredPool(w) || (w.Status.Update != nil && w.Status.Update.Phase == "Stopping" && name == w.Status.Update.Configuration.PoolName)
}

func configuredPool(w *v1.AgentInstance) string {
	if w.Status.Configuration != nil {
		return w.Status.Configuration.PoolName
	}
	return allocation.PoolName(w)
}

func (r *Reconciler) setClaimPool(ctx context.Context, claim *unstructured.Unstructured, name string) error {
	base := claim.DeepCopy()
	if err := unstructured.SetNestedField(claim.Object, name, "spec", "warmPoolRef", "name"); err != nil {
		return err
	}
	return r.directClient().Patch(ctx, claim, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func updateCondition(reason, message string) *metav1.Condition {
	return &metav1.Condition{Type: controller.ConditionConfigurationAccepted, Status: metav1.ConditionTrue, Reason: reason, Message: message}
}

// The allocated Claim remains the owner. Its pool reference supplies immutable
// metadata to Extensions; only AgentRally changes the stopped Sandbox's PodSpec.
func (r *Reconciler) reconcileAllocatedUpdate(ctx context.Context, w *v1.AgentInstance, object *unstructured.Unstructured, current *sandbox.Sandbox, config executionConfiguration, mode string) (*metav1.Condition, error) {
	if err := r.observeVolumeIdentities(ctx, w, object, current, updateRequested(w, config) || w.Status.Update != nil); err != nil {
		return nil, err
	}
	if !config.Fallback && (updateRequested(w, config) || (w.Status.Update != nil && w.Status.Update.Phase == "Stopping")) {
		pools := allocation.Manager{Client: r.directClient(), Reader: r.referenceReader(), Scheme: r.Scheme}
		selectionInput := w.DeepCopy()
		// A named pool selects initial capacity; an allocated Sandbox is never
		// checked out again. Application updates use managed immutable snapshots.
		selectionInput.Spec.Provisioning, selectionInput.Status.Provisioning = nil, nil
		selected, err := pools.Select(ctx, selectionInput, config.Configuration, config.Revision)
		if err != nil {
			return nil, err
		}
		target := v1.AgentInstanceConfiguration{PoolName: selected.PoolName, TemplateName: selected.TemplateName, Revision: config.Revision}
		if pending := w.Status.Update; pending == nil || pending.Configuration != target || pending.RestartToken != w.Spec.RestartToken {
			if pending != nil && pending.Phase == "Stopping" {
				// Keep the previous intent until its metadata handoff is undone.
				// Otherwise a Claim already on that pool would lose its durable
				// identity reference when the pending target is superseded.
				claim := r.allocationClaim(w)
				if err := r.directClient().Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					return nil, err
				}
				if _, err := r.recordPoolClaim(ctx, w, claim); err != nil {
					return nil, err
				}
				pool, err := sandbox.ClaimPoolName(claim)
				if err != nil {
					return nil, err
				}
				if pool != configuredPool(w) {
					return updateCondition("Updating", "Resetting the previous metadata handoff before accepting the latest target"), r.setClaimPool(ctx, claim, configuredPool(w))
				}
			}
			base := w.DeepCopy()
			w.Status.Update = &v1.AgentInstanceUpdate{Configuration: target, RestartToken: w.Spec.RestartToken, Phase: "Stopping", StartedAt: metav1.NewTime(r.now())}
			setInstanceCondition(w, controller.ConditionRestarting, metav1.ConditionTrue, controller.ReasonRestarting, "Accepted update; preserving Claim, Sandbox and persistent volumes")
			if err := r.patchStatus(ctx, w, base); err != nil {
				return nil, err
			}
		}
		if err := pools.Ensure(ctx, selected, config.Configuration); err != nil {
			return nil, err
		}
	}
	pending := w.Status.Update
	if pending != nil && pending.Phase == "Stopping" {
		if err := r.updateClaimedMode(ctx, object, sandbox.Suspended); err != nil {
			return nil, err
		}
		pod, err := r.executionPod(ctx, object)
		if err != nil {
			return nil, err
		}
		if pod != nil {
			reason, message := "Updating", "Waiting for the old Pod to terminate before applying the update"
			if pod.DeletionTimestamp != nil {
				issue := diagnostics.Termination(pod, r.now())
				reason, message = issue.Reason, issue.Message
			}
			return updateCondition(reason, message), nil
		}
		suspended := meta.FindStatusCondition(current.Status.Conditions, controller.ConditionSuspended)
		if suspended == nil || suspended.Status != metav1.ConditionTrue || suspended.ObservedGeneration != object.GetGeneration() {
			return updateCondition("Updating", "Waiting for Core to confirm suspension of the current Sandbox generation"), nil
		}
		if config.Fallback {
			return updateCondition("UpdateBlocked", "Execution is stopped; waiting for a valid target configuration"), nil
		}
		claim := r.allocationClaim(w)
		if err := r.directClient().Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
			return nil, err
		}
		if _, err := r.recordPoolClaim(ctx, w, claim); err != nil {
			return nil, err
		}
		pool, err := sandbox.ClaimPoolName(claim)
		if err != nil {
			return nil, err
		}
		if pool != pending.Configuration.PoolName {
			if err := r.setClaimPool(ctx, claim, pending.Configuration.PoolName); err != nil {
				return nil, err
			}
			return updateCondition("Updating", "Waiting for Extensions to observe the new configuration pool"), nil
		}
		if !reflect.DeepEqual(userPoolMetadata(config.PodTemplate.Labels), userPoolMetadata(current.Spec.PodTemplate.Labels)) ||
			!reflect.DeepEqual(userPoolMetadata(config.PodTemplate.Annotations), userPoolMetadata(current.Spec.PodTemplate.Annotations)) {
			return updateCondition("Updating", "Waiting for Extensions to synchronize the target template metadata"), nil
		}
		template := config.PodTemplate.DeepCopy()
		if err := podconfig.Stamp(template, config.AgentContainer, config.Revision); err != nil {
			return nil, err
		}
		fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&template.Spec)
		if err != nil {
			return nil, err
		}
		base := object.DeepCopy()
		if err := unstructured.SetNestedMap(object.Object, fields, "spec", "podTemplate", "spec"); err != nil {
			return nil, err
		}
		if err := unstructured.SetNestedField(object.Object, config.Service, "spec", "service"); err != nil {
			return nil, err
		}
		if err := r.directClient().Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
		// Keep the Sandbox suspended until this durable phase is visible.
		before := w.DeepCopy()
		w.Status.Configuration = &pending.Configuration
		w.Status.Definition = config.Definition.DeepCopy()
		w.Status.LastRestartToken = pending.RestartToken
		w.Status.Update.Phase = "Starting"
		w.Status.Lifecycle = nil
		w.Status.AppliedRevision, w.Status.PodName, w.Status.PodUID = "", "", ""
		if err := r.patchStatus(ctx, w, before); err != nil {
			return nil, err
		}
		current.Spec.PodTemplate.Spec = template.Spec
		current.Spec.Service = config.Service
		return updateCondition("Updating", "Target configuration saved; waiting to reconcile the requested operating mode"), nil
	}
	if err := r.updateClaimedMode(ctx, object, mode); err != nil {
		return nil, err
	}
	if mismatch := validateAssignedSandbox(current, config.Configuration); mismatch != nil && !config.Fallback {
		reason, message := diagnostics.Classify(mismatch)
		return &metav1.Condition{Type: controller.ConditionConfigurationAccepted, Status: metav1.ConditionFalse, Reason: reason, Message: message + "; change restartToken to apply current dependency values"}, nil
	}
	return nil, nil
}

// A written template is not a running-version acknowledgement. Suspended updates
// only acknowledge preparation; resume still has to observe the new Pod.
func completeExecutionUpdate(w *v1.AgentInstance, pod *corev1.Pod, available metav1.Condition) {
	pending := w.Status.Update
	if pending == nil || pending.Phase != "Starting" {
		return
	}
	current := func(kind string) bool {
		c := meta.FindStatusCondition(w.Status.Conditions, kind)
		return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == w.Generation
	}
	stopped := executionMode(w) == sandbox.Suspended && pod == nil && current(controller.ConditionSuspended)
	running := current(controller.ConditionConfigurationApplied) && (available.Status == metav1.ConditionTrue || current(controller.ConditionFinished))
	if !current(controller.ConditionConfigurationAccepted) || (!stopped && !running) {
		return
	}
	w.Status.Update = nil
	message := "The target execution configuration is observed"
	if stopped {
		message = "The target configuration is saved; the instance remains suspended until explicitly resumed"
	}
	setInstanceCondition(w, controller.ConditionRestarting, metav1.ConditionFalse, controller.ReasonRestartCompleted, message)
}
