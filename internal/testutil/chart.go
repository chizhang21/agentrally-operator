package testutil

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// RenderChart renders the source chart or a packaged archive.
func RenderChart(t *testing.T, chart string, overrides ...string) map[string][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm required")
	}
	args := append([]string{"template", "agentrally-operator", chart, "-n", "agentrally-system"}, overrides...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm: %v\n%s", err, out)
	}
	objects := map[string][]byte{}
	reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(out)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		var object struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &object); err != nil {
			t.Fatal(err)
		}
		if object.Kind != "" {
			objects[object.Kind+"/"+object.Name] = doc
		}
	}
}

// PackageStage copies build inputs into a temporary directory.
func PackageStage(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	for _, name := range []string{"chart", "hack", "mk"} {
		if err := os.CopyFS(filepath.Join(stage, name), os.DirFS(filepath.Join(root, name))); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "Makefile"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return stage
}
