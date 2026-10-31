//go:build e2e

package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/sandbox"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPersistentWorkspaceFoundation(t *testing.T) {
	ran := false
	for _, propagation := range []metav1.DeletionPropagation{metav1.DeletePropagationForeground, metav1.DeletePropagationBackground} {
		t.Run(string(propagation), func(t *testing.T) {
			ran = true
			ctx := context.Background()
			kube, kubeconfig := e2eClient(t)
			group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "persistent-" + strings.ToLower(string(propagation))}, Spec: v1.AgentGroupSpec{
				PersistentWorkspaces: true,
				Quota:                &corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePersistentVolumeClaims: resource.MustParse("4")}},
				LimitRange:           &corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypePersistentVolumeClaim, Max: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}},
				NetworkPolicy:        &networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}},
			}}
			create(t, ctx, kube, group)
			await(t, "persistent Group ready", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
				return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
			})
			ns := &corev1.Namespace{}
			if err := kube.Get(ctx, client.ObjectKey{Name: group.Status.Namespace}, ns); err != nil {
				t.Fatal(err)
			}
			if len(ns.OwnerReferences) != 0 || ns.UID != group.Status.NamespaceUID {
				t.Fatal("namespace not independently pinned")
			}
			definition, instance := testAgent(group, "writer", "sleep 3600")
			instance.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", Persistence: &v1.WorkspacePersistence{RetentionSeconds: 3600}, VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("32Mi")}}}}
			create(t, ctx, kube, definition)
			create(t, ctx, kube, instance)
			readyAgent(t, ctx, kube, instance)
			binding := instance.Status.Workspace.DeepCopy()
			if binding.WorkspaceUID == "" || binding.UID == "" {
				t.Fatal("workspace not pinned")
			}
			kubectl(t, kubeconfig, "-n", instance.Namespace, "exec", instance.Status.PodName, "--", "sh", "-c", "printf durable > /workspace/marker")
			if out := kubectl(t, kubeconfig, "-n", instance.Namespace, "exec", instance.Status.PodName, "--", "cat", "/workspace/marker"); !strings.Contains(out, "durable") {
				t.Fatal(out)
			}
			instance.Spec.OperatingMode = sandbox.Suspended
			update(t, ctx, kube, instance)
			await(t, "suspended", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
				return conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
			})
			instance.Spec.OperatingMode = sandbox.Running
			update(t, ctx, kube, instance)
			readyAgent(t, ctx, kube, instance)
			if *binding != *instance.Status.Workspace {
				t.Fatal("resume changed storage identity")
			}
			if out := kubectl(t, kubeconfig, "-n", instance.Namespace, "exec", instance.Status.PodName, "--", "cat", "/workspace/marker"); !strings.Contains(out, "durable") {
				t.Fatal(out)
			}
			oldPodUID := instance.Status.PodUID
			instance.Spec.RestartToken = "replace-execution"
			update(t, ctx, kube, instance)
			await(t, "replacement execution ready", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
				return instance.Status.PodUID != oldPodUID && conditionTrue(instance.Status.Conditions, "Ready", instance.Generation), err
			})
			if *binding != *instance.Status.Workspace {
				t.Fatal("replacement changed storage identity")
			}
			if out := kubectl(t, kubeconfig, "-n", instance.Namespace, "exec", instance.Status.PodName, "--", "cat", "/workspace/marker"); strings.TrimSpace(out) != "durable" {
				t.Fatal("replacement lost data", out)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(definition), definition); err != nil {
				t.Fatal(err)
			}
			template := sandbox.ExtensionObject("SandboxTemplate")
			template.SetName(definition.Status.TemplateName)
			template.SetNamespace(instance.Namespace)
			if err := kube.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil {
				t.Fatal(err)
			}
			revisionUID := template.GetUID()
			if len(template.GetOwnerReferences()) != 0 {
				t.Fatal("revision still cascades with Definition")
			}
			if err := kube.Delete(ctx, definition, client.PropagationPolicy(propagation)); err != nil {
				t.Fatal(err)
			}
			if err := kube.Delete(ctx, group, client.PropagationPolicy(propagation)); err != nil {
				t.Fatal(err)
			}
			await(t, "Group retained", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
				for _, c := range group.Status.Conditions {
					if c.Reason == "WorkspaceRetained" {
						return true, err
					}
				}
				return false, err
			})
			// Explicit stopping must continue after either parent has begun deletion.
			instance.Spec.OperatingMode = sandbox.Suspended
			update(t, ctx, kube, instance)
			await(t, "suspended during parent deletion", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance)
				return conditionTrue(instance.Status.Conditions, "Suspended", instance.Generation), err
			})
			if err := kube.Delete(ctx, instance); err != nil {
				t.Fatal(err)
			}
			awaitDeleted(t, ctx, kube, instance)
			record := &v1.AgentWorkspace{ObjectMeta: metav1.ObjectMeta{Name: binding.WorkspaceName}}
			await(t, "release pending", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(record), record)
				for _, c := range record.Status.Conditions {
					if c.Reason == "ReleasePending" {
						return true, err
					}
				}
				return false, err
			})
			pvc := &corev1.PersistentVolumeClaim{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: binding.ClaimName}, pvc); err != nil {
				t.Fatal(err)
			}
			if pvc.UID != binding.UID || len(pvc.OwnerReferences) != 0 || pvc.DeletionTimestamp != nil {
				t.Fatal("PVC identity or survival lost")
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(template), template); err != nil || template.GetUID() != revisionUID || template.GetDeletionTimestamp() != nil {
				t.Fatal("revision lost", err)
			}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil || ns.DeletionTimestamp != nil {
				t.Fatal("namespace lost", err)
			}
			for _, policy := range []client.Object{
				&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "agentrally-quota", Namespace: ns.Name}},
				&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "agentrally-limits", Namespace: ns.Name}},
				&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "agentrally-group-policy", Namespace: ns.Name}},
			} {
				if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
					t.Fatal(err)
				}
				owner := metav1.GetControllerOf(policy)
				if policy.GetDeletionTimestamp() != nil || owner == nil || owner.UID != ns.UID || owner.APIVersion != "v1" || (owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion) {
					t.Fatalf("namespace policy lost: %+v", policy)
				}
			}
			if err := kube.Delete(ctx, record, client.PropagationPolicy(propagation)); err != nil {
				t.Fatal(err)
			}
			await(t, "workspace delete blocked", func() (bool, error) {
				err := kube.Get(ctx, client.ObjectKeyFromObject(record), record)
				for _, c := range record.Status.Conditions {
					if c.Reason == "DeletionBlocked" {
						return true, err
					}
				}
				return false, err
			})
			if err := kube.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil || pvc.DeletionTimestamp != nil {
				t.Fatal("record deletion cascaded into PVC", err)
			}
			// The disposable cluster owns these intentionally retained objects. No purge path is exercised.
		})
	}
	if !ran {
		t.Skip("no workspace scenarios selected")
	}
}

// Pause only the disposable test deployment to reproduce the two initialization
// windows deterministically; a real controller must finish cleanup after restart.
func TestPersistentNamespaceBootstrapDeletion(t *testing.T) {
	ctx := context.Background()
	kube, kubeconfig := e2eClient(t)
	if err := appsv1.AddToScheme(kube.Scheme()); err != nil {
		t.Fatal(err)
	}
	kubectl(t, kubeconfig, "-n", "agentrally-system", "scale", "deployment/agentrally-operator", "--replicas=0")
	t.Cleanup(func() {
		// testing.T.Context is canceled before cleanup callbacks run.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "agentrally-operator", Namespace: "agentrally-system"}}
		if err := kube.Patch(cleanupCtx, deployment, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":1}}`))); err != nil {
			t.Errorf("restore disposable Operator deployment: %v", err)
		}
	})
	kubectl(t, kubeconfig, "-n", "agentrally-system", "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name=agentrally-operator", "--timeout=90s")
	var groups []*v1.AgentGroup
	var namespaces []*corev1.Namespace
	for _, name := range []string{"before-pin", "before-detach"} {
		group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-" + name, Finalizers: []string{"arally.io/agent-group-cleanup"}}, Spec: v1.AgentGroupSpec{PersistentWorkspaces: true}}
		create(t, ctx, kube, group)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: group.NamespaceName(), OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "AgentGroup", Name: group.Name, UID: group.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}}
		create(t, ctx, kube, ns)
		if name == "before-detach" {
			group.Status.NamespaceUID = ns.UID
			if err := kube.Status().Update(ctx, group); err != nil {
				t.Fatal(err)
			}
		}
		if err := kube.Delete(ctx, group, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			t.Fatal(err)
		}
		groups = append(groups, group)
		namespaces = append(namespaces, ns)
	}
	kubectl(t, kubeconfig, "-n", "agentrally-system", "scale", "deployment/agentrally-operator", "--replicas=1")
	kubectl(t, kubeconfig, "-n", "agentrally-system", "rollout", "status", "deployment/agentrally-operator", "--timeout=120s")
	for i, group := range groups {
		awaitDeleted(t, ctx, kube, namespaces[i])
		awaitDeleted(t, ctx, kube, group)
	}
}
