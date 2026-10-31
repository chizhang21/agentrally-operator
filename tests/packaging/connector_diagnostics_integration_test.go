//go:build integration

package packaging_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/connector"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyConnectorDiagnostics(t *testing.T, ctx context.Context, admin client.Client, base connector.Request, send func(connector.Request, int) connector.Response) {
	t.Helper()
	var body unstructured.Unstructured
	if err := json.Unmarshal(base.Body, &body); err != nil {
		t.Fatal(err)
	}
	body.SetName("diagnostic-agent")
	base.ID = "create-diagnostic-agent"
	var err error
	base.Body, err = json.Marshal(&body)
	if err != nil {
		t.Fatal(err)
	}
	created := send(base, http.StatusCreated)
	var instance v1.AgentInstance
	if err := json.Unmarshal(created.Body, &instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.Conditions = []metav1.Condition{{Type: "ConfigurationAccepted", Status: metav1.ConditionFalse,
		ObservedGeneration: instance.Generation, LastTransitionTime: metav1.Now(), Reason: "DependencyNotFound", Message: "sensitive-diagnostic-marker"}}
	if err := admin.Status().Update(ctx, &instance); err != nil {
		t.Fatal(err)
	}
	read := base
	read.ID, read.Method, read.Resource, read.Name, read.Body = "diagnose-current", http.MethodGet, "instanceobservations", instance.Name, nil
	response := send(read, http.StatusOK)
	var observation connector.InstanceObservation
	if json.Unmarshal(response.Body, &observation) != nil || observation.Diagnostic.Stage != "Configuration" || observation.Diagnostic.Reason != "DependencyNotFound" || observation.Diagnostic.Resource.UID != instance.UID || strings.Contains(string(response.Body), "sensitive-diagnostic-marker") {
		t.Fatal("current configuration diagnostic lost identity or exposed raw status")
	}
	// Feasibility evidence for conditional suspension: replacing a Pod does not
	// invalidate an Instance CAS. Neither this nor envtest proves runtime fencing.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "observed-intent-boundary", Namespace: instance.Namespace},
		Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "agent", Image: "example/agent", SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}}}}
	if err := admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	firstUID := pod.UID
	if err := admin.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	pod.ObjectMeta = metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}
	if err := admin.Create(ctx, pod); err != nil || pod.UID == firstUID {
		t.Fatalf("Pod replacement fixture failed: %v", err)
	}
	current := &v1.AgentInstance{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(&instance), current); err != nil || current.ResourceVersion != instance.ResourceVersion {
		t.Fatalf("independent Pod change unexpectedly invalidated the Instance CAS: %v", err)
	}
	seconds := int32(3600)
	instance.Spec.Lifecycle = &v1.AgentInstanceLifecycle{MaxLifetimeSeconds: &seconds}
	if err := admin.Update(ctx, &instance); err != nil {
		t.Fatal("Instance CAS after independent Pod replacement:", err)
	}
	read.ID = "diagnose-stale-configuration"
	response = send(read, http.StatusOK)
	if json.Unmarshal(response.Body, &observation) != nil || observation.Diagnostic.Stage != "Observation" || strings.Contains(string(response.Body), "DependencyNotFound") || strings.Contains(string(response.Body), "sensitive-diagnostic-marker") {
		t.Fatal("stale configuration condition was presented as current evidence")
	}
}
