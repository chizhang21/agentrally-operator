package connector

import (
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// InstanceDiagnostic is a current observation, not another lifecycle state.
type InstanceDiagnostic struct {
	Stage    string             `json:"stage"`
	Reason   string             `json:"reason"`
	Message  string             `json:"message"`
	Resource diagnosticResource `json:"resource"`
}

type diagnosticResource struct {
	Kind            string    `json:"kind"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

func newInstanceDiagnostic(stage, reason, message, kind string, object metav1.Object) InstanceDiagnostic {
	return InstanceDiagnostic{Stage: stage, Reason: reason, Message: message,
		Resource: diagnosticResource{Kind: kind, Name: object.GetName(), UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}}
}

func observationDiagnostic(out *InstanceObservation, instance *v1.AgentInstance, pod *corev1.Pod, now time.Time) InstanceDiagnostic {
	if out.State == "Idle" {
		return newInstanceDiagnostic("Idle", "SandboxSuspended", "Suspension is confirmed and the execution Pod is absent; process release remains unverified", "AgentInstance", instance)
	}
	if out.Reason == "Stopping" || out.Reason == "ExecutionResourcesRemoved" {
		if pod != nil && pod.DeletionTimestamp != nil {
			issue := diagnostics.Termination(pod, now)
			return newInstanceDiagnostic("Cleanup", issue.Reason, issue.Message, "Pod", pod)
		}
		if condition := currentObservedCondition(instance, controller.ConditionCleanupReady); condition != nil && condition.Status != metav1.ConditionTrue {
			switch condition.Reason {
			case "ExecutionCleanupStalled", "CleanupObservationBlocked", "CleanupFailed":
				return newInstanceDiagnostic("Cleanup", condition.Reason, "Execution cleanup is blocked; inspect controller and runtime health before retrying", "AgentInstance", instance)
			}
		}
		return newInstanceDiagnostic("Cleanup", out.Reason, "Execution cleanup is in progress; removed API resources do not prove process release", "AgentInstance", instance)
	}
	// Desired configuration may differ from an otherwise healthy accepted execution.
	if condition := currentObservedCondition(instance, controller.ConditionConfigurationAccepted); condition != nil && condition.Status != metav1.ConditionTrue {
		reason, message := "ConfigurationNotAccepted", "The desired configuration has not been accepted; inspect the Instance configuration and references"
		switch condition.Reason {
		case "DependencyNotFound":
			reason, message = condition.Reason, "A required configuration resource is missing; create it or correct the reference"
		case "DependencyAccessDenied":
			reason, message = condition.Reason, "A configuration resource cannot be read; check Operator permissions"
		case "DependencyReadFailed":
			reason, message = condition.Reason, "A configuration resource could not be read; check API connectivity and retry"
		case "InvalidConfiguration":
			reason, message = condition.Reason, "The desired configuration is invalid; check its fields and references"
		default:
			if condition.Status == metav1.ConditionUnknown {
				reason, message = "ConfigurationUnobserved", "The desired configuration has not yet been verified"
			}
		}
		return newInstanceDiagnostic("Configuration", reason, message, "AgentInstance", instance)
	}
	if pod != nil {
		if issue := diagnostics.Pod(pod, now); issue != nil {
			stage := "Startup"
			switch issue.Reason {
			case "Unschedulable", "SchedulingGated":
				stage = "Allocation"
			case "CrashLoopBackOff", "OOMKilled", "ContainerExited", "PodFailed", "PodSucceeded", "ContainersNotReady", "ReadinessStalled", "PodNotReady":
				stage = "Runtime"
			case "PodStarting", "SandboxStarting", "PodStartupStalled":
				if condition := currentObservedCondition(instance, controller.ConditionAvailable); condition != nil && condition.Status == metav1.ConditionFalse && instance.Status.PodUID == string(pod.UID) {
					switch condition.Reason {
					case "VolumeMountFailed", "VolumeAttachFailed", "PodSandboxCreationFailed":
						return newInstanceDiagnostic("Startup", condition.Reason, "The Controller observed a storage or runtime startup failure for this Pod; inspect node and runtime diagnostics", "AgentInstance", instance)
					}
				}
			}
			return newInstanceDiagnostic(stage, issue.Reason, issue.Message, "Pod", pod)
		}
		if out.State == "Active" {
			return newInstanceDiagnostic("Runtime", "PodReady", "The current execution Pod is running and ready", "Pod", pod)
		}
	} else if condition := currentObservedCondition(instance, controller.ConditionProvisioningReady); condition != nil && condition.Status != metav1.ConditionTrue {
		return newInstanceDiagnostic("Allocation", "AllocationPending", "A matching execution has not yet been allocated; inspect provisioning and capacity", "AgentInstance", instance)
	}
	return newInstanceDiagnostic("Observation", out.Reason, "Current execution evidence is incomplete; query again after reconciliation", "AgentInstance", instance)
}

func currentObservedCondition(instance *v1.AgentInstance, kind string) *metav1.Condition {
	condition := meta.FindStatusCondition(instance.Status.Conditions, kind)
	if condition == nil || condition.ObservedGeneration != instance.Generation {
		return nil
	}
	return condition
}
