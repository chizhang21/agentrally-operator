package agentinstance

import (
	"context"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/sandbox"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const activityStateAnnotation = "arally.io/activity-state"
const lastActivityAnnotation = "arally.io/last-activity-time"
const activityClockSkew = 5 * time.Second

// executionMode combines explicit user intent with optional idle suspension.
// It never rewrites spec or uses execution replacement as a wake-up mechanism.
func executionMode(instance *v1.AgentInstance) string {
	if instance.Spec.OperatingMode == sandbox.Suspended || instance.Status.Activity != nil && instance.Status.Activity.Suspended {
		return sandbox.Suspended
	}
	if instance.Spec.OperatingMode == "" {
		return sandbox.Running
	}
	return instance.Spec.OperatingMode
}

func (r *Reconciler) observeActivity(ctx context.Context, instance *v1.AgentInstance) error {
	base := instance.DeepCopy()
	policy := instance.Spec.Lifecycle
	if policy == nil || policy.IdleTimeoutSeconds == nil && policy.ActivityLeaseRef == nil {
		instance.Status.Activity = nil
		meta.RemoveStatusCondition(&instance.Status.Conditions, controller.ConditionActivityReady)
		return r.patchStatus(ctx, instance, base)
	}
	condition := metav1.ConditionUnknown
	reason, message := "ActivityUnavailable", "Waiting for a fresh activity Lease for the current execution"
	if policy.IdleTimeoutSeconds == nil || *policy.IdleTimeoutSeconds <= 0 || policy.ActivityLeaseRef == nil {
		condition, reason, message = metav1.ConditionFalse, "ActivityPolicyInvalid", "Configure a positive idle timeout and an activity Lease together"
	} else if instance.Status.SandboxUID != "" && instance.Status.Provisioning != nil && instance.Status.Provisioning.CompletedAt != nil {
		lease := &coordinationv1.Lease{}
		if err := r.referenceReader().Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: policy.ActivityLeaseRef.Name}, lease); err == nil {
			activity, issue := activityObservation(instance, lease, r.now())
			if issue != "" {
				reason, message = issue, "The activity Lease is stale, invalid, or belongs to another execution; automatic suspension is disabled"
			} else {
				instance.Status.Activity = activity
				condition, reason, message = metav1.ConditionTrue, "ActivityObserved", "Fresh caller activity is fenced to the current execution"
			}
		}
	}
	setInstanceCondition(instance, controller.ConditionActivityReady, condition, reason, message)
	return r.patchStatus(ctx, instance, base)
}

func activityObservation(instance *v1.AgentInstance, lease *coordinationv1.Lease, now time.Time) (*v1.AgentInstanceActivityStatus, string) {
	if lease.DeletionTimestamp != nil || controller.CheckOwner(lease, instance) != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(instance.Status.SandboxUID) {
		return nil, "ActivityIdentityMismatch"
	}
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds < 1 || *lease.Spec.LeaseDurationSeconds > 300 {
		return nil, "ActivityLeaseInvalid"
	}
	renewed := lease.Spec.RenewTime.Time
	validUntil := renewed.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	if renewed.After(now.Add(activityClockSkew)) || !now.Before(validUntil) {
		return nil, "ActivityLeaseStale"
	}
	last, err := time.Parse(time.RFC3339Nano, lease.Annotations[lastActivityAnnotation])
	if err != nil || last.After(now.Add(activityClockSkew)) || last.After(renewed.Add(activityClockSkew)) {
		return nil, "ActivityTimeInvalid"
	}
	last = last.UTC().Truncate(time.Microsecond)
	validUntil = validUntil.UTC().Truncate(time.Microsecond)
	if prior := instance.Status.Activity; prior != nil && prior.LastActivityAt != nil && last.Before(prior.LastActivityAt.Time) {
		return nil, "ActivityTimeRegressed"
	}
	activity := &v1.AgentInstanceActivityStatus{LastActivityAt: &metav1.MicroTime{Time: last}, ValidUntil: &metav1.MicroTime{Time: validUntil}}
	switch lease.Annotations[activityStateAnnotation] {
	case "Busy":
		return activity, ""
	case "Idle":
		anchor := last
		if allocated := instance.Status.Provisioning.CompletedAt.Time; allocated.After(anchor) {
			anchor = allocated
		}
		deadline := anchor.Add(time.Duration(*instance.Spec.Lifecycle.IdleTimeoutSeconds) * time.Second)
		activity.IdleDeadline = &metav1.MicroTime{Time: deadline.UTC()}
		activity.Suspended = !now.Before(deadline)
		return activity, ""
	default:
		return nil, "ActivityStateInvalid"
	}
}
