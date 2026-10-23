//go:build e2e

package e2e_test

import (
	"fmt"
	"os"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestTaskCompletionPolicies(t *testing.T) {
	kube, _ := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "task-policies")
	data, err := os.ReadFile("../../examples/custom-template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		exitCode       int
		retainFailed   *bool
		shutdownPolicy string
	}{
		{"successful", 0, nil, "Delete"},
		{"failed-retained", 7, nil, "Delete"},
		{"failed-expired", 9, ptr.To(false), "Retain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := sandbox.ExtensionObject("SandboxTemplate")
			if err := yaml.Unmarshal(data, template); err != nil {
				t.Fatal(err)
			}
			template.SetName(tc.name)
			template.SetNamespace(group.Status.Namespace)
			podSpec, _, err := unstructured.NestedMap(template.Object, "spec", "podTemplate", "spec")
			if err != nil {
				t.Fatal(err)
			}
			podSpec["restartPolicy"] = "Never"
			container := podSpec["containers"].([]interface{})[0].(map[string]interface{})
			container["image"] = "docker.io/library/busybox:1.37.0"
			container["command"] = []interface{}{"sh", "-c", fmt.Sprintf("echo task-result; exit %d", tc.exitCode)}
			container["resources"] = map[string]interface{}{"requests": map[string]interface{}{"cpu": "10m", "memory": "16Mi"}}
			if err := unstructured.SetNestedMap(template.Object, podSpec, "spec", "podTemplate", "spec"); err != nil {
				t.Fatal(err)
			}
			create(t, ctx, kube, template)
			definition, instance := testAgent(group, tc.name, "")
			definition.Spec.Template, definition.Spec.Container = "", nil
			definition.Spec.TemplateRef = &v1.SandboxTemplateReference{Name: template.GetName()}
			instance.Spec.Lifecycle = &v1.AgentInstanceLifecycle{TTLSecondsAfterFinished: ptr.To(int32(0)), RetainFailed: tc.retainFailed, ShutdownPolicy: tc.shutdownPolicy}
			create(t, ctx, kube, definition)
			create(t, ctx, kube, instance)
			if tc.exitCode != 0 && tc.retainFailed == nil {
				await(t, "failed task retained despite zero TTL", func() (bool, error) {
					err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
					finished := meta.FindStatusCondition(instance.Status.Conditions, "Finished")
					return finished != nil && finished.Status == "True" && finished.Reason == "PodFailed" &&
						instance.Status.Lifecycle != nil && instance.Status.Lifecycle.FinishedAt != nil && instance.Status.Lifecycle.ExpiresAt == nil, err
				})
				// Policy edits must apply to the already recorded completion.
				instance.Spec.Lifecycle.RetainFailed = ptr.To(false)
				update(t, ctx, kube, instance)
			}
			if tc.shutdownPolicy == "Delete" {
				awaitDeleted(t, ctx, kube, instance)
			} else {
				await(t, "expired task retains its logical instance", func() (bool, error) {
					err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
					return conditionTrue(instance.Status.Conditions, "Expired", instance.Generation), err
				})
			}
			// Verify expiry's cleanup before explicit deletion can mask a failure.
			awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
			if tc.shutdownPolicy == "Retain" {
				if err := kube.Delete(ctx, instance); err != nil {
					t.Fatal(err)
				}
				awaitDeleted(t, ctx, kube, instance)
			}
			if err := kube.Delete(ctx, definition); err != nil {
				t.Fatal(err)
			}
			awaitDeleted(t, ctx, kube, definition)
		})
	}
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}
