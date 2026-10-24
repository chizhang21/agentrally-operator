package agentinstance

import (
	"context"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/metrics"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AgentInstanceFinalizer keeps allocation cleanup ahead of Claim removal.
const AgentInstanceFinalizer = "arally.io/agent-instance-cleanup"

// Reconciler handles AgentInstance configuration and allocation.
type Reconciler struct {
	client.Client
	// Reader obtains current native references and capability evidence.
	Reader   client.Reader
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// InteractionClient bypasses the cache for optional routing APIs.
	InteractionClient client.Client
	// Now makes deadline planning deterministic in controller tests.
	Now     func() time.Time
	Metrics *metrics.Observer
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	defer func() { resultErr = diagnostics.SafeError(resultErr) }()
	workload := &v1.AgentInstance{}
	if err := r.Get(ctx, req.NamespacedName, workload); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() { result = r.withExecutionDeadlines(workload, result) }()
	if workload.DeletionTimestamp != nil {
		return r.finalize(ctx, workload)
	}
	if err := controller.AddFinalizer(ctx, r.Client, workload, AgentInstanceFinalizer); err != nil {
		return ctrl.Result{}, err
	}
	if handled, result, err := r.reconcileRestart(ctx, workload); handled || err != nil {
		return result, err
	}
	if err := r.observeActivity(ctx, workload); err != nil {
		return ctrl.Result{}, err
	}
	if handled, result, err := r.reconcileExpiry(ctx, workload); handled || err != nil {
		return result, err
	}
	// Stopping an owned execution and enforcing an in-flight allocation budget
	// must not depend on whether a new desired configuration can be resolved.
	if executionMode(workload) == sandbox.Suspended {
		object, err := r.existingExecution(ctx, workload)
		if err == nil {
			if err := r.suspensionRequest(ctx, workload, true); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.updateClaimedMode(ctx, object, sandbox.Suspended); err != nil {
				return r.failed(ctx, workload, err)
			}
		} else if reason, _ := diagnostics.Classify(err); !apierrors.IsNotFound(err) && reason != controller.ReasonPoolPending {
			return r.pendingOrFailed(ctx, workload, err)
		}
	} else if err := r.suspensionRequest(ctx, workload, false); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileAllocationDeadline(ctx, workload); err != nil {
		return r.pendingOrFailed(ctx, workload, err)
	}
	if workload.Spec.Interaction == nil {
		if _, err := r.reconcileInteraction(ctx, workload, nil, 0); err != nil {
			return r.failed(ctx, workload, err)
		}
	}
	group, waiting, err := r.readyGroup(ctx, workload)
	if err != nil {
		if waiting {
			reason, message := diagnostics.Classify(err)
			return r.pending(ctx, workload, reason, message)
		}
		return r.failed(ctx, workload, err)
	}
	return r.reconcileExecution(ctx, workload, group)
}

// withExecutionDeadlines bounds all retry paths, including paths that exit before allocation.
func (r *Reconciler) withExecutionDeadlines(workload *v1.AgentInstance, result ctrl.Result) ctrl.Result {
	if activity := workload.Status.Activity; activity != nil && workload.DeletionTimestamp == nil && meta.IsStatusConditionTrue(workload.Status.Conditions, controller.ConditionActivityReady) {
		for _, deadline := range []*metav1.MicroTime{activity.ValidUntil, activity.IdleDeadline} {
			if deadline == nil || !deadline.After(r.now()) {
				continue
			}
			delay := deadline.Sub(r.now())
			if result.RequeueAfter == 0 || delay < result.RequeueAfter {
				result.RequeueAfter = delay
			}
		}
	}
	if lifecycle := workload.Status.Lifecycle; lifecycle != nil && lifecycle.ExpiredAt == nil && lifecycle.ExpiresAt != nil {
		delay := lifecycle.ExpiresAt.Sub(r.now())
		if delay <= 0 {
			delay = time.Millisecond
		}
		if result.RequeueAfter == 0 || delay < result.RequeueAfter {
			result.RequeueAfter = delay
		}
	}
	if state := workload.Status.Provisioning; workload.DeletionTimestamp == nil && state != nil && state.CompletedAt == nil && state.TimedOutAt == nil && state.StartedAt != nil {
		seconds := allocationTimeout(workload)
		delay := state.StartedAt.Add(time.Duration(seconds) * time.Second).Sub(r.now())
		if delay <= 0 {
			delay = time.Millisecond
		}
		if result.RequeueAfter == 0 || delay < result.RequeueAfter {
			result.RequeueAfter = delay
		}
	}
	return result
}

// The waiting flag distinguishes expected group readiness from API/ownership failures.
func (r *Reconciler) readyGroup(ctx context.Context, workload *v1.AgentInstance) (*v1.AgentGroup, bool, error) {
	group := &v1.AgentGroup{}
	if err := r.Get(ctx, client.ObjectKey{Name: workload.Spec.AgentGroupRef.Name}, group); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, diagnostics.New("GroupNotFound", "Referenced AgentGroup does not exist", nil)
		}
		return nil, false, err
	}
	if group.NamespaceName() != workload.Namespace {
		return nil, true, diagnostics.New("GroupNamespaceMismatch", "Workload must be in its AgentGroup's dedicated namespace", nil)
	}
	condition := meta.FindStatusCondition(group.Status.Conditions, controller.ConditionReady)
	// Policy/quota preparation gates provisioning, not stopping an owned execution.
	suspending := executionMode(workload) == sandbox.Suspended
	if !suspending && (group.DeletionTimestamp != nil || condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != group.Generation) {
		return nil, true, diagnostics.New(controller.ReasonGroupNotReady, "Waiting for the AgentGroup namespace and policy", nil)
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: workload.Namespace}, ns); err != nil {
		return nil, false, err
	}
	if err := controller.CheckOwner(ns, group); err != nil {
		return nil, false, err
	}
	if ns.DeletionTimestamp != nil && !suspending {
		return nil, true, diagnostics.New("NamespaceTerminating", "The group namespace is terminating", nil)
	}
	return group, false, nil
}

func (r *Reconciler) reconcileExecution(ctx context.Context, workload *v1.AgentInstance, group *v1.AgentGroup) (ctrl.Result, error) {
	mode := executionMode(workload)
	if mode == "" {
		mode = sandbox.Running
	}
	if mode != sandbox.Running && mode != sandbox.Suspended {
		return r.pending(ctx, workload, "InvalidConfiguration", "operatingMode must be Running or Suspended")
	}
	config, err := r.resolveExecutionConfiguration(ctx, workload, group, mode)
	if err != nil {
		reason, message := diagnostics.Classify(err)
		return r.pending(ctx, workload, reason, message)
	}
	if workload.Status.DesiredRevision != config.Revision {
		base := workload.DeepCopy()
		workload.Status.DesiredRevision = config.Revision
		if err := r.patchStatus(ctx, workload, base); err != nil {
			return ctrl.Result{}, err
		}
	}
	reader := r.referenceReader()
	validationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	validation := validateCapabilities(validationCtx, reader, workload, group, &config.PodTemplate.Spec, config.VolumeClaimTemplates)
	cancel()
	blocked, err := r.capabilityBlocksExecution(ctx, workload, validation, config, mode)
	if err != nil {
		return r.failed(ctx, workload, err)
	}
	if blocked {
		return ctrl.Result{RequeueAfter: controller.DependencyRetry}, r.blockedStatusWithValidation(ctx, workload, "CapabilityValidationFailed", validation.Message, &validation)
	}
	object, configurationIssue, err := r.provisionFromPool(ctx, workload, config, mode)
	if err != nil {
		return r.pendingOrFailed(ctx, workload, err)
	}
	current, err := sandbox.Decode(object)
	if err != nil {
		return r.failed(ctx, workload, err)
	}
	interaction, interactionErr := r.reconcileInteraction(ctx, workload, current, object.GetGeneration())
	if interactionErr != nil {
		interactionErr = diagnostics.SafeError(interactionErr)
		interaction.pending(workload, "ReconcileError", interactionErr.Error())
	}
	config.Issue = configurationIssue
	if err := r.status(ctx, workload, object, current, interaction, validation, config); err != nil {
		return ctrl.Result{}, err
	}
	if workload.Status.Lifecycle != nil && workload.Status.Lifecycle.ExpiredAt != nil {
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if interactionErr != nil {
		return ctrl.Result{}, interactionErr
	}
	// Read-only Pod observation and allocation APIs are polled without Pod/Claim/Pool informers.
	return ctrl.Result{RequeueAfter: controller.DependencyRetry}, nil
}

func (r *Reconciler) resolveExecutionConfiguration(ctx context.Context, workload *v1.AgentInstance, group *v1.AgentGroup, mode string) (executionConfiguration, error) {
	configurationCtx, configurationCancel := context.WithTimeout(ctx, 10*time.Second)
	defer configurationCancel()
	resolved, err := desiredExecution(configurationCtx, r.referenceReader(), workload)
	templateFallback := false
	if err != nil && mode == sandbox.Suspended {
		// Missing credentials/storage must not prevent stopping an existing instance.
		if existing, getErr := r.existingExecution(ctx, workload); getErr == nil {
			if current, decodeErr := sandbox.Decode(existing); decodeErr == nil {
				resolved, err = &resolvedTemplate{PodTemplate: &current.Spec.PodTemplate, Service: current.Spec.Service, VolumeClaimTemplates: current.Spec.VolumeClaimTemplates}, nil
				templateFallback = true
			}
		}
	}
	if err != nil {
		return executionConfiguration{}, err
	}
	revision := workload.Status.DesiredRevision
	if !templateFallback {
		if err := r.bindDefinition(configurationCtx, workload, resolved.Definition); err != nil {
			return executionConfiguration{}, err
		}
		if err := r.bindWorkspace(configurationCtx, workload, resolved.Workspace); err != nil {
			return executionConfiguration{}, err
		}
		resolved.SnapshotSources, err = r.resolveSnapshotSources(configurationCtx, workload, group, resolved.VolumeClaimTemplates)
		if err != nil {
			return executionConfiguration{}, err
		}
		revision, err = configRevision(configurationCtx, r.referenceReader(), workload.Namespace, *resolved)
		if err != nil {
			return executionConfiguration{}, err
		}
	}
	return executionConfiguration{Configuration: *resolved, Revision: revision, Fallback: templateFallback}, nil
}

// An allocated, matching Agent continues running when capability evidence changes.
func (r *Reconciler) capabilityBlocksExecution(ctx context.Context, workload *v1.AgentInstance, validation metav1.Condition, config executionConfiguration, mode string) (bool, error) {
	if validation.Status == metav1.ConditionFalse && mode != sandbox.Suspended {
		object, err := r.existingExecution(ctx, workload)
		reason, _ := diagnostics.Classify(err)
		if err != nil && !apierrors.IsNotFound(err) && reason != controller.ReasonPoolPending {
			return false, err
		}
		preserveRunning := false
		if err == nil {
			current, err := sandbox.Decode(object)
			if err != nil {
				return false, err
			}
			preserveRunning = current.Spec.OperatingMode == sandbox.Running && validateAssignedSandbox(current, config.Configuration) == nil
		}
		if !preserveRunning {
			return true, nil
		}
	}
	return false, nil
}

func (r *Reconciler) pendingOrFailed(ctx context.Context, workload *v1.AgentInstance, err error) (ctrl.Result, error) {
	if _, ok := err.(*diagnostics.Error); ok {
		reason, message := diagnostics.Classify(err)
		return r.pending(ctx, workload, reason, message)
	}
	return r.failed(ctx, workload, err)
}
