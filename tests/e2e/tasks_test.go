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

func TestTaskCompletionRetainsExecution(t *testing.T) {
	kube, _ := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "task-completion")
	ran := false
	for _, tc := range []struct {
		name     string
		exitCode int
	}{
		{"successful", 0},
		{"failed", 7},
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
			create(t, ctx, kube, definition)
			create(t, ctx, kube, instance)
			reason := "PodSucceeded"
			if tc.exitCode != 0 {
				reason = "PodFailed"
			}
			await(t, "task completion recorded", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
				finished := meta.FindStatusCondition(instance.Status.Conditions, "Finished")
				return conditionTrue(instance.Status.Conditions, "Finished", instance.Generation) && finished.Reason == reason &&
					instance.Status.Lifecycle != nil && instance.Status.Lifecycle.FinishedAt != nil && instance.Status.Lifecycle.FinishedReason == reason, err
			})
			execution := sandbox.Object()
			if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.SandboxName}, execution); err != nil {
				t.Fatal("completed task lost its Sandbox", err)
			}
			podState := &corev1.Pod{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.PodName}, podState); err != nil {
				t.Fatal("completed task lost its Pod and diagnostics", err)
			}
			if execution.GetDeletionTimestamp() != nil || podState.DeletionTimestamp != nil {
				t.Fatal("completion started automatic cleanup")
			}
			if err := kube.Delete(ctx, instance); err != nil {
				t.Fatal(err)
			}
			awaitDeleted(t, ctx, kube, instance)
			awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
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
