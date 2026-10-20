package packaging_test

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

func TestMetricsNetworkPolicySelectsManager(t *testing.T) {
	if _, exists := renderObjects(t)["NetworkPolicy/arally-operator-metrics"]; exists {
		t.Fatal("metrics policy must be opt-in")
	}
	objects := renderObjects(t, "--set", "metrics.networkPolicy.enabled=true")
	var policy networkingv1.NetworkPolicy
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(objects["NetworkPolicy/arally-operator-metrics"], &policy); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(objects["Deployment/arally-operator"], &deployment); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, deployment.Spec.Selector.MatchLabels) || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 || policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["metrics"] != "enabled" || policy.Spec.Ingress[0].Ports[0].Port.IntVal != 8080 {
		t.Fatal("metrics policy no longer matches the manager and authorized namespaces")
	}
}
