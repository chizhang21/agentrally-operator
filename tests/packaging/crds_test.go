package packaging_test

import (
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// CRDs must have the same schema on install and upgrade and remain protected
// against uninstall/removal deletion. API tests validate the rendered schemas.
func TestHelmManagesCRDs(t *testing.T) {
	installed := renderObjects(t)
	upgraded := renderObjects(t, "--is-upgrade")
	for key, data := range installed {
		if !strings.HasPrefix(key, "CustomResourceDefinition/") {
			continue
		}
		t.Run(strings.TrimPrefix(key, "CustomResourceDefinition/"), func(t *testing.T) {
			var original, actual apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &original); err != nil {
				t.Fatal(err)
			}
			if _, exists := upgraded[key]; !exists {
				t.Fatalf("%s omitted from Helm upgrade manifest", key)
			}
			if err := yaml.Unmarshal(upgraded[key], &actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.Spec, original.Spec) {
				t.Fatal("CRD schema differs between install and upgrade")
			}
			for _, crd := range []*apiextensionsv1.CustomResourceDefinition{&original, &actual} {
				if crd.Annotations["helm.sh/resource-policy"] != "keep" || crd.Annotations["helm.sh/hook"] != "" {
					t.Fatal("CRD must be retained and managed without hooks")
				}
				if crd.Labels["app.kubernetes.io/managed-by"] != "Helm" || crd.Annotations["meta.helm.sh/release-name"] != "agentrally-operator" || crd.Annotations["meta.helm.sh/release-namespace"] != "agentrally-system" {
					t.Fatal("CRD release ownership missing")
				}
			}
		})
	}
	for _, objects := range []map[string][]byte{installed, upgraded} {
		if _, exists := objects["ClusterCapabilities/cluster"]; exists {
			t.Fatal("chart submits a report before its CRD is registered")
		}
	}
}
