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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	seconds := int32(3600)
	instance.Spec.Lifecycle = &v1.AgentInstanceLifecycle{MaxLifetimeSeconds: &seconds}
	if err := admin.Update(ctx, &instance); err != nil {
		t.Fatal("update Instance generation:", err)
	}
	read.ID = "diagnose-stale-configuration"
	response = send(read, http.StatusOK)
	if json.Unmarshal(response.Body, &observation) != nil || observation.Diagnostic.Stage != "Observation" || strings.Contains(string(response.Body), "DependencyNotFound") || strings.Contains(string(response.Body), "sensitive-diagnostic-marker") {
		t.Fatal("stale configuration condition was presented as current evidence")
	}
}
