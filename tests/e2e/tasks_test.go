//go:build e2e

package e2e_test

import (
	"fmt"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestTaskCompletionPolicies(t *testing.T) {
	kube, _ := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "task-policies")
	ran := false
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
			ran = true
			template := sandbox.ExtensionObject("SandboxTemplate")
			template.SetName(tc.name)
			template.SetNamespace(group.Status.Namespace)
			template.SetLabels(map[string]string{"arally.io/category": "generic"})
			pod := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
				NodeSelector:    map[string]string{corev1.LabelOSStable: "linux"},
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{
					Name: "agent", Image: "docker.io/library/busybox:1.37.0",
					Command:   []string{"sh", "-c", fmt.Sprintf("echo task-result; exit %d", tc.exitCode)},
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
				}},
			}}
			podTemplate, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
			if err != nil {
				t.Fatal(err)
			}
			template.Object["spec"] = map[string]interface{}{
				"networkPolicyManagement": "Unmanaged", "envVarsInjectionPolicy": "Disallowed",
				"volumeClaimTemplatesPolicy": "Disallowed", "service": true, "podTemplate": podTemplate,
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
	if !ran {
		t.Skip("no task scenarios selected")
	}
}
