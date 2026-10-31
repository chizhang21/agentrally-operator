package diagnostics

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ExecutionStallThreshold is diagnostic, never a deadline for killing Pods.
const ExecutionStallThreshold = 5 * time.Minute

// ExecutionDiagnostic contains only classified reasons and safe user guidance.
type ExecutionDiagnostic struct {
	Reason  string
	Message string
}

// Suspension describes Pod removal without claiming that processes are fenced.
func Suspension(pod *corev1.Pod, now time.Time) *ExecutionDiagnostic {
	if pod == nil {
		return &ExecutionDiagnostic{"SandboxSuspended", "Core removed the execution Pod; the Sandbox and its workspace PVCs are retained"}
	}
	if pod.DeletionTimestamp != nil {
		return Termination(pod, now)
	}
	return &ExecutionDiagnostic{"Suspending", "Waiting for Core to delete the execution Pod; inspect Core controller status if suspension does not progress"}
}

// Termination describes a Pod with a deletion timestamp, including stalled removal.
func Termination(pod *corev1.Pod, now time.Time) *ExecutionDiagnostic {
	if now.Sub(pod.DeletionTimestamp.Time) >= ExecutionStallThreshold {
		return &ExecutionDiagnostic{
			"PodTerminationStalled",
			"Pod remains present at least 5 minutes past its deletion timestamp; ask the cluster administrator to inspect kubelet, container runtime, volume teardown and finalizers",
		}
	}
	return &ExecutionDiagnostic{"PodTerminating", "Waiting for Pod deletion and volume teardown; Core creates a replacement only after cleanup when Running is requested"}
}

// Pod classifies observed Pod problems. A nil result alone does not prove health.
func Pod(pod *corev1.Pod, now time.Time) *ExecutionDiagnostic {
	if pod == nil {
		return nil
	}
	if pod.DeletionTimestamp != nil {
		return Termination(pod, now)
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		return &ExecutionDiagnostic{"PodSucceeded", "The execution Pod completed; inspect its logs for the result"}
	}
	if scheduled := podCondition(pod, corev1.PodScheduled); scheduled != nil && scheduled.Status == corev1.ConditionFalse {
		return Scheduling(scheduled)
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if issue := containerDiagnostic(status); issue != nil {
				return issue
			}
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return &ExecutionDiagnostic{"PodFailed", "The execution Pod failed; inspect Pod events and container logs"}
	}
	return podProgressDiagnostic(pod, now)
}

// Scheduling classifies a false PodScheduled condition without copying its text.
func Scheduling(condition *corev1.PodCondition) *ExecutionDiagnostic {
	if condition.Reason == "SchedulingGated" {
		return &ExecutionDiagnostic{"SchedulingGated", "Pod scheduling is gated; check the controller responsible for removing scheduling gates"}
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
	return &ExecutionDiagnostic{"Unschedulable", message + "; check Pod events, resource requests, available nodes, taints and storage binding"}
}

// Container messages and termination output can contain application credentials.
// Publish known reasons, names, counters and exit codes, never their raw text.
func containerDiagnostic(status corev1.ContainerStatus) *ExecutionDiagnostic {
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
		return &ExecutionDiagnostic{waiting.Reason, prefix + "cannot pull its image; check the image reference, registry access and imagePullSecrets"}
	case "CrashLoopBackOff":
		if last := status.LastTerminationState.Terminated; last != nil {
			if last.Reason == "OOMKilled" {
				return terminationFailure(status.Name, last)
			}
			return &ExecutionDiagnostic{waiting.Reason, fmt.Sprintf("%srestarting after exit code %d (%d restarts); check the application command and container logs", prefix, last.ExitCode, status.RestartCount)}
		}
		return &ExecutionDiagnostic{waiting.Reason, prefix + "repeatedly restarting; check the application command, probes and container logs"}
	case "CreateContainerConfigError":
		return &ExecutionDiagnostic{waiting.Reason, prefix + "configuration failed; check Secret/ConfigMap references, security settings and Pod events"}
	case "CreateContainerError", "RunContainerError", "StartError":
		return &ExecutionDiagnostic{waiting.Reason, prefix + "could not start; check the image command, filesystem permissions, security settings and node runtime"}
	}
	return nil
}

func terminationFailure(name string, state *corev1.ContainerStateTerminated) *ExecutionDiagnostic {
	if state.Reason == "OOMKilled" {
		return &ExecutionDiagnostic{"OOMKilled", fmt.Sprintf("Container %s was killed for exceeding available memory; check memory limits and application usage", name)}
	}
	return &ExecutionDiagnostic{"ContainerExited", fmt.Sprintf("Container %s exited with code %d; check its command, configuration and container logs", name, state.ExitCode)}
}

func podProgressDiagnostic(pod *corev1.Pod, now time.Time) *ExecutionDiagnostic {
	if ready := podCondition(pod, corev1.PodReady); ready != nil && ready.Status == corev1.ConditionTrue {
		return nil
	}
	if pod.Status.Phase == corev1.PodPending {
		if !pod.CreationTimestamp.IsZero() && now.Sub(pod.CreationTimestamp.Time) >= ExecutionStallThreshold {
			return &ExecutionDiagnostic{"PodStartupStalled", "Pod startup has not completed for at least 5 minutes; inspect Pod events, volume mounts, image pulls and the node runtime"}
		}
		if condition := podCondition(pod, corev1.PodReadyToStartContainers); condition != nil && condition.Status == corev1.ConditionFalse {
			return &ExecutionDiagnostic{"SandboxStarting", "Waiting for the Pod sandbox and networking; inspect Pod events for runtime, network or volume failures"}
		}
		return &ExecutionDiagnostic{"PodStarting", "Waiting for Pod initialization and container startup; inspect Pod events if this does not progress"}
	}
	if condition := podCondition(pod, corev1.ContainersReady); condition != nil && condition.Status == corev1.ConditionFalse {
		if !condition.LastTransitionTime.IsZero() && now.Sub(condition.LastTransitionTime.Time) >= ExecutionStallThreshold {
			return &ExecutionDiagnostic{"ReadinessStalled", "Containers have remained unready for at least 5 minutes; check application startup, readiness probes and container logs"}
		}
		return &ExecutionDiagnostic{"ContainersNotReady", "Waiting for container readiness; check application startup, readiness probes and container logs"}
	}
	if condition := podCondition(pod, corev1.PodReady); condition != nil && condition.Status == corev1.ConditionFalse {
		return &ExecutionDiagnostic{"PodNotReady", "Waiting for Pod readiness; check readiness gates, probes and container logs"}
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
