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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

func TestChartExcludesLocalCredentials(t *testing.T) {
	stage, _ := packageStage(t)
	const marker = "AUDIT_ONLY_NOT_A_REAL_SECRET"
	for _, name := range []string{".env", ".env.dev", "client.kubeconfig", "client.kubeconfig.bak", "values.dev.private.yaml", "values.dev.local.yaml", "secrets/token", "templates/.env"} {
		path := filepath.Join(stage, "chart", name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := buildVersionedChart(t, stage, "1.2.3")
	file, err := os.Open(filepath.Join(stage, "dist", "arally-operator-"+version+".tgz"))
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

// Packaging must use the selected version and image without changing sources.
func TestVersionedCharts(t *testing.T) {
	stage, _ := packageStage(t)
	for _, version := range []string{"", "0.0.0-pr.123.gaaaaaaaaaaaa", "1.2.3-rc.1", "1.2.3", "1.2.3-" + strings.Repeat("a", 40) + ".rc1"} {
		t.Run("version="+version, func(t *testing.T) {
			before := sourceSnapshot(t, stage)
			actual := buildVersionedChart(t, stage, version)
			archive := filepath.Join(stage, "dist", "arally-operator-"+actual+".tgz")
			verifyChartVersion(t, archive, actual)
			verifyPackagedResources(t, archive, "registry.example.com:5000/arally-operator:v"+actual)
			if !reflect.DeepEqual(before, sourceSnapshot(t, stage)) {
				t.Fatal("packaging modified source files")
			}
		})
	}
}

func TestChartReusesSavedImage(t *testing.T) {
	stage, _ := packageStage(t)
	before := sourceSnapshot(t, stage)
	for _, arguments := range [][]string{
		{"version", "VERSION=1.2.3", "IMG=registry.example.com:5000/operator:custom"},
		{"chart-package"},
	} {
		runMake(t, stage, arguments...)
	}
	archive := filepath.Join(stage, "dist", "arally-operator-1.2.3.tgz")
	verifyChartVersion(t, archive, "1.2.3")
	verifyPackagedResources(t, archive, "registry.example.com:5000/operator:custom")
	if !reflect.DeepEqual(before, sourceSnapshot(t, stage)) {
		t.Fatal("packaging modified source files")
	}
}

func buildVersionedChart(t *testing.T, stage, version string) string {
	t.Helper()
	args := []string{"version", "chart-package", "IMAGE_TAG_BASE=registry.example.com:5000/arally-operator"}
	if version != "" {
		args = append(args, "VERSION="+version)
	}
	out := runMake(t, stage, args...)
	actual, _, _ := strings.Cut(string(out), "\n")
	if version != "" && actual != version {
		t.Fatalf("package version %q, want %q", actual, version)
	}
	if version == "" && actual != "0.0.0-dev" {
		t.Fatalf("local version is not the fixed dev version: %q", actual)
	}
	return actual
}

func verifyChartVersion(t *testing.T, archive, version string) {
	t.Helper()
	out, err := exec.Command("helm", "show", "chart", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("chart metadata: %v\n%s", err, out)
	}
	var chart struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(out, &chart); err != nil || chart.Version != version || chart.AppVersion != version {
		t.Fatalf("chart version differs from selected version: %+v (%v)", chart, err)
	}
	var deployment appsv1.Deployment
	objects := renderChartObjects(t, archive)
	if err := yaml.Unmarshal(objects["Deployment/arally-operator"], &deployment); err != nil {
		t.Fatal(err)
	}
	for key, value := range deployment.Spec.Template.Labels {
		if errors := validation.IsValidLabelValue(value); len(errors) > 0 {
			t.Fatalf("invalid %s label %q: %v", key, value, errors)
		}
	}
	if deployment.Spec.Template.Labels["app.kubernetes.io/version"] != version {
		t.Fatal("Pod version label differs from chart version")
	}
	policy := corev1.PullIfNotPresent
	if version == "0.0.0-dev" {
		policy = corev1.PullAlways
	}
	if deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy != policy {
		t.Fatalf("image pull policy differs for %s", version)
	}
}

func verifyPackagedResources(t *testing.T, archive, image string) {
	t.Helper()
	objects := renderChartObjects(t, archive)
	crds := 0
	for key := range objects {
		if strings.HasPrefix(key, "CustomResourceDefinition/") {
			crds++
		}
	}
	if crds != 4 {
		t.Fatalf("chart archive has %d CRDs, want 4", crds)
	}
	if _, ok := objects["ClusterCapabilities/cluster"]; ok {
		t.Fatal("report bootstrap must wait for CRD registration in the controller")
	}
	if _, ok := objects["ClusterRole/arally-operator-arally-system"]; !ok {
		t.Fatal("chart archive omitted manager RBAC")
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(objects["Deployment/arally-operator"], &deployment); err != nil {
		t.Fatal(err)
	}
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Image != image {
		t.Fatalf("chart lost selected image reference: %+v", containers)
	}
}

// Tags and digests must survive chart packaging without changing source defaults.
func TestChartBuildPreservesSourceConfiguration(t *testing.T) {
	stage, _ := packageStage(t)
	before := sourceSnapshot(t, stage)
	for _, image := range []string{"registry.example.com/arally-operator:custom", "registry.example.com/arally-operator@sha256:" + strings.Repeat("a", 64)} {
		command := exec.Command("bash", "hack/package-chart.sh", image, filepath.Join(stage, "dist"))
		command.Dir = stage
		command.Env = append(os.Environ(), "VERSION=1.2.3")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("chart package: %v\n%s", err, out)
		}
		archive := filepath.Join(stage, "dist", "arally-operator-1.2.3.tgz")
		verifyPackagedResources(t, archive, image)
	}
	if !reflect.DeepEqual(before, sourceSnapshot(t, stage)) {
		t.Fatal("packaging modified source files")
	}
}
