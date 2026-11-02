//go:build e2e

package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"agentrally/operator/internal/testutil"

	"sigs.k8s.io/yaml"
)

// Exercise our attribution and deduplication rules in the real Prometheus engine.
func TestResourceUsageRules(t *testing.T) {
	_, _ = e2eClient(t)
	objects := testutil.RenderChart(t, "../../chart", "--api-versions", "monitoring.coreos.com/v1/PrometheusRule")
	var rule struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal(objects["PrometheusRule/agentrally-operator-resources"], &rule); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	data, err := yaml.Marshal(rule.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "rules.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("resource_metrics.test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "usage.test.yaml"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--network=none",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-v", directory+":/work:ro", "-w", "/work", "--entrypoint=/bin/promtool",
		"prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996",
		"test", "rules", "usage.test.yaml")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("resource attribution rules: %v\n%s", err, out)
	}
}
