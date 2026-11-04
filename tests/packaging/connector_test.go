package packaging_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestConnectorChartSeparatesCredentials(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		overrides := []string{}
		if enabled {
			name = "enabled"
			overrides = []string{"--set", "connector.enabled=true", "--set", "connector.endpoint=https://backend.example/",
				"--set", "connector.id=test-cluster", "--set", "connector.credentialsSecret=credentials"}
		}
		t.Run(name, func(t *testing.T) {
			objects := renderObjects(t, overrides...)
			var operator appsv1.Deployment
			if err := yaml.Unmarshal(objects["Deployment/agentrally-operator"], &operator); err != nil {
				t.Fatal(err)
			}
			if len(operator.Spec.Template.Spec.Volumes) != 0 || len(operator.Spec.Template.Spec.Containers[0].VolumeMounts) != 0 {
				t.Fatal("Operator retains Connector credentials")
			}
			data, exists := objects["Deployment/agentrally-operator-connector"]
			if exists != enabled {
				t.Fatal("Connector Deployment does not follow enabled")
			}
			if !enabled {
				return
			}
			var relay appsv1.Deployment
			if err := yaml.Unmarshal(data, &relay); err != nil {
				t.Fatal(err)
			}
			pod := relay.Spec.Template.Spec
			if pod.ServiceAccountName == operator.Spec.Template.Spec.ServiceAccountName {
				t.Fatal("Connector shares controller privileges")
			}
			if len(pod.Volumes) != 1 || pod.Volumes[0].Secret == nil || pod.Volumes[0].Secret.SecretName != "credentials" ||
				len(pod.Containers[0].VolumeMounts) != 1 || !pod.Containers[0].VolumeMounts[0].ReadOnly {
				t.Fatal("Connector credentials are not mounted read-only")
			}
		})
	}
}
