package packaging_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentrally/operator/internal/testutil"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestChartPackagePreservesVersionImageAndSources(t *testing.T) {
	stage := testutil.PackageStage(t)
	before := sourceSnapshot(t, stage)
	const version = "1.2.3-rc.1"
	image := "registry.example.com:5000/operator@sha256:" + strings.Repeat("a", 64)
	runMake(t, stage, "chart-package", "VERSION="+version, "IMG="+image)
	archive := filepath.Join(stage, "dist", "agentrally-operator-"+version+".tgz")
	out, err := exec.Command("helm", "show", "chart", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("chart metadata: %v\n%s", err, out)
	}
	var metadata struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(out, &metadata); err != nil || metadata.Version != version || metadata.AppVersion != version {
		t.Fatalf("selected version was not packaged: %+v (%v)", metadata, err)
	}
	objects := testutil.RenderChart(t, archive)
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(objects["Deployment/agentrally-operator"], &deployment); err != nil {
		t.Fatal(err)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 || deployment.Spec.Template.Spec.Containers[0].Image != image {
		t.Fatal("packaging lost the selected image digest")
	}
	if !reflect.DeepEqual(before, sourceSnapshot(t, stage)) {
		t.Fatal("packaging modified chart sources")
	}
}

func TestChartPackageExcludesLocalCredentials(t *testing.T) {
	stage := testutil.PackageStage(t)
	const marker = "AUDIT_ONLY_NOT_A_REAL_SECRET"
	for _, name := range []string{".env", "client.kubeconfig", "values.dev.private.yaml", "secrets/token"} {
		path := filepath.Join(stage, "chart", name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runMake(t, stage, "chart-package", "VERSION=1.2.3", "IMG=registry.example.com/operator:v1.2.3")
	file, err := os.Open(filepath.Join(stage, "dist", "agentrally-operator-1.2.3.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compressed.Close() }()
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), marker) {
			t.Fatalf("local credentials included in chart: %s", header.Name)
		}
	}
}
