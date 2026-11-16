//go:build e2e

package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These are the published YAML files, not a separately maintained approximation.
func TestPublishedExamples(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	kubectl(t, kubeconfig, "apply", "-f", "../../examples/02-group.yaml")
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	await(t, "example Group", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	kubectl(t, kubeconfig, "apply", "-f", "../../examples/03-definition.yaml", "-f", "../../examples/04-instance-basic.yaml")
	basic := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: group.Status.Namespace}}
	readyAgent(t, ctx, kube, basic)
	verifyDemoResponse(t, kubeconfig, basic)
	suspendAgent(t, kube, basic)
	basic.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, basic)
	readyAgent(t, ctx, kube, basic)
	verifyDemoResponse(t, kubeconfig, basic)
	oldRevision, basicPod := basic.Status.Definition.Revision, basic.Status.PodUID

	kubectl(t, kubeconfig, "apply", "-f", "../../examples/04-instance-full.yaml")
	full := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-full", Namespace: group.Status.Namespace}}
	readyAgent(t, ctx, kube, full)
	pvc := allocatedWorkspace(t, ctx, kube, full)
	instanceUID, claimUID, sandboxUID := full.UID, full.Status.Provisioning.ClaimUID, full.Status.SandboxUID
	execPod(t, kubeconfig, full.Namespace, full.Status.PodName, "python", "-c", "from pathlib import Path; Path('/workspace/session').write_text('example-data')")
	verifyWorkspace := func() {
		t.Helper()
		if full.UID != instanceUID || full.Status.Provisioning.ClaimUID != claimUID || full.Status.SandboxUID != sandboxUID || allocatedWorkspace(t, ctx, kube, full).UID != pvc.UID {
			t.Fatal("example lifecycle replaced the persistent allocation")
		}
		if got := execPod(t, kubeconfig, full.Namespace, full.Status.PodName, "python", "-c", "from pathlib import Path; p=Path('/workspace/session'); value=p.read_text(); p.write_text(value); print(value)"); got != "example-data" {
			t.Fatal("example workspace lost read/write access", got)
		}
		verifyDemoResponse(t, kubeconfig, full)
	}
	verifyWorkspace()
	suspendAgent(t, kube, full)
	full.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, full)
	readyAgent(t, ctx, kube, full)
	verifyWorkspace()
	previousPod := full.Status.PodUID
	full.Spec.RestartToken = "example-restart"
	update(t, ctx, kube, full)
	readyAgent(t, ctx, kube, full)
	if full.Status.PodUID == previousPod {
		t.Fatal("restart did not replace the process")
	}
	verifyWorkspace()

	definition := &v1.AgentDefinition{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: full.Namespace, Name: full.Spec.DefinitionRef.Name}, definition); err != nil {
		t.Fatal(err)
	}
	definition.Spec.Container.Env = append(definition.Spec.Container.Env, corev1.EnvVar{Name: "EXAMPLE_REVISION", Value: "updated"})
	update(t, ctx, kube, definition)
	await(t, "example revision publication", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != oldRevision && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	full.Spec.Revision = definition.Status.CurrentRevision
	update(t, ctx, kube, full)
	readyAgent(t, ctx, kube, full)
	if full.Status.Definition.Revision != definition.Status.CurrentRevision || execPod(t, kubeconfig, full.Namespace, full.Status.PodName, "python", "-c", "import os; print(os.environ['EXAMPLE_REVISION'])") != "updated" {
		t.Fatal("example did not execute the requested revision")
	}
	verifyWorkspace()
	if err := kube.Get(ctx, client.ObjectKeyFromObject(basic), basic); err != nil {
		t.Fatal(err)
	}
	if basic.Status.Definition.Revision != oldRevision || basic.Status.PodUID != basicPod {
		t.Fatal("updating full affected basic")
	}

	for _, instance := range []*v1.AgentInstance{basic, full} {
		if err := kube.Delete(ctx, instance); err != nil {
			t.Fatal(err)
		}
		awaitDeleted(t, ctx, kube, instance)
	}
	awaitDeleted(t, ctx, kube, pvc)
	awaitExecutionRemoved(t, ctx, kube, group.Status.Namespace)
	if err := kube.Delete(ctx, definition); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, definition)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func suspendAgent(t *testing.T, kube client.Client, instance *v1.AgentInstance) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace, Name: instance.Status.PodName}}
	instance.Spec.OperatingMode = sandbox.Suspended
	update(t, t.Context(), kube, instance)
	await(t, "instance suspended", func() (bool, error) {
		err := kube.Get(t.Context(), client.ObjectKeyFromObject(instance), instance)
		return conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
	})
	awaitDeleted(t, t.Context(), kube, pod)
}

func verifyDemoResponse(t *testing.T, kubeconfig string, instance *v1.AgentInstance) {
	t.Helper()
	body := agentHTTP(t, kubeconfig, instance, "POST", "/chat", `{"message":"Hello"}`)
	var response struct{ Response, Mode string }
	if err := json.Unmarshal([]byte(body), &response); err != nil || response.Mode != "demo" || response.Response != "AgentRally demo agent is ready." {
		t.Fatalf("unexpected demo reply: %s (%v)", body, err)
	}
}

// Use the documented port-forward access path and verify application content.
func agentHTTP(t *testing.T, kubeconfig string, instance *v1.AgentInstance, method, path, body string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", instance.Namespace, "port-forward", "pod/"+instance.Status.PodName, ":8080", "--address=127.0.0.1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "Forwarding from 127.0.0.1:") {
			continue
		}
		address := strings.Fields(line)[2]
		request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("Agent HTTP %s: %s (%v)", response.Status, data, err)
		}
		return strings.TrimSpace(string(data))
	}
	t.Fatal("Agent port-forward did not start", scanner.Err())
	return ""
}
