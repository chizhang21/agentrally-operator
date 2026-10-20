package packaging_test

import (
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// CRDs must have the same schema on install and upgrade and remain protected
// against uninstall/removal deletion. API tests validate the rendered schemas.
func TestHelmManagesCRDs(t *testing.T) {
	installed := renderObjects(t)
	for _, mode := range []string{"install", "upgrade"} {
		t.Run(mode, func(t *testing.T) {
			objects := installed
			if mode == "upgrade" {
				objects = renderObjects(t, "--is-upgrade")
			}
			for _, plural := range []string{"agentgroups", "agentclaims", "clustercapabilities"} {
				name := plural + ".arally.io"
				data, exists := objects["CustomResourceDefinition/"+name]
				if !exists {
					t.Fatalf("%s omitted from Helm %s manifest", name, mode)
				}
				var actual apiextensionsv1.CustomResourceDefinition
				if err := yaml.Unmarshal(data, &actual); err != nil {
					t.Fatal(err)
				}
				var original apiextensionsv1.CustomResourceDefinition
				if err := yaml.Unmarshal(installed["CustomResourceDefinition/"+name], &original); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual.Spec, original.Spec) {
					t.Fatalf("CRD schema differs between install and upgrade: %s", name)
				}
				if actual.Annotations["helm.sh/resource-policy"] != "keep" || actual.Annotations["helm.sh/hook"] != "" {
					t.Fatalf("CRD must be retained and managed without hooks: %s", name)
				}
				if actual.Labels["app.kubernetes.io/managed-by"] != "Helm" || actual.Annotations["meta.helm.sh/release-name"] != "arally-operator" || actual.Annotations["meta.helm.sh/release-namespace"] != "arally-system" {
					t.Fatalf("CRD release ownership missing: %s", name)
				}
			}
			if _, exists := objects["ClusterCapabilities/cluster"]; exists {
				t.Fatal("fresh install submits a report before its CRD is registered")
			}
		})
	}
}
