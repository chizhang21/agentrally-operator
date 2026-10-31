//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestConnectorOnlyTraffic(t *testing.T) {
	if os.Getenv("E2E_CNI") != "cilium" {
		t.Skip("requires E2E_CNI=cilium")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	group := readyGroup(t, ctx, kube, "connector-ingress")
	definition, agent := testAgent(group, "server", "mkdir -p /tmp/www; printf probe-ok > /tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www")
	create(t, ctx, kube, definition)
	create(t, ctx, kube, agent)
	readyAgent(t, ctx, kube, agent)
	server := &corev1.Pod{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Status.PodName}, server); err != nil {
		t.Fatal(err)
	}
	group.Spec.InvocationAccess = &v1.InvocationAccess{Mode: "ConnectorOnly"}
	update(t, ctx, kube, group)
	await(t, "Connector ingress configured", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "InvocationAccessReady", group.Generation), err
	})
	condition := meta.FindStatusCondition(group.Status.Conditions, "IngressEnforcementVerified")
	if condition == nil || condition.Status != metav1.ConditionUnknown {
		t.Fatal("configuration claimed traffic enforcement", condition)
	}
	ran := false
	for _, scenario := range []struct {
		name, namespace string
		matching        bool
		want            string
	}{
		{"operator-identity", "agentrally-system", true, "probe-ok"},
		{"wrong-pod", "agentrally-system", false, "denied"},
		{"wrong-namespace", group.Status.Namespace, true, "denied"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ran = true
			labels := map[string]string{"app.kubernetes.io/name": "unrelated"}
			if scenario.matching {
				labels = map[string]string{"app.kubernetes.io/name": "agentrally-operator", "app.kubernetes.io/instance": "agentrally-operator", "app.kubernetes.io/component": "manager"}
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe-" + scenario.name, Namespace: scenario.namespace, Labels: labels}, Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(false), SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{Name: "probe", Image: "docker.io/library/busybox:1.37.0", Command: []string{"sleep", "3600"}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}}}}
			create(t, ctx, kube, pod)
			t.Cleanup(func() { _ = kube.Delete(ctx, pod) })
			await(t, "probe ready", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(pod), pod)
				return pod.Status.Phase == corev1.PodRunning, err
			})
			matches := 0
			await(t, "traffic "+scenario.want, func() (bool, error) {
				probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				defer cancel()
				command := exec.CommandContext(probeCtx, "kubectl", "--kubeconfig", kubeconfig, "-n", pod.Namespace, "exec", pod.Name, "--", "sh", "-c",
					`if wget -q -T 1 -O /tmp/response "$1" 2>/dev/null; then cat /tmp/response; else printf denied; fi`, "probe", "http://"+server.Status.PodIP+":8080/")
				out, err := command.CombinedOutput()
				if err != nil {
					return false, fmt.Errorf("probe exec: %w: %s", err, out)
				}
				if strings.TrimSpace(string(out)) == scenario.want {
					matches++
				} else {
					matches = 0
				}
				return matches >= 3, nil
			})
		})
	}
	if err := kube.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, agent)
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
	if !ran {
		t.Skip("no traffic scenarios selected")
	}
}
