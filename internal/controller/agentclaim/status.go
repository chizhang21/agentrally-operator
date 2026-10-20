package agentclaim

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// setClaimCondition stamps conditions owned by this controller. Mirrored Core
// conditions keep their separately checked generation and transition timestamp.
func setClaimCondition(claim *v1.AgentClaim, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type: kind, Status: status, ObservedGeneration: claim.Generation,
		Reason: reason, Message: message,
	})
}

func (r *Reconciler) pending(ctx context.Context, workload *v1.AgentClaim, reason, message string) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.blockedStatus(ctx, workload, reason, message)
}

func (r *Reconciler) failed(ctx context.Context, workload *v1.AgentClaim, err error) (ctrl.Result, error) {
	err = diagnostics.SafeError(err)
	if statusErr := r.blockedStatus(ctx, workload, "ReconcileError", err.Error()); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

func (r *Reconciler) blockedStatus(ctx context.Context, workload *v1.AgentClaim, reason, message string) error {
	return r.blockedStatusWithValidation(ctx, workload, reason, message, nil)
}

func (r *Reconciler) blockedStatusWithValidation(ctx context.Context, workload *v1.AgentClaim, reason, message string, validation *metav1.Condition) error {
	base := workload.DeepCopy()
	for _, kind := range []string{controller.ConditionPodScheduled, controller.ConditionFinished, controller.ConditionSuspended} {
		meta.RemoveStatusCondition(&workload.Status.Conditions, kind)
	}
	if validation != nil {
		meta.SetStatusCondition(&workload.Status.Conditions, *validation)
	} else {
		meta.RemoveStatusCondition(&workload.Status.Conditions, controller.ConditionCapabilityValidated)
	}
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.ServiceName, workload.Status.ServiceFQDN = "", ""
	workload.Status.Interaction = nil
	r.observeBlockedExecution(ctx, workload)
	setClaimCondition(workload, controller.ConditionConfigurationApplied, metav1.ConditionUnknown, "WorkloadBlocked", "Desired execution configuration cannot currently be verified")
	if hasInteraction(workload) {
		setClaimCondition(workload, controller.ConditionInteractionReady, metav1.ConditionFalse, "WorkloadBlocked", message)
	}
	for _, kind := range []string{controller.ConditionConfigurationAccepted, controller.ConditionReady} {
		setClaimCondition(workload, kind, metav1.ConditionFalse, reason, message)
	}
	return r.patchStatus(ctx, workload, base)
}

func (r *Reconciler) status(
	ctx context.Context, workload *v1.AgentClaim, object *unstructured.Unstructured, current *sandbox.Sandbox,
	interaction interactionState, validation metav1.Condition, config executionConfiguration,
) error {
	generation := object.GetGeneration()
	base := workload.DeepCopy()
	meta.SetStatusCondition(&workload.Status.Conditions, validation)
	pod, podErr := r.executionPod(ctx, object)
	mirrorCoreConditions(workload, current, generation, pod, podErr)
	workload.Status.ObservedGeneration = workload.Generation
	workload.Status.SandboxName, workload.Status.ServiceName, workload.Status.ServiceFQDN = current.Metadata.Name, current.Status.Service, current.Status.ServiceFQDN
	workload.Status.Interaction = interaction.status
	acceptedConfigurationStatus(workload, config)
	appliedConfigurationStatus(workload, config, pod, podErr)
	decision, err := evaluateExecutionLifecycle(workload, pod, r.now())
	if err != nil {
		return err
	}
	workload.Status.Lifecycle = decision.Status
	available := r.executionAvailableStatus(ctx, workload, current, generation, pod, podErr)
	interactionReadyStatus(workload, interaction, available)
	readyStatus(workload, available)
	return r.patchStatus(ctx, workload, base)
}

func mirrorCoreConditions(workload *v1.AgentClaim, current *sandbox.Sandbox, generation int64, pod *corev1.Pod, podErr error) {
	for _, kind := range []string{controller.ConditionPodScheduled, controller.ConditionFinished, controller.ConditionSuspended} {
		condition := currentCoreCondition(current, kind, generation, workload.Generation)
		switch kind {
		case controller.ConditionFinished:
			condition = executionFinishedCondition(workload, pod, condition)
		case controller.ConditionPodScheduled:
			if observed := executionScheduledCondition(pod, workload.Generation); observed != nil {
				condition = observed
			}
		case controller.ConditionSuspended:
			if workload.Spec.OperatingMode != sandbox.Suspended {
				condition = nil
			} else if podErr != nil {
				condition = &metav1.Condition{
					Type: kind, Status: metav1.ConditionUnknown, ObservedGeneration: workload.Generation,
					Reason:  "PodObservationBlocked",
					Message: "Cannot verify execution Pod removal; check Pod ownership and operator read permissions",
				}
			} else if pod != nil {
				condition = &metav1.Condition{
					Type: kind, Status: metav1.ConditionFalse, ObservedGeneration: workload.Generation,
					Reason:  "PodNotTerminated",
					Message: "The execution Pod is still present; waiting for Core to finish suspension",
				}
			}
		}
		if condition == nil {
			meta.RemoveStatusCondition(&workload.Status.Conditions, kind)
		} else {
			meta.SetStatusCondition(&workload.Status.Conditions, *condition)
		}
	}
}

func currentCoreCondition(current *sandbox.Sandbox, kind string, generation, targetGeneration int64) *metav1.Condition {
	source := meta.FindStatusCondition(current.Status.Conditions, kind)
	if source == nil || source.ObservedGeneration != generation {
		return nil
	}
	condition := *source
	condition.ObservedGeneration = targetGeneration
	if condition.Reason == "" {
		condition.Reason = "SandboxObserved"
	}
	return &condition
}

func acceptedConfigurationStatus(workload *v1.AgentClaim, config executionConfiguration) {
	configuration := metav1.Condition{Type: controller.ConditionConfigurationAccepted, Status: metav1.ConditionTrue,
		ObservedGeneration: workload.Generation, Reason: "PoolConfigurationAccepted", Message: "The assigned Sandbox configuration matches the AgentClaim; Extensions manages allocation"}
	if config.Fallback {
		configuration.Status = metav1.ConditionFalse
		configuration.Reason = "SuspendedUsingExistingConfiguration"
		configuration.Message = "Suspension submitted using existing PodTemplate; desired model/workspace configuration could not be resolved"
	}
	if config.Issue != nil {
		configuration = *config.Issue
		configuration.ObservedGeneration = workload.Generation
	}
	meta.SetStatusCondition(&workload.Status.Conditions, configuration)
	workload.Status.DesiredRevision = config.Revision
}

func appliedConfigurationStatus(workload *v1.AgentClaim, config executionConfiguration, pod *corev1.Pod, podErr error) {
	observation := observeConfiguration(config.Template, pod, config.AgentContainer, config.Revision)
	observation.Condition.ObservedGeneration = workload.Generation
	if podErr != nil {
		observation.Condition.Status = metav1.ConditionUnknown
		observation.Condition.Reason = "PodObservationBlocked"
		observation.Condition.Message = "Cannot verify the current execution Pod; check Pod ownership and operator read permissions"
	}
	if pod != nil && pod.DeletionTimestamp != nil {
		observation.AppliedRevision = ""
		observation.Condition.Status = metav1.ConditionUnknown
		observation.Condition.Reason = "PodTerminating"
		observation.Condition.Message = "The execution Pod is being removed; waiting for a live Pod to verify configuration"
	}
	if config.Fallback {
		observation.Condition.Status = metav1.ConditionUnknown
		observation.Condition.Reason = "SuspendedUsingExistingConfiguration"
		observation.Condition.Message = "Desired configuration could not be resolved; suspension uses the existing execution configuration"
	}
	workload.Status.AppliedRevision = observation.AppliedRevision
	workload.Status.PodName, workload.Status.PodUID = "", ""
	if pod != nil {
		workload.Status.PodName, workload.Status.PodUID = pod.Name, string(pod.UID)
	}
	meta.SetStatusCondition(&workload.Status.Conditions, observation.Condition)
}

func (r *Reconciler) executionAvailableStatus(ctx context.Context, workload *v1.AgentClaim, current *sandbox.Sandbox, generation int64, pod *corev1.Pod, podErr error) metav1.Condition {
	ready := metav1.Condition{Type: controller.ConditionReady, Status: metav1.ConditionUnknown, ObservedGeneration: workload.Generation,
		Reason: "SandboxPending", Message: "Waiting for a current Core Sandbox Ready condition"}
	if source := currentCoreCondition(current, controller.ConditionReady, generation, workload.Generation); source != nil {
		ready = *source
	}
	suspended := meta.FindStatusCondition(workload.Status.Conditions, controller.ConditionSuspended)
	ready = r.executionReadiness(ctx, workload, ready, pod, podErr, suspended != nil && suspended.Status == metav1.ConditionTrue)
	ready.Type = controller.ConditionAvailable
	meta.SetStatusCondition(&workload.Status.Conditions, ready)
	return ready
}

// A blocked desired configuration does not erase health observations of an
// existing execution. Observe through the same UID checks used by normal status.
func (r *Reconciler) observeBlockedExecution(ctx context.Context, workload *v1.AgentClaim) {
	object, err := r.existingExecution(ctx, workload)
	if err != nil {
		setClaimCondition(workload, controller.ConditionAvailable, metav1.ConditionUnknown,
			"ExecutionObservationBlocked", "Cannot verify a current owned execution")
		return
	}
	current, err := sandbox.Decode(object)
	if err != nil {
		setClaimCondition(workload, controller.ConditionAvailable, metav1.ConditionUnknown,
			"ExecutionObservationBlocked", "Cannot decode the current owned execution")
		return
	}
	pod, podErr := r.executionPod(ctx, object)
	mirrorCoreConditions(workload, current, object.GetGeneration(), pod, podErr)
	r.executionAvailableStatus(ctx, workload, current, object.GetGeneration(), pod, podErr)
}

// Ready describes convergence of the current declaration. Availability and route
// health remain independent so a pending update does not misreport a live Agent.
func readyStatus(workload *v1.AgentClaim, available metav1.Condition) {
	ready := available
	ready.Type, ready.LastTransitionTime = controller.ConditionReady, metav1.Time{}
	if available.Status != metav1.ConditionFalse {
		conditions := []string{
			controller.ConditionConfigurationApplied, controller.ConditionConfigurationAccepted,
			controller.ConditionProvisioningReady, controller.ConditionCapabilityValidated,
		}
		if hasInteraction(workload) {
			conditions = append(conditions, controller.ConditionInteractionReady)
		}
		for _, kind := range conditions {
			condition := meta.FindStatusCondition(workload.Status.Conditions, kind)
			// Discovery confidence is advisory; confirmed validation failures are not.
			if condition != nil && condition.ObservedGeneration == workload.Generation &&
				condition.Status == metav1.ConditionUnknown && kind == controller.ConditionCapabilityValidated {
				continue
			}
			if condition == nil || condition.ObservedGeneration != workload.Generation {
				if ready.Status == metav1.ConditionTrue {
					ready.Status, ready.Reason, ready.Message = metav1.ConditionUnknown, "ConfigurationUnverified", "Waiting for current configuration observations"
				}
				continue
			}
			if condition.Status == metav1.ConditionFalse {
				ready.Status, ready.Reason, ready.Message = condition.Status, condition.Reason, condition.Message
				break
			}
			if condition.Status == metav1.ConditionUnknown && ready.Status == metav1.ConditionTrue {
				ready.Status, ready.Reason, ready.Message = condition.Status, condition.Reason, condition.Message
			}
		}
	}
	meta.SetStatusCondition(&workload.Status.Conditions, ready)
}

func interactionReadyStatus(workload *v1.AgentClaim, interaction interactionState, ready metav1.Condition) {
	if interaction.condition == nil {
		return
	}
	condition := *interaction.condition
	if condition.Status == metav1.ConditionTrue && ready.Status != metav1.ConditionTrue {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "ExecutionNotReady", ready.Message
	}
	meta.SetStatusCondition(&workload.Status.Conditions, condition)
}

func (r *Reconciler) patchStatus(ctx context.Context, workload, base *v1.AgentClaim) error {
	if equality.Semantic.DeepEqual(base.Status, workload.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, workload, client.MergeFrom(base)); err != nil {
		return err
	}
	controller.RecordConditionChanges(r.Recorder, workload, base.Status.Conditions, workload.Status.Conditions)
	return nil
}
