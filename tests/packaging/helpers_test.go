package packaging_test

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentrally/operator/internal/testutil"
)

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
	err := filepath.WalkDir(filepath.Join(stage, "chart"), func(path string, entry fs.DirEntry, err error) error {
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
	return result
}

func renderObjects(t *testing.T, overrides ...string) map[string][]byte {
	t.Helper()
	return testutil.RenderChart(t, "../../chart", overrides...)
}
