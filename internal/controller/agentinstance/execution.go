package agentinstance

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/executionconfig"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type executionConfiguration struct {
	allocation.Configuration
	Revision string
	Fallback bool
	Issue    *metav1.Condition
}

// Preparation owns writes and persists identity before fingerprinting or
// allocation. Resolution itself only reads and renders configuration.
func (r *Reconciler) prepareExecutionConfiguration(ctx context.Context, instance *v1.AgentInstance, group *v1.AgentGroup, mode string) (executionConfiguration, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	config, err := r.resolveExecutionConfiguration(ctx, instance, mode)
	if err != nil || config.Fallback {
		return config, err
	}
	if err := r.bindDefinition(ctx, instance, config.Definition); err != nil {
		return executionConfiguration{}, err
	}
	if err := r.bindWorkspace(ctx, instance, config.Workspace); err != nil {
		return executionConfiguration{}, err
	}
	if err := r.retainAllocatedStorage(ctx, instance, &config); err != nil {
		return executionConfiguration{}, err
	}
	config.SnapshotSources, err = r.resolveSnapshotSources(ctx, instance, group, config.VolumeClaimTemplates)
	if err != nil {
		return executionConfiguration{}, err
	}
	config.Revision, err = executionconfig.Revision(ctx, r.referenceReader(), instance.Namespace, config.Configuration)
	return config, err
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

// Retry only an unallocated attempt; allocated executions update in place.
func (r *Reconciler) reconcileAllocationRetry(ctx context.Context, w *v1.AgentInstance) (bool, ctrl.Result, error) {
	// Allocated instances restart in place after configuration and identity checks.
	if w.Status.SandboxUID != "" || (w.Status.Provisioning != nil && w.Status.Provisioning.CompletedAt != nil) {
		return false, ctrl.Result{}, nil
	}
	phase := meta.FindStatusCondition(w.Status.Conditions, controller.ConditionRestarting)
	if w.Spec.RestartToken != w.Status.LastRestartToken && w.Status.Provisioning != nil && w.Status.Provisioning.TimedOutAt == nil {
		object, err := r.existingExecution(ctx, w)
		if err == nil {
			current, decodeErr := sandbox.Decode(object)
			if decodeErr != nil {
				return true, ctrl.Result{}, decodeErr
			}
			if err := r.completePoolAllocation(ctx, w, current); err != nil {
				return true, ctrl.Result{}, err
			}
			return false, ctrl.Result{}, nil
		}
		if reason, _ := diagnostics.Classify(err); !apierrors.IsNotFound(err) && reason != controller.ReasonPoolPending {
			return true, ctrl.Result{}, err
		}
	}
	if w.Spec.RestartToken != w.Status.LastRestartToken {
		base := w.DeepCopy()
		w.Status.LastRestartToken = w.Spec.RestartToken
		setInstanceCondition(w, controller.ConditionReady, metav1.ConditionFalse, controller.ReasonRestarting, "Retrying the unallocated attempt")
		setInstanceCondition(w, controller.ConditionAvailable, metav1.ConditionUnknown, controller.ReasonRestarting, "Waiting for allocation retry cleanup")
		setInstanceCondition(w, controller.ConditionConfigurationApplied, metav1.ConditionUnknown, controller.ReasonRestarting, "Waiting for a new allocation attempt")
		if hasInteraction(w) {
			setInstanceCondition(w, controller.ConditionInteractionReady, metav1.ConditionFalse, controller.ReasonRestarting, "The interaction endpoint is unavailable before allocation")
		}
		setInstanceCondition(w, controller.ConditionRestarting, metav1.ConditionTrue, controller.ReasonRestarting, "Accepted allocation retry; waiting for unfinished attempt cleanup")
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
	w.Status.Timings = nil
	if w.Status.Definition != nil && w.Spec.Revision != "" && w.Spec.Revision != w.Status.Definition.Revision {
		w.Status.Definition.Revision, w.Status.Definition.TemplateName = "", ""
		w.Status.Definition.AgentContainer, w.Status.Definition.InvocationProtocol = "", ""
	}
	w.Status.SandboxName = ""
	w.Status.SandboxUID = ""
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
		controller.ConditionSuspended, controller.ConditionInteractionReady,
		controller.ConditionCleanupReady,
	} {
		meta.RemoveStatusCondition(&w.Status.Conditions, kind)
	}
	setInstanceCondition(w, controller.ConditionRestarting, metav1.ConditionFalse, controller.ReasonRestartCompleted, "Owned execution cleanup completed; the next execution can be provisioned")
	return true, ctrl.Result{RequeueAfter: time.Millisecond}, r.patchStatus(ctx, w, base)
}

func (r *Reconciler) cleanupExecution(ctx context.Context, w *v1.AgentInstance) (bool, error) {
	if w.Status.Provisioning != nil || w.Status.SandboxName != "" {
		base := w.DeepCopy()
		timings := instanceTimings(w)
		if timings.CleanupStartedAt == nil {
			now := metav1.NewTime(r.now())
			timings.CleanupStartedAt = &now
		}
		if err := r.patchStatus(ctx, w, base); err != nil {
			return false, err
		}
	}
	// Request execution shutdown before waiting for optional route finalizers.
	claim := r.allocationClaim(w)
	done, err := controller.DeleteOwned(ctx, r.directClient(), claim, w)
	if meta.IsNoMatchError(err) && (w.Status.Provisioning == nil || w.Status.Provisioning.ClaimName == "") {
		done, err = true, nil
	}
	if hasInteraction(w) {
		interactionDone, interactionErr := r.cleanupInteraction(ctx, w, "")
		done, err = done && interactionDone, errors.Join(err, interactionErr)
	}
	base := w.DeepCopy()
	condition := metav1.ConditionFalse
	var reason, message string
	if err != nil {
		reason, message = "CleanupFailed", diagnostics.SafeError(err).Error()
	} else if done {
		condition, reason, message = metav1.ConditionTrue, "CleanupCompleted", "Owned execution and access resources have been removed"
	} else {
		reason, message = r.cleanupDiagnostic(ctx, w)
	}
	setInstanceCondition(w, controller.ConditionCleanupReady, condition, reason, message)
	if done && err == nil && w.Status.Timings != nil && w.Status.Timings.CleanupStartedAt != nil && w.Status.Timings.CleanupCompletedAt == nil {
		now := metav1.NewTime(r.now())
		w.Status.Timings.CleanupCompletedAt = &now
	}
	return done, errors.Join(err, r.patchStatus(ctx, w, base))
}

func (r *Reconciler) cleanupDiagnostic(ctx context.Context, w *v1.AgentInstance) (string, string) {
	object, err := r.existingExecution(ctx, w)
	if err == nil {
		pod, podErr := r.executionPod(ctx, object)
		if podErr != nil {
			return "CleanupObservationBlocked", "Cannot verify execution Pod cleanup; inspect ownership and read permissions"
		}
		if pod != nil && pod.DeletionTimestamp != nil {
			diagnostic := diagnostics.Termination(pod, r.now())
			return diagnostic.Reason, diagnostic.Message
		}
	}
	if timings := w.Status.Timings; timings != nil && timings.CleanupStartedAt != nil && r.now().Sub(timings.CleanupStartedAt.Time) >= executionStallThreshold {
		return "ExecutionCleanupStalled", "Foreground cleanup has not completed for at least 5 minutes; inspect upstream controllers, dependents and finalizers"
	}
	return "CleanupPending", "Waiting for owned execution and access resources to finish foreground cleanup"
}

func (r *Reconciler) allocationClaim(w *v1.AgentInstance) *unstructured.Unstructured {
	claim := desiredPoolClaim(w)
	if w.Status.Provisioning != nil && w.Status.Provisioning.ClaimName != "" {
		claim = sandbox.ExtensionObject("SandboxClaim")
		claim.SetName(w.Status.Provisioning.ClaimName)
		claim.SetNamespace(w.Namespace)
	}
	return claim
}

func (r *Reconciler) existingExecution(ctx context.Context, w *v1.AgentInstance) (*unstructured.Unstructured, error) {
	claim := r.allocationClaim(w)
	if err := r.directClient().Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		return nil, err
	}
	if err := controller.CheckOwner(claim, w); err != nil {
		return nil, err
	}
	if err := checkClaimIdentity(w, claim); err != nil {
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
	if err := r.directClient().Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return nil, err
	}
	if err := checkClaimSandboxOwner(object, claim); err != nil {
		return nil, err
	}
	return object, checkSandboxIdentity(w, object)
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
