package packaging_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	yaml "sigs.k8s.io/yaml/goyaml.v3"
)

type workflowContract struct {
	On map[string]struct {
		Tags []string `yaml:"tags"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Uses        string            `yaml:"uses"`
		If          string            `yaml:"if"`
		Needs       []string          `yaml:"needs"`
		With        map[string]string `yaml:"with"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflowContract {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/" + name + ".yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowContract
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestReleasePublishesBuiltArtifactsWithoutWaitingForValidation(t *testing.T) {
	release := readWorkflow(t, "release")
	if len(release.On) != 1 || !slices.Equal(release.On["push"].Tags, []string{"v*"}) {
		t.Fatal("Operator publication must start from version-tag pushes")
	}
	publish := release.Jobs["publish"]
	if !slices.Equal(publish.Needs, []string{"packages"}) {
		t.Fatal("publication must follow packaging without waiting for validation", publish.Needs)
	}
	e2e := release.Jobs["e2e"]
	for _, gate := range []string{"checks", "security", "packages"} {
		if !slices.Contains(e2e.Needs, gate) {
			t.Fatalf("release E2E can run without %s", gate)
		}
	}
	if e2e.With["image"] == "" || e2e.With["image"] != publish.With["image"] {
		t.Fatal("E2E and publication do not select the same image")
	}
	artifacts := map[string]bool{}
	for _, job := range readWorkflow(t, "packages").Jobs {
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				name, _ := step.With["name"].(string)
				artifacts[name] = true
			}
		}
	}
	wanted := map[string]bool{}
	for _, input := range []string{"image_artifact", "chart_artifact"} {
		name := e2e.With[input]
		if name == "" || !artifacts[name] {
			t.Fatalf("E2E does not consume the built %s", input)
		}
		wanted[name] = true
	}
	for _, step := range readWorkflow(t, "publish").Jobs["publish"].Steps {
		if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
			name, _ := step.With["name"].(string)
			if !wanted[name] {
				t.Fatalf("publication and E2E select different built artifacts: %s", name)
			}
			delete(wanted, name)
		}
	}
	if len(wanted) != 0 {
		t.Fatal("publication omits built package artifacts", wanted)
	}
}

func TestOperatorValidationCannotPublishPackages(t *testing.T) {
	for _, name := range []string{"ci", "checks", "security", "packages", "e2e"} {
		t.Run(name, func(t *testing.T) {
			workflow := readWorkflow(t, name)
			if workflow.Permissions["packages"] == "write" {
				t.Fatal("validation workflow grants registry write access")
			}
			for _, job := range workflow.Jobs {
				if job.Permissions["packages"] == "write" || job.Uses == "./.github/workflows/publish.yml" {
					t.Fatal("validation can invoke package publication")
				}
				for _, step := range job.Steps {
					if strings.HasPrefix(step.Uses, "docker/login-action@") {
						t.Fatal("validation attempts a registry login")
					}
					if strings.HasPrefix(step.Uses, "docker/build-push-action@") && step.With["push"] != false {
						t.Fatal("validation attempts to push its image")
					}
				}
			}
		})
	}
	publish := readWorkflow(t, "publish").Jobs["publish"]
	if publish.Permissions["packages"] != "write" || publish.If != "github.event_name == 'push' && github.event.deleted != true && startsWith(github.ref, 'refs/tags/v')" {
		t.Fatal("publication must grant package writes only for a version-tag push")
	}
}
