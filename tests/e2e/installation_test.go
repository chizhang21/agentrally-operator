//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestChartLifecycle(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	if err := apiextensionsv1.AddToScheme(kube.Scheme()); err != nil {
		t.Fatal(err)
	}
	helm := func(t *testing.T, args ...string) []byte {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		command := exec.CommandContext(commandCtx, "helm", append([]string{"--kubeconfig", kubeconfig, "-n", "agentrally-system"}, args...)...)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("helm %s: %v\n%s", args[0], err, out)
		}
		return out
	}
	directory := t.TempDir()
	values := filepath.Join(directory, "values.yaml")
	if err := os.WriteFile(values, helm(t, "get", "values", "agentrally-operator", "-o", "yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	group := readyGroup(t, ctx, kube, "chart-retained")
	t.Cleanup(func() {
		if err := client.IgnoreNotFound(kube.Delete(ctx, group)); err != nil {
			t.Error(err)
		}
	})
	original := os.Getenv("E2E_CHART")
	if original == "" {
		original = "chart"
	}
	if !filepath.IsAbs(original) {
		original = filepath.Join("../..", original)
	}
	t.Cleanup(func() {
		helm(t, "upgrade", "--install", "agentrally-operator", original, "-f", values, "--wait", "--timeout=180s")
	})
	crd := &apiextensionsv1.CustomResourceDefinition{}
	key := client.ObjectKey{Name: "agentinstances.arally.io"}
	if err := kube.Get(ctx, key, crd); err != nil {
		t.Fatal(err)
	}
	uid := crd.UID

	t.Run("upgrade-updates-schema-in-place", func(t *testing.T) {
		chart := filepath.Join(directory, "chart")
		if err := os.CopyFS(chart, os.DirFS("../../chart")); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(chart, "templates/crds/arally.io_agentinstances.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		header, _, found := strings.Cut(string(data), "\nspec:\n")
		if !found {
			t.Fatal("CRD template has no spec")
		}
		const description = "E2E schema upgrade"
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = description
		data, err = yaml.Marshal(struct {
			Spec apiextensionsv1.CustomResourceDefinitionSpec `json:"spec"`
		}{Spec: crd.Spec})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append([]byte(header+"\n"), data...), 0644); err != nil {
			t.Fatal(err)
		}
		helm(t, "upgrade", "agentrally-operator", chart, "-f", values, "--wait", "--timeout=180s")
		if err := kube.Get(ctx, key, crd); err != nil || crd.UID != uid || crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description != description {
			t.Fatalf("CRD upgrade lost its identity or new schema: %v", err)
		}
	})
	t.Run("uninstall-retains-crds-and-agents", func(t *testing.T) {
		helm(t, "uninstall", "agentrally-operator")
		if err := kube.Get(ctx, key, crd); err != nil || crd.UID != uid {
			t.Fatalf("uninstall removed the CRD: %v", err)
		}
		retained := &v1.AgentGroup{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(group), retained); err != nil || retained.UID != group.UID {
			t.Fatalf("uninstall removed the existing AgentGroup: %v", err)
		}
	})
}
