package packaging_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

var enabledOverrides = []string{
	"--set", "connector.enabled=true",
	"--set", "connector.endpoint=https://backend.example/",
	"--set", "connector.id=cluster-one",
	"--set", "connector.credentialsSecret=backend-credentials",
}

func TestConnectorChartEnablesWorkerInOperator(t *testing.T) {
	managerOnly := renderObjects(t, "--set", "replicaCount=2")
	connected := renderObjects(t, append(enabledOverrides, "--set", "replicaCount=2")...)
	if _, found := managerOnly["CustomResourceDefinition/clustercapabilities.arally.io"]; !found {
		t.Fatal("manager chart lost capability report CRD")
	}
	for name, object := range managerOnly {
		if name == "Deployment/agentrally-operator" {
			continue
		}
		if strings.Contains(name, "connector") || !reflect.DeepEqual(object, connected[name]) {
			t.Fatalf("connector changed manager resource %s", name)
		}
	}
	if len(connected) != len(managerOnly)+2 {
		t.Fatal("connector introduced unexpected resources")
	}
	object := connected["Deployment/agentrally-operator"]
	var deployment appsv1.Deployment
	if object == nil || yaml.Unmarshal(object, &deployment) != nil {
		t.Fatal("operator deployment missing")
	}
	pod := deployment.Spec.Template.Spec
	if *deployment.Spec.Replicas != 2 || len(pod.Containers) != 1 || pod.ServiceAccountName != "agentrally-operator" {
		t.Fatal("connector must run inside the operator")
	}
	container := pod.Containers[0]
	expectedArgs := []string{"--leader-elect=true", "--metrics-bind-address=:8080", "--health-probe-bind-address=:8081", "--connector-enabled=true", "--connector-endpoint=https://backend.example/", "--connector-id=cluster-one"}
	if len(container.Command) != 0 || !reflect.DeepEqual(container.Args, expectedArgs) || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("connector startup/security configuration incorrect")
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].Secret.SecretName != "backend-credentials" || len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != pod.Volumes[0].Name || container.VolumeMounts[0].MountPath != "/var/run/agentrally-connector" || !container.VolumeMounts[0].ReadOnly {
		t.Fatal("credential must be mounted as a rotating read-only Secret")
	}
	// Turning the feature off removes its flags and Secret mount, not the operator.
	var disabled appsv1.Deployment
	if err := yaml.Unmarshal(managerOnly["Deployment/agentrally-operator"], &disabled); err != nil {
		t.Fatal(err)
	}
	pod.Volumes = nil
	pod.Containers[0].Args = pod.Containers[0].Args[:3]
	pod.Containers[0].VolumeMounts = nil
	if !reflect.DeepEqual(pod, disabled.Spec.Template.Spec) {
		t.Fatal("connector changed unrelated operator settings")
	}
	var role rbacv1.ClusterRole
	if yaml.Unmarshal(connected["ClusterRole/agentrally-operator-agentrally-system-connector"], &role) != nil {
		t.Fatal("invalid connector role")
	}
	expected := []rbacv1.PolicyRule{
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"arally.io"}, Resources: []string{"agentgroups", "agentdefinitions"}, Verbs: []string{"create", "delete"}},
		{APIGroups: []string{"arally.io"}, Resources: []string{"agentinstances"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"list", "create"}},
	}
	if !reflect.DeepEqual(role.Rules, expected) {
		t.Fatal("connector must only add EndpointSlice reads, missing CR writes and Secret list/create")
	}
	var binding rbacv1.ClusterRoleBinding
	if err := yaml.Unmarshal(connected["ClusterRoleBinding/agentrally-operator-agentrally-system-connector"], &binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Name != pod.ServiceAccountName || binding.Subjects[0].Namespace != "agentrally-system" {
		t.Fatal("connector RBAC bound to incorrect identity")
	}
}

func TestConnectorChartRequiresConfigurationOnlyWhenEnabled(t *testing.T) {
	for _, overrides := range [][]string{
		{"--set-string", "connector.invocationPreview=false"},
		{"--set", "connector.enabled=true"},
		{"--set", "connector.enabled=true", "--set", "connector.endpoint=http://backend.example"},
		{"--set", "connector.enabled=true", "--set", "connector.endpoint=https://backend.example", "--set", "connector.id=cluster"},
	} {
		args := append([]string{"template", "agentrally-operator", "../../chart", "-n", "agentrally-system"}, overrides...)
		if _, err := exec.Command("helm", args...).CombinedOutput(); err == nil {
			t.Fatal("enabled connector accepted incomplete/insecure configuration")
		}
	}
}
