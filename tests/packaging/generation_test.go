package packaging_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// Deleted APIs must not survive as installable CRDs in the chart.
func TestGenerationRemovesObsoleteSchemas(t *testing.T) {
	stage, tools := packageStage(t)
	generator := filepath.Join(tools, "controller-gen")
	if _, err := os.Stat(generator); err != nil {
		t.Skip("make controller-gen required")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"api/v1alpha1", "internal/controller"} {
		if err := os.MkdirAll(filepath.Join(stage, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := filepath.Glob(filepath.Join(root, "api/v1alpha1", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(root, "internal/controller/rbac.go"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, rel), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	stale := []string{"chart/templates/crds/arally.io_obsolete.yaml"}
	for _, path := range stale {
		if err := os.WriteFile(filepath.Join(stage, path), []byte("obsolete output"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	outputs := []string{"api/v1alpha1/zz_generated.deepcopy.go", "chart/templates/roles.yaml"}
	for _, path := range outputs {
		if err := os.Remove(filepath.Join(stage, path)); err != nil {
			t.Fatal(err)
		}
	}
	runMake(t, stage, "generate", "CONTROLLER_GEN="+generator, "GOWORK=off")
	for _, path := range outputs {
		if _, err := os.Stat(filepath.Join(stage, path)); err != nil {
			t.Fatalf("missing generated output: %s (%v)", path, err)
		}
	}
	for _, path := range stale {
		if _, err := os.Stat(filepath.Join(stage, path)); !os.IsNotExist(err) {
			t.Fatalf("obsolete resource survived: %s (%v)", path, err)
		}
	}
	crds, err := filepath.Glob(filepath.Join(stage, "chart/templates/crds", "*.yaml"))
	if err != nil || len(crds) != 3 {
		t.Fatalf("current APIs were lost: %v %v", crds, err)
	}
	// Checking drift must neither repair files nor remove obsolete manifests.
	if err := os.WriteFile(filepath.Join(stage, stale[0]), []byte("obsolete output"), 0644); err != nil {
		t.Fatal(err)
	}
	before := sourceSnapshot(t, stage)
	command := exec.Command("python3", "hack/verify-generated.py")
	command.Dir = stage
	command.Env = append(os.Environ(), "LOCALBIN="+tools, "CONTROLLER_GEN="+generator, "GOWORK=off")
	if out, err := command.CombinedOutput(); err == nil {
		t.Fatalf("verification accepted obsolete output: %s", out)
	}
	if !reflect.DeepEqual(before, sourceSnapshot(t, stage)) {
		t.Fatal("verification modified generated sources")
	}
}
