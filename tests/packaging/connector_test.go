package packaging_test

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestConnectorChartWiresFlagsAndCredentials(t *testing.T) {
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
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(objects["Deployment/agentrally-operator"], &deployment); err != nil {
				t.Fatal(err)
			}
			pod := deployment.Spec.Template.Spec
			if len(pod.Containers) != 1 {
				t.Fatal("Connector must run inside the Operator process")
			}
			container := pod.Containers[0]
			if !enabled {
				if len(pod.Volumes) != 0 || slices.Contains(container.Args, "--connector-enabled=true") {
					t.Fatal("disabled Connector retains its credentials or startup flag")
				}
				return
			}
			for _, flag := range []string{"--connector-enabled=true", "--connector-endpoint=https://backend.example/", "--connector-id=test-cluster"} {
				if !slices.Contains(container.Args, flag) {
					t.Fatalf("missing Connector flag: %s", flag)
				}
			}
			if len(pod.Volumes) != 1 || pod.Volumes[0].Secret == nil || pod.Volumes[0].Secret.SecretName != "credentials" || len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly {
				t.Fatal("Connector credentials are not mounted read-only")
			}
		})
	}
}
