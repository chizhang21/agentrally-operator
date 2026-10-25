//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestOpenClawLifecycle(t *testing.T) {
	if os.Getenv("E2E_OPENCLAW") != "true" {
		t.Skip("enable with E2E_OPENCLAW=true make e2e")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := context.Background()
	load := func(name string, object client.Object) {
		t.Helper()
		data, err := os.ReadFile("../../examples/openclaw/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.UnmarshalStrict(data, object); err != nil {
			t.Fatal(err)
		}
	}
	group, definition, instance, pvc := &v1.AgentGroup{}, &v1.AgentDefinition{}, &v1.AgentInstance{}, &corev1.PersistentVolumeClaim{}
	load("group.yaml", group)
	load("definition.yaml", definition)
	load("instance.yaml", instance)
	load("workspace.yaml", pvc)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(tokenBytes)
	t.Cleanup(func() {
		if t.Failed() && instance.Status.PodName != "" {
			out, _ := exec.Command("kubectl", "--kubeconfig", kubeconfig, "-n", instance.Namespace, "logs", instance.Status.PodName, "--all-containers", "--tail=50").CombinedOutput()
			t.Log("OpenClaw logs:", strings.ReplaceAll(string(out), token, "<redacted>"))
		}
	})
	create(t, ctx, kube, group)
	await(t, "OpenClaw Group readiness", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	create(t, ctx, kube, pvc)
	create(t, ctx, kube, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alice-gateway", Namespace: instance.Namespace}, Data: map[string][]byte{"token": []byte(token)}})
	create(t, ctx, kube, definition)
	create(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	t.Log("OpenClaw image:", definition.Spec.Container.Image)
	t.Log("OpenClaw version:", execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "/app/dist/index.js", "--version"))
	health := func() {
		t.Helper()
		out := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "-e", `(async()=>{for(const path of ['/healthz','/startupz']){const r=await fetch('http://127.0.0.1:18789'+path);if(r.status!==200)throw Error(path+': '+r.status);console.log(path+' '+r.status)}})().catch(e=>{console.error(e.message);process.exit(1)})`)
		t.Log("HTTP probes:", out)
		out = execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "/app/dist/index.js", "health", "--json", "--timeout", "10000")
		var report struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal([]byte(out), &report); err != nil || !report.OK {
			t.Fatalf("authenticated OpenClaw health RPC failed: %v: %s", err, out)
		}
		t.Log("Authenticated health RPC: ok")
	}
	health()
	// Persist actual OpenClaw scheduler state without model calls or delivery.
	jobJSON := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "/app/dist/index.js", "cron", "add", "--name", "operator-retention", "--every", "1h", "--command", "printf operator-retention", "--disabled", "--no-deliver", "--json")
	var job struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(jobJSON), &job); err != nil || job.ID == "" || job.Enabled {
		t.Fatalf("create disabled OpenClaw automation: %v: %s", err, jobJSON)
	}
	t.Log("Disabled OpenClaw automation created")
	canary := `const fs=require('node:fs');for(const dir of ['/workspace/.openclaw/workspace','/workspace/.config/openclaw']){fs.mkdirSync(dir,{recursive:true});fs.writeFileSync(dir+'/operator-canary','retained-openclaw-state\n')}console.log('canaries written')`
	t.Log(execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "-e", canary))
	retained := func() {
		t.Helper()
		out := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "-e", `const fs=require('node:fs');for(const dir of ['/workspace/.openclaw/workspace','/workspace/.config/openclaw']){if(fs.readFileSync(dir+'/operator-canary','utf8')!=='retained-openclaw-state\n')throw Error('state changed')}const cfg=JSON.parse(fs.readFileSync(process.env.OPENCLAW_CONFIG_PATH,'utf8'));if(cfg.gateway.mode!=='local'||cfg.gateway.controlUi.enabled!==false)throw Error('configuration changed');console.log('state, credentials directory and config retained')`)
		if err := kube.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
			t.Fatal(err)
		}
		if instance.Status.Workspace == nil || instance.Status.Workspace.UID != pvc.UID || len(pvc.OwnerReferences) != 0 {
			t.Fatal("retained workspace identity or ownership changed")
		}
		t.Log(out)
		jobsJSON := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "/app/dist/index.js", "cron", "list", "--all", "--json")
		var list struct {
			Jobs []struct {
				ID      string `json:"id"`
				Enabled bool   `json:"enabled"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(jobsJSON), &list); err != nil {
			t.Fatal("list retained OpenClaw automations", err)
		}
		found := false
		for _, current := range list.Jobs {
			if current.ID == job.ID && !current.Enabled {
				found = true
			}
		}
		if !found {
			t.Fatal("OpenClaw scheduler state was not retained")
		}
		t.Log("OpenClaw scheduler job retained with the same ID and disabled state")
	}
	pvcUID, firstPod, firstPodName, firstSandbox := pvc.UID, instance.Status.PodUID, instance.Status.PodName, instance.Status.SandboxName
	instance.Spec.OperatingMode = sandbox.Suspended
	update(t, ctx, kube, instance)
	await(t, "OpenClaw suspension", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
	})
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: firstPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("suspended OpenClaw still has a Pod", err)
	}
	instance.Spec.OperatingMode = sandbox.Running
	update(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	if instance.Status.PodUID == firstPod || instance.Status.SandboxName != firstSandbox {
		t.Fatal("resume did not replace only the Pod")
	}
	health()
	retained()
	t.Log("Suspend/resume: new Pod, retained Sandbox/PVC/files")
	previousRevision, previousPod := instance.Status.Definition.Revision, instance.Status.PodUID
	definition.Spec.Container.Env = append(definition.Spec.Container.Env, corev1.EnvVar{Name: "AGENTRALLY_OPENCLAW_E2E_REVISION", Value: "v2"})
	update(t, ctx, kube, definition)
	await(t, "OpenClaw definition publication", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition)
		return definition.Status.CurrentRevision != previousRevision && conditionTrue(definition.Status.Conditions, "Ready", definition.Generation), err
	})
	if err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
	if instance.Status.Definition.Revision != previousRevision || instance.Status.PodUID != previousPod {
		t.Fatal("publication replaced the pinned OpenClaw execution")
	}
	revision := func() string {
		return execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "node", "-e", `console.log(process.env.AGENTRALLY_OPENCLAW_E2E_REVISION||'v1')`)
	}
	if revision() != "v1" {
		t.Fatal("pinned OpenClaw adopted unpublished execution settings")
	}
	instance.Spec.RestartToken = "openclaw-config-v2"
	update(t, ctx, kube, instance)
	await(t, "OpenClaw configuration replacement", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		return instance.Status.Definition != nil && instance.Status.Definition.Revision == definition.Status.CurrentRevision && instance.Status.PodUID != previousPod && conditionTrue(instance.Status.Conditions, "Ready", instance.Generation), err
	})
	if revision() != "v2" {
		t.Fatal("replacement did not adopt the requested configuration")
	}
	health()
	retained()
	t.Log("Explicit restart: new revision and execution, retained PVC/files")
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2}
	update(t, ctx, kube, group)
	await(t, "Group prewarming policy observation", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := kube.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.Provisioning.PoolName}, pool); err != nil {
		t.Fatal(err)
	}
	if count, _, err := unstructured.NestedInt64(pool.Object, "spec", "replicas"); err != nil || count != 0 {
		t.Fatal("OpenClaw started unassigned spare assistants", count, err)
	}
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, instance)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil || pvc.UID != pvcUID || len(pvc.OwnerReferences) != 0 {
		t.Fatal("instance deletion changed external storage", err)
	}
	t.Log("Instance deletion: retained external PVC UID and ownership")
	if err := kube.Delete(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, pvc)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}
