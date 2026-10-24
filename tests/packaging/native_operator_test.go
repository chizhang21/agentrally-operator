package packaging_test

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestHelmOperatorAndDelegatedRBAC(t *testing.T) {
	deployment, role, services := false, false, 0
	reportCreate := false
	discoveryReads := map[string]bool{}
	interactionWrites := map[string]bool{}
	eventWrites := map[string]bool{}
	for _, doc := range renderObjects(t, "--set", "replicaCount=2") {
		var kind metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &kind); err != nil {
			t.Fatal(err)
		}
		switch kind.Kind {
		case "Deployment":
			var dep appsv1.Deployment
			if err := yaml.Unmarshal(doc, &dep); err != nil {
				t.Fatal(err)
			}
			deployment = true
			if *dep.Spec.Replicas != 2 {
				t.Fatal("replica count constrained")
			}
			c := dep.Spec.Template.Spec.Containers[0]
			if len(c.Env) != 1 || c.Env[0].Name != "POD_NAMESPACE" {
				t.Fatalf("non-native startup dependency: %+v", c.Env)
			}
			if !strings.Contains(strings.Join(c.Args, " "), "--leader-elect=true") {
				t.Fatal("leader election disabled")
			}
			for _, port := range c.Ports {
				if port.ContainerPort != 8080 && port.ContainerPort != 8081 {
					t.Fatal("unexpected listener")
				}
			}
		case "ClusterRole":
			var cr rbacv1.ClusterRole
			if err := yaml.Unmarshal(doc, &cr); err != nil {
				t.Fatal(err)
			}
			role = true
			for _, rule := range cr.Rules {
				if slices.Contains(rule.APIGroups, "*") || slices.Contains(rule.Resources, "*") || slices.Contains(rule.Verbs, "*") {
					t.Fatalf("Operator RBAC must not grant wildcard permissions: %+v", rule)
				}
				for _, group := range rule.APIGroups {
					if group == "security.openshift.io" {
						for _, verb := range rule.Verbs {
							if verb != "get" && verb != "list" && verb != "watch" {
								t.Fatal("Operator must not grant admission-policy management permissions")
							}
						}
					}
				}
				for _, resource := range rule.Resources {
					if resource == "events" && slices.Contains(rule.APIGroups, "events.k8s.io") {
						for _, verb := range rule.Verbs {
							eventWrites[verb] = true
						}
					}
					if resource == "secrets" {
						for _, verb := range rule.Verbs {
							if verb != "get" {
								t.Fatalf("Secret access must be reference-only GET, got %s", verb)
							}
						}
					}
					if strings.HasPrefix(resource, "pods/") {
						t.Fatalf("duplicate upstream responsibility: %s", resource)
					}
					if resource == "sandboxes" {
						for _, verb := range rule.Verbs {
							if verb == "create" || verb == "delete" || verb == "update" {
								t.Fatal("Arally must delegate Sandbox creation/deletion to Extensions")
							}
						}
					}
					if resource == "services" || resource == "httproutes" || resource == "routes" || resource == "sandboxwarmpools" || resource == "sandboxtemplates" || resource == "sandboxclaims" {
						for _, verb := range rule.Verbs {
							if verb == "create" {
								interactionWrites[resource] = true
							}
						}
					}
					if resource == "clustercapabilities" {
						for _, verb := range rule.Verbs {
							if verb == "create" {
								reportCreate = true
							} else if verb != "get" && verb != "list" && verb != "watch" {
								t.Fatal("report bootstrap may only create; existing spec/metadata must remain untouched")
							}
						}
					}
					if resource == "gateways" || resource == "pods" || resource == "persistentvolumeclaims" || resource == "secrets" || resource == "serviceaccounts" || resource == "nodes" || resource == "configmaps" || resource == "daemonsets" || resource == "runtimeclasses" || resource == "storageclasses" || resource == "csidrivers" || resource == "csinodes" || resource == "volumesnapshotclasses" {
						discoveryReads[resource] = true
						for _, verb := range rule.Verbs {
							if verb != "get" && verb != "list" && verb != "watch" {
								t.Fatalf("dependency mutation permission: %s %s", resource, verb)
							}
						}
					}
				}
			}
		case "Service":
			var service corev1.Service
			if err := yaml.Unmarshal(doc, &service); err != nil {
				t.Fatal(err)
			}
			services++
			if service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Name != "metrics" {
				t.Fatal("non-metrics operator service retained")
			}
		}
	}
	for _, resource := range []string{"gateways", "pods", "persistentvolumeclaims", "secrets", "serviceaccounts", "nodes", "configmaps", "daemonsets", "runtimeclasses", "storageclasses", "csidrivers", "csinodes", "volumesnapshotclasses"} {
		if !discoveryReads[resource] {
			t.Fatalf("discovery read permission missing: %s", resource)
		}
	}
	for _, resource := range []string{"services", "httproutes", "routes", "sandboxwarmpools", "sandboxtemplates", "sandboxclaims"} {
		if !interactionWrites[resource] {
			t.Fatalf("interaction write permission missing: %s", resource)
		}
	}
	for _, verb := range []string{"create", "patch"} {
		if !eventWrites[verb] {
			t.Fatalf("events.k8s.io write permission missing: %s", verb)
		}
	}
	if !deployment || !role || services != 1 || !reportCreate {
		t.Fatal("Helm resources missing")
	}
}
