package connector

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestInstanceDiagnosticsPreserveEvidenceAndPrivacy(t *testing.T) {
	for _, scenario := range []struct {
		name, stage, reason, kind string
		change                    func(*v1.AgentInstance, *corev1.Pod)
	}{
		{"healthy", "Runtime", "PodReady", "Pod", func(*v1.AgentInstance, *corev1.Pod) {}},
		{"image", "Startup", "ImagePullBackOff", "Pod", func(_ *v1.AgentInstance, p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "sensitive-diagnostic-marker"}}}}}
		}},
		{"unknown", "Observation", "ContainerObservationUnavailable", "AgentInstance", func(_ *v1.AgentInstance, p *corev1.Pod) { p.Status = corev1.PodStatus{Phase: corev1.PodRunning} }},
		{"configuration", "Configuration", "DependencyNotFound", "AgentInstance", func(w *v1.AgentInstance, _ *corev1.Pod) {
			w.Status.Conditions = []metav1.Condition{{Type: "ConfigurationAccepted", Status: metav1.ConditionFalse, ObservedGeneration: w.Generation, Reason: "DependencyNotFound", Message: "sensitive-diagnostic-marker"}}
		}},
		{"stale configuration", "Runtime", "PodReady", "Pod", func(w *v1.AgentInstance, _ *corev1.Pod) {
			w.Status.Conditions = []metav1.Condition{{Type: "ConfigurationAccepted", Status: metav1.ConditionFalse, ObservedGeneration: w.Generation - 1, Reason: "DependencyNotFound", Message: "sensitive-diagnostic-marker"}}
		}},
		{"unknown configuration reason", "Configuration", "ConfigurationNotAccepted", "AgentInstance", func(w *v1.AgentInstance, _ *corev1.Pod) {
			w.Status.Conditions = []metav1.Condition{{Type: "ConfigurationAccepted", Status: metav1.ConditionFalse, ObservedGeneration: w.Generation, Reason: "sensitive-diagnostic-marker", Message: "sensitive-diagnostic-marker"}}
		}},
		{"persisted restart", "Cleanup", "Stopping", "AgentInstance", func(w *v1.AgentInstance, _ *corev1.Pod) {
			w.Status.Conditions = []metav1.Condition{{Type: "Restarting", Status: metav1.ConditionTrue, ObservedGeneration: w.Generation - 1, Reason: "RestartRequested", Message: "sensitive-diagnostic-marker"}}
		}},
		{"termination stalled", "Cleanup", "PodTerminationStalled", "Pod", func(_ *v1.AgentInstance, p *corev1.Pod) {
			stamp := metav1.NewTime(time.Now().Add(-6 * time.Minute))
			p.DeletionTimestamp = &stamp
		}},
		{"mount evidence", "Startup", "VolumeMountFailed", "AgentInstance", func(w *v1.AgentInstance, p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending}
			w.Status.PodUID = string(p.UID)
			w.Status.Conditions = []metav1.Condition{{Type: "Available", Status: metav1.ConditionFalse, ObservedGeneration: w.Generation, Reason: "VolumeMountFailed", Message: "sensitive-diagnostic-marker"}}
		}},
		{"old mount evidence", "Startup", "PodStarting", "Pod", func(w *v1.AgentInstance, p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending}
			w.Status.PodUID = "previous-pod"
			w.Status.Conditions = []metav1.Condition{{Type: "Available", Status: metav1.ConditionFalse, ObservedGeneration: w.Generation, Reason: "VolumeMountFailed", Message: "sensitive-diagnostic-marker"}}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			kube, req := managementFixture(t)
			instanceObject, err := kube.Tracker().Get(workloadGVR, req.Namespace, req.Name)
			if err != nil {
				t.Fatal(err)
			}
			podObject, err := kube.Tracker().Get(podResource, req.Namespace, "warm-pod")
			if err != nil {
				t.Fatal(err)
			}
			var instance v1.AgentInstance
			var pod corev1.Pod
			if err := json.Unmarshal(mustJSON(instanceObject), &instance); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(mustJSON(podObject), &pod); err != nil {
				t.Fatal(err)
			}
			scenario.change(&instance, &pod)
			for _, object := range []struct {
				value    runtime.Object
				resource schema.GroupVersionResource
			}{{&instance, workloadGVR}, {&pod, podResource}} {
				fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object.value)
				if err != nil {
					t.Fatal(err)
				}
				if err := kube.Tracker().Update(object.resource, &unstructured.Unstructured{Object: fields}, req.Namespace); err != nil {
					t.Fatal(err)
				}
			}
			response := (Executor{Client: kube}).Execute(t.Context(), req)
			var body struct {
				Diagnostic struct {
					Stage, Reason, Message string
					Resource               struct{ Kind, Name, UID, ResourceVersion string }
				}
			}
			if response.Status != http.StatusOK || json.Unmarshal(response.Body, &body) != nil {
				t.Fatalf("diagnostic query failed: %s", response.Body)
			}
			if body.Diagnostic.Stage != scenario.stage || body.Diagnostic.Reason != scenario.reason || body.Diagnostic.Resource.Kind != scenario.kind || body.Diagnostic.Resource.UID == "" || body.Diagnostic.Resource.ResourceVersion == "" || body.Diagnostic.Message == "" {
				t.Fatalf("diagnostic did not reflect current evidence: %+v", body.Diagnostic)
			}
			if strings.Contains(string(response.Body), "sensitive-diagnostic-marker") {
				t.Fatal("diagnostics exposed untrusted status or Pod messages")
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() != "get" {
					t.Fatal("diagnostic read produced a side effect")
				}
			}
		})
	}
}
