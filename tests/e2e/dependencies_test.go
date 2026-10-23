//go:build e2e

package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"

	"arally/operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDependencyRecoveryAndRotation(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	group := readyGroup(t, ctx, kube, "dependency-recovery")
	definition, instance := testAgent(group, "agent", "exec sleep 3600")
	definition.Spec.Container.Env = []corev1.EnvVar{
		{Name: "CREDENTIAL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}, Key: "token"}}},
		{Name: "SETTING", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "mode"}}},
	}
	create(t, ctx, kube, definition)
	create(t, ctx, kube, instance)
	await(t, "missing dependency blocks capacity", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		ready := meta.FindStatusCondition(instance.Status.Conditions, "Ready")
		return ready != nil && ready.Reason == "DependencyNotFound" && instance.Status.Provisioning == nil, err
	})
	for _, kind := range []string{"SandboxWarmPool", "SandboxClaim"} {
		list := sandbox.ExtensionList(kind)
		if err := kube.List(ctx, list, client.InNamespace(instance.Namespace)); err != nil || len(list.Items) != 0 {
			t.Fatalf("missing credentials created %s capacity: %v", kind, err)
		}
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: instance.Namespace}, Data: map[string]string{"mode": "original"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: instance.Namespace}, Data: map[string][]byte{"token": []byte("e2e-only-credential-v1")}}
	create(t, ctx, kube, config)
	create(t, ctx, kube, secret)
	readyAgent(t, ctx, kube, instance)
	oldPod := instance.Status.PodUID
	if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "printenv", "SETTING"); got != "original" {
		t.Fatal("recovered execution did not receive the ConfigMap", got)
	}
	config.Data["mode"], secret.Data["token"] = "rotated", []byte("e2e-only-credential-v2")
	if err := kube.Update(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := kube.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	await(t, "rotation pending without replacing healthy execution", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
		applied := meta.FindStatusCondition(instance.Status.Conditions, "ConfigurationApplied")
		return applied != nil && applied.Reason == "RestartRequired" && conditionTrue(instance.Status.Conditions, "Available", instance.Generation) && instance.Status.PodUID == oldPod, err
	})
	if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "printenv", "SETTING"); got != "original" {
		t.Fatal("rotation changed a running process environment", got)
	}
	instance.Spec.RestartToken = "rotate-credentials"
	update(t, ctx, kube, instance)
	readyAgent(t, ctx, kube, instance)
	if instance.Status.PodUID == oldPod {
		t.Fatal("rotation restart reused the old Pod")
	}
	for name, want := range map[string]string{"SETTING": "rotated", "CREDENTIAL": "e2e-only-credential-v2"} {
		if got := execPod(t, kubeconfig, instance.Namespace, instance.Status.PodName, "printenv", name); got != want {
			t.Fatalf("replacement did not receive rotated %s", name)
		}
	}
	encoded, err := json.Marshal(instance.Status)
	if err != nil || strings.Contains(string(encoded), "e2e-only-credential-") {
		t.Fatal("status exposed credential contents", err)
	}
	if err := kube.Delete(ctx, instance); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, instance)
	awaitExecutionRemoved(t, ctx, kube, instance.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}
