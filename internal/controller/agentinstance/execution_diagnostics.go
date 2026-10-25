package agentinstance

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This is a diagnostic threshold, not a deadline for killing or replacing Pods.
const executionStallThreshold = 5 * time.Minute

type executionDiagnostic struct {
	Reason  string
	Message string
}

// Core remains the authority for successful execution. Direct, UID-verified Pod
// observations can explain a failure before Core has refreshed its conditions.
func (r *Reconciler) executionReadiness(ctx context.Context, w *v1.AgentInstance, ready metav1.Condition, pod *corev1.Pod, podErr error, suspended bool) metav1.Condition {
	var diagnostic *executionDiagnostic
	switch {
	case podErr != nil:
		ready.Status = metav1.ConditionUnknown
		ready.Reason = "PodObservationBlocked"
		ready.Message = "Cannot inspect the execution Pod; check Sandbox ownership and operator Pod read permissions"
		ready.LastTransitionTime = metav1.Time{}
		return ready
	case executionMode(w) == sandbox.Suspended:
		diagnostic = suspensionDiagnostic(pod, r.now())
		if pod == nil && !suspended {
			// Core must still acknowledge the current Sandbox generation.
			diagnostic = &executionDiagnostic{"Suspending", "Waiting for Core to confirm suspension after the execution Pod is removed"}
		}
	case pod == nil:
		if ready.Status == metav1.ConditionTrue {
			diagnostic = &executionDiagnostic{"PodNotObserved", "The execution Pod does not exist; waiting for Core to create it and refresh readiness"}
		}
	default:
		diagnostic = podDiagnostic(pod, r.now())
		if diagnostic != nil && (diagnostic.Reason == "PodStarting" || diagnostic.Reason == "SandboxStarting" || diagnostic.Reason == "PodStartupStalled") {
			if event := r.executionEventDiagnostic(ctx, pod); event != nil {
				diagnostic = event
			}
		}
	}
	if diagnostic != nil {
		ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, diagnostic.Reason, diagnostic.Message
		// A Pod diagnosis did not occur at Core's earlier readiness transition.
		// SetStatusCondition records a new transition only when status changes.
		ready.LastTransitionTime = metav1.Time{}
	}
	return ready
}

func suspensionDiagnostic(pod *corev1.Pod, now time.Time) *executionDiagnostic {
	if pod == nil {
		return &executionDiagnostic{"SandboxSuspended", "Core removed the execution Pod; the Sandbox and its workspace PVCs are retained"}
	}
	if pod.DeletionTimestamp != nil {
		return terminationDiagnostic(pod, now)
	}
	return &executionDiagnostic{"Suspending", "Waiting for Core to delete the execution Pod; inspect Core controller status if suspension does not progress"}
}

func terminationDiagnostic(pod *corev1.Pod, now time.Time) *executionDiagnostic {
	if now.Sub(pod.DeletionTimestamp.Time) >= executionStallThreshold {
		return &executionDiagnostic{
			"PodTerminationStalled",
			"Pod remains present at least 5 minutes past its deletion timestamp; ask the cluster administrator to inspect kubelet, container runtime, volume teardown and finalizers",
		}
	}
	return &executionDiagnostic{"PodTerminating", "Waiting for Pod deletion and volume teardown; Core creates a replacement only after cleanup when Running is requested"}
}

func podDiagnostic(pod *corev1.Pod, now time.Time) *executionDiagnostic {
	if pod == nil {
		return nil
	}
	if pod.DeletionTimestamp != nil {
		return terminationDiagnostic(pod, now)
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		return &executionDiagnostic{"PodSucceeded", "The execution Pod completed; inspect its logs for the result"}
	}
	if scheduled := podCondition(pod, corev1.PodScheduled); scheduled != nil && scheduled.Status == corev1.ConditionFalse {
		return schedulingDiagnostic(scheduled)
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if issue := containerDiagnostic(status); issue != nil {
				return issue
			}
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return &executionDiagnostic{"PodFailed", "The execution Pod failed; inspect Pod events and container logs"}
	}
	return podProgressDiagnostic(pod, now)
}

func schedulingDiagnostic(condition *corev1.PodCondition) *executionDiagnostic {
	if condition.Reason == "SchedulingGated" {
		return &executionDiagnostic{"SchedulingGated", "Pod scheduling is gated; check the controller responsible for removing scheduling gates"}
	}
	var causes []string
	for _, entry := range []struct{ fragment, safe string }{
		{"Insufficient cpu", "insufficient CPU"},
		{"Insufficient memory", "insufficient memory"},
		{"untolerated taint", "untolerated taints"},
		{"node affinity", "node affinity constraints"},
		{"unbound immediate PersistentVolumeClaims", "unbound PVCs"},
	} {
		if strings.Contains(condition.Message, entry.fragment) {
			causes = append(causes, entry.safe)
		}
	}
	message := "Pod cannot be scheduled"
	if len(causes) > 0 {
		message += " (" + strings.Join(causes, ", ") + ")"
	}
	return &executionDiagnostic{"Unschedulable", message + "; check Pod events, resource requests, available nodes, taints and storage binding"}
}

// Container messages and termination output can contain application credentials.
// Publish known reasons, names, counters and exit codes, never their raw text.
func containerDiagnostic(status corev1.ContainerStatus) *executionDiagnostic {
	if terminated := status.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
		return terminationFailure(status.Name, terminated)
	}
	waiting := status.State.Waiting
	if waiting == nil {
		return nil
	}
	prefix := fmt.Sprintf("Container %s: ", status.Name)
	switch waiting.Reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return &executionDiagnostic{waiting.Reason, prefix + "cannot pull its image; check the image reference, registry access and imagePullSecrets"}
	case "CrashLoopBackOff":
		if last := status.LastTerminationState.Terminated; last != nil {
			if last.Reason == "OOMKilled" {
				return terminationFailure(status.Name, last)
			}
			return &executionDiagnostic{waiting.Reason, fmt.Sprintf("%srestarting after exit code %d (%d restarts); check the application command and container logs", prefix, last.ExitCode, status.RestartCount)}
		}
		return &executionDiagnostic{waiting.Reason, prefix + "repeatedly restarting; check the application command, probes and container logs"}
	case "CreateContainerConfigError":
		return &executionDiagnostic{waiting.Reason, prefix + "configuration failed; check Secret/ConfigMap references, security settings and Pod events"}
	case "CreateContainerError", "RunContainerError", "StartError":
		return &executionDiagnostic{waiting.Reason, prefix + "could not start; check the image command, filesystem permissions, security settings and node runtime"}
	}
	return nil
}

func terminationFailure(name string, state *corev1.ContainerStateTerminated) *executionDiagnostic {
	if state.Reason == "OOMKilled" {
		return &executionDiagnostic{"OOMKilled", fmt.Sprintf("Container %s was killed for exceeding available memory; check memory limits and application usage", name)}
	}
	return &executionDiagnostic{"ContainerExited", fmt.Sprintf("Container %s exited with code %d; check its command, configuration and container logs", name, state.ExitCode)}
}

func podProgressDiagnostic(pod *corev1.Pod, now time.Time) *executionDiagnostic {
	if ready := podCondition(pod, corev1.PodReady); ready != nil && ready.Status == corev1.ConditionTrue {
		return nil
	}
	if pod.Status.Phase == corev1.PodPending {
		if !pod.CreationTimestamp.IsZero() && now.Sub(pod.CreationTimestamp.Time) >= executionStallThreshold {
			return &executionDiagnostic{"PodStartupStalled", "Pod startup has not completed for at least 5 minutes; inspect Pod events, volume mounts, image pulls and the node runtime"}
		}
		if condition := podCondition(pod, corev1.PodReadyToStartContainers); condition != nil && condition.Status == corev1.ConditionFalse {
			return &executionDiagnostic{"SandboxStarting", "Waiting for the Pod sandbox and networking; inspect Pod events for runtime, network or volume failures"}
		}
		return &executionDiagnostic{"PodStarting", "Waiting for Pod initialization and container startup; inspect Pod events if this does not progress"}
	}
	if condition := podCondition(pod, corev1.ContainersReady); condition != nil && condition.Status == corev1.ConditionFalse {
		if !condition.LastTransitionTime.IsZero() && now.Sub(condition.LastTransitionTime.Time) >= executionStallThreshold {
			return &executionDiagnostic{"ReadinessStalled", "Containers have remained unready for at least 5 minutes; check application startup, readiness probes and container logs"}
		}
		return &executionDiagnostic{"ContainersNotReady", "Waiting for container readiness; check application startup, readiness probes and container logs"}
	}
	if condition := podCondition(pod, corev1.PodReady); condition != nil && condition.Status == corev1.ConditionFalse {
		return &executionDiagnostic{"PodNotReady", "Waiting for Pod readiness; check readiness gates, probes and container logs"}
	}
	return nil
}

func podCondition(pod *corev1.Pod, kind corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == kind {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
