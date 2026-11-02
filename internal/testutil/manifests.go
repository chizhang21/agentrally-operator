package testutil

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

// ExampleManifest records an example document and its source location.
type ExampleManifest struct {
	Path   string
	Object *unstructured.Unstructured
}

// ExampleManifests reads the repository's CR configuration examples.
func ExampleManifests(t *testing.T) []ExampleManifest {
	t.Helper()
	const root = "../../examples"
	var manifests []ExampleManifest
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for document := 1; ; document++ {
			object := &unstructured.Unstructured{}
			if err := decoder.Decode(object); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("%s document %d: %v", path, document, err)
			}
			if len(object.Object) != 0 {
				manifests = append(manifests, ExampleManifest{Path: fmt.Sprintf("%s/%d", entry.Name(), document), Object: object})
			}
		}
	}
	if len(manifests) == 0 {
		t.Fatal("manifest resources are missing")
	}
	return manifests
}
