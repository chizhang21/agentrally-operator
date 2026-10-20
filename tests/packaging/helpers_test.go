package packaging_test

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Isolate packaging tests from developer outputs and generated source files.
func packageStage(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	for _, name := range []string{"chart", "hack", "mk", "Makefile", "go.mod", "go.sum"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(stage, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0755)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return stage, filepath.Join(root, ".cache", "tools")
}

func runMake(t *testing.T, stage string, args ...string) []byte {
	t.Helper()
	command := exec.Command("make", append([]string{"--no-print-directory", "-s"}, args...)...)
	command.Dir = stage
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "VERSION", "IMG", "IMAGE_TAG_BASE", "CHART_OCI_REPO", "MAKEFLAGS", "MFLAGS", "MAKELEVEL":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("make %v: %v\n%s", args, err, out)
	}
	return out
}

func sourceSnapshot(t *testing.T, stage string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	for _, name := range []string{"chart", "hack", "mk", "Makefile", "go.mod", "go.sum"} {
		err := filepath.WalkDir(filepath.Join(stage, name), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[path] = sha256.Sum256(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}
