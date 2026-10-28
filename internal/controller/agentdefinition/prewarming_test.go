package agentdefinition

import (
	"context"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func prewarmingDefinition(t *testing.T) (*Reconciler, *v1.AgentGroup, *v1.AgentDefinition) {
	t.Helper()
	group, ns, object, _, _ := testutil.Fixture()
	group.Spec.Prewarming = &v1.PrewarmingPolicy{Replicas: 2}
	object.Spec.TemplateRef, object.Spec.Template = nil, "generic"
	object.Spec.Container = &v1.AgentContainerSpec{Image: "example/agent:v1", ReadinessProbe: &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}},
	}}
	object.Spec.Prewarming = &v1.DefinitionPrewarmingPolicy{Replicas: 1}
	c, scheme := testutil.Client(t, group, ns, object)
	return &Reconciler{Client: c, Reader: c, Scheme: scheme}, group, object
}

func reconcileDefinition(t *testing.T, r *Reconciler, object *v1.AgentDefinition) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionPrewarmingWithoutInstancesAndScaleWithoutRevision(t *testing.T) {
	r, group, object := prewarmingDefinition(t)
	reconcileDefinition(t, r, object)
	state := object.Status.Prewarming
	if state == nil || state.PoolName == "" || !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") || meta.IsStatusConditionTrue(object.Status.Conditions, "Prepared") {
		t.Fatal("publication and proactive selection were not separated", object.Status)
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: object.Namespace, Name: state.PoolName}, pool); err != nil {
		t.Fatal(err)
	}
	if replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); replicas != 0 {
		t.Fatal("definition bypassed the Group's capacity decision", replicas)
	}
	manager := &allocation.Manager{Client: r.Client, Reader: r.Reader, Scheme: r.Scheme}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal("active definition capacity was pruned", err)
	}
	if replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); replicas != 1 {
		t.Fatal("definition's request was not applied", replicas)
	}
	pool.Object["status"] = map[string]interface{}{"observedGeneration": pool.GetGeneration() - 1, "readyReplicas": int64(1)}
	if err := r.Update(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	if meta.IsStatusConditionTrue(object.Status.Conditions, "Prepared") {
		t.Fatal("stale capacity was reported prepared")
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal(err)
	}
	pool.Object["status"] = map[string]interface{}{"observedGeneration": pool.GetGeneration(), "readyReplicas": int64(1)}
	if err := r.Update(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	if !meta.IsStatusConditionTrue(object.Status.Conditions, "Prepared") || object.Status.Prewarming.ReadyReplicas != 1 {
		t.Fatal("probe-checked current capacity was not reported", object.Status)
	}
	stableRV := object.ResourceVersion
	for range 2 {
		reconcileDefinition(t, r, object)
		if object.ResourceVersion != stableRV {
			t.Fatal("unchanged capacity observation rewrote Definition status")
		}
	}
	initial := object.Status.CurrentRevision
	object.Spec.Prewarming.Replicas = 2
	if err := r.Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	if object.Status.CurrentRevision != initial || !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") {
		t.Fatal("capacity change republished or invalidated application configuration", object.Status)
	}
	object.Spec.Prewarming.Replicas = 0
	if err := r.Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	if object.Status.Prewarming != nil && object.Status.Prewarming.PoolName != "" {
		t.Fatal("zero request retained active capacity")
	}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pool), sandbox.ExtensionObject("SandboxWarmPool")); !apierrors.IsNotFound(err) {
		t.Fatal("zero request permanently retained an unused pool", err)
	}
	instances := &v1.AgentInstanceList{}
	if err := r.List(t.Context(), instances); err != nil || len(instances.Items) != 0 {
		t.Fatal("proactive preparation created placeholder instances", err)
	}
}

func TestDefinitionDependencyRotationDrainsOldSparesAndClearsFailedDemand(t *testing.T) {
	r, group, object := prewarmingDefinition(t)
	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: object.Namespace}, Data: map[string]string{"version": "one"}}
	if err := r.Create(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	object.Spec.Container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: settings.Name}}}}
	if err := r.Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	initial, revision := *object.Status.Prewarming, object.Status.CurrentRevision
	instance := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "allocated", Namespace: object.Namespace}, Spec: v1.AgentInstanceSpec{
		AgentGroupRef: object.Spec.AgentGroupRef, DefinitionRef: v1.AgentDefinitionReference{Name: object.Name}},
		Status: v1.AgentInstanceStatus{Provisioning: &v1.AgentInstanceProvisioningStatus{PoolName: initial.PoolName, TemplateName: initial.TemplateName}}}
	if err := r.Create(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	manager := &allocation.Manager{Client: r.Client, Reader: r.Reader, Scheme: r.Scheme}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	settings.Data["version"] = "two"
	if err := r.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	if object.Status.CurrentRevision != revision || object.Status.Prewarming.PoolName == initial.PoolName {
		t.Fatal("dependency rotation did not separate configuration and application revisions", object.Status)
	}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	old := sandbox.ExtensionObject("SandboxWarmPool")
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: object.Namespace, Name: initial.PoolName}, old); err != nil {
		t.Fatal("rotation removed an allocated execution's dependencies", err)
	}
	if replicas, _, _ := unstructured.NestedInt64(old.Object, "spec", "replicas"); replicas != 0 {
		t.Fatal("rotation continued refilling the old credential configuration", replicas)
	}
	if err := r.Delete(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err == nil {
		t.Fatal("missing configuration was not reported")
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	if object.Status.Prewarming != nil || meta.IsStatusConditionTrue(object.Status.Conditions, "Prepared") || !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") {
		t.Fatal("failed dependency retained an active demand or revoked publication", object.Status)
	}
}

func TestReactivatedPreparationRejectsAnOlderPruningSnapshot(t *testing.T) {
	r, group, object := prewarmingDefinition(t)
	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: object.Namespace}}
	if err := r.Create(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	object.Spec.Container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: settings.Name}}}}
	if err := r.Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, object)
	initial := *object.Status.Prewarming
	if err := r.Delete(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); err == nil {
		t.Fatal("missing dependency did not deactivate demand")
	}
	settings.ResourceVersion, settings.UID = "", ""
	if err := r.Create(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	reactivated := false
	reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
		if err := c.List(ctx, list, options...); err != nil {
			return err
		}
		if _, ok := list.(*v1.AgentDefinitionList); ok {
			// The pruner has already read both old resource RVs and the cleared
			// selection. Reactivate before it issues conditional deletes.
			reconcileDefinition(t, r, object)
			reactivated = true
		}
		return nil
	}})
	manager := &allocation.Manager{Client: r.Client, Reader: reader, Scheme: r.Scheme}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if !reactivated || object.Status.Prewarming == nil || object.Status.Prewarming.PoolName != initial.PoolName {
		t.Fatal("test did not reactivate the same configuration", object.Status)
	}
	for kind, name := range map[string]string{"SandboxWarmPool": initial.PoolName, "SandboxTemplate": initial.TemplateName} {
		if err := r.Get(t.Context(), client.ObjectKey{Namespace: object.Namespace, Name: name}, sandbox.ExtensionObject(kind)); err != nil {
			t.Fatal("stale pruning removed reactivated capacity", kind, err)
		}
	}
}

func TestPublicationConflictCannotPrepareAnOlderApplication(t *testing.T) {
	r, _, object := prewarmingDefinition(t)
	base := r.Client.(client.WithWatch)
	updated := false
	r.Client = interceptor.NewClient(base, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, target client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
		if definition, ok := target.(*v1.AgentDefinition); ok && subresource == "status" && !updated && definition.Status.CurrentRevision != "" {
			latest := &v1.AgentDefinition{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(object), latest); err != nil {
				return err
			}
			latest.Spec.Container.Image = "example/agent:v2"
			latest.Generation++ // The fake client does not implement API generation increments.
			if err := c.Update(ctx, latest); err != nil {
				return err
			}
			updated = true
		}
		return c.SubResource(subresource).Patch(ctx, target, patch, options...)
	}})
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}); !apierrors.IsConflict(err) || !updated {
		t.Fatal("publication accepted a concurrently replaced generation", err)
	}
	pools := sandbox.ExtensionList("SandboxWarmPool")
	if err := r.List(t.Context(), pools); err != nil || len(pools.Items) != 0 {
		t.Fatal("stale publication created proactive capacity", err)
	}
}

func TestDefinitionPrewarmingRejectsUnsafeInputsWithoutBlockingPublication(t *testing.T) {
	for _, tc := range []struct{ scenario, reason string }{
		{"no-probe", "PrewarmingProbeRequired"},
		{"workspace", "PrewarmingStorageUnsupported"},
		{"group-disabled", "PrewarmingDisabled"},
		{"paused", "AllocationPaused"},
		{"token", "PrewarmingCredentialsUnsupported"},
		{"runtime-policy", "RuntimeClassDenied"},
		{"task", "PrewarmingTaskUnsupported"},
		{"nfs", "PrewarmingStorageUnsupported"},
		{"csi", "PrewarmingStorageUnsupported"},
		{"pvc", "PrewarmingStorageUnsupported"},
		{"projected-token", "PrewarmingCredentialsUnsupported"},
	} {
		scenario := tc.scenario
		t.Run(scenario, func(t *testing.T) {
			r, group, object := prewarmingDefinition(t)
			switch scenario {
			case "no-probe":
				object.Spec.Container.ReadinessProbe = nil
			case "workspace":
				object.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{}}
			case "group-disabled":
				group.Spec.Prewarming = nil
			case "paused":
				group.Spec.AllocationPaused = true
			case "token":
				// A native template can opt into API credentials; preparation must reject it.
				object.Spec.Template, object.Spec.Container = "", nil
				object.Spec.TemplateRef = &v1.SandboxTemplateReference{Name: "token-template"}
				source := sandbox.ExtensionObject("SandboxTemplate")
				source.SetName("token-template")
				source.SetNamespace(object.Namespace)
				source.Object["spec"] = map[string]interface{}{"networkPolicyManagement": "Unmanaged", "podTemplate": map[string]interface{}{"spec": map[string]interface{}{
					"automountServiceAccountToken": true, "containers": []interface{}{map[string]interface{}{"name": "agent", "image": "example/agent:v1",
						"readinessProbe": map[string]interface{}{"exec": map[string]interface{}{"command": []interface{}{"true"}}}}},
				}}}
				if err := r.Create(t.Context(), source); err != nil {
					t.Fatal(err)
				}
			case "runtime-policy":
				group.Spec.ExecutionPolicy = &v1.ExecutionPolicy{AllowedRuntimeClasses: []string{"kata"}}
			default:
				pod := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "example/agent:v1", ReadinessProbe: object.Spec.Container.ReadinessProbe}}}}
				switch scenario {
				case "task":
					pod.Spec.RestartPolicy = corev1.RestartPolicyNever
				case "nfs":
					pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "example.invalid", Path: "/data"}}}}
				case "csi":
					pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "example.invalid"}}}}
				case "pvc":
					pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "business-data"}}}}
				case "projected-token":
					pod.Spec.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}}}}
				}
				fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
				if err != nil {
					t.Fatal(err)
				}
				object.Spec.Template, object.Spec.Container = "", nil
				object.Spec.TemplateRef = &v1.SandboxTemplateReference{Name: "native"}
				source := sandbox.ExtensionObject("SandboxTemplate")
				source.SetName("native")
				source.SetNamespace(object.Namespace)
				source.Object["spec"] = map[string]interface{}{"networkPolicyManagement": "Unmanaged", "podTemplate": fields}
				if err := r.Create(t.Context(), source); err != nil {
					t.Fatal(err)
				}
			}
			for _, changed := range []client.Object{object, group} {
				if err := r.Update(t.Context(), changed); err != nil {
					t.Fatal(err)
				}
			}
			reconcileDefinition(t, r, object)
			prepared := meta.FindStatusCondition(object.Status.Conditions, "Prepared")
			if !meta.IsStatusConditionTrue(object.Status.Conditions, "Ready") || prepared == nil || prepared.Status != "False" || prepared.Reason != tc.reason {
				t.Fatal("unsafe preparation changed publication readiness", object.Status)
			}
			pools := sandbox.ExtensionList("SandboxWarmPool")
			if err := r.List(t.Context(), pools); err != nil || len(pools.Items) != 0 {
				t.Fatal("unsafe preparation created capacity", err)
			}
		})
	}
}

func TestProactiveDefinitionsShareBudgetAndPauseUnusedCapacity(t *testing.T) {
	r, group, first := prewarmingDefinition(t)
	group.Spec.Prewarming.Replicas = 5
	group.Spec.Prewarming.MaxTotalReplicas = ptr.To(int32(4))
	if err := r.Update(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	second := first.DeepCopy()
	second.Name, second.UID, second.ResourceVersion = "second", "second-uid", ""
	second.Spec.Prewarming.Replicas = 3
	if err := r.Create(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	reconcileDefinition(t, r, first)
	reconcileDefinition(t, r, second)
	manager := &allocation.Manager{Client: r.Client, Reader: r.Reader, Scheme: r.Scheme}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	for _, definition := range []*v1.AgentDefinition{first, second} {
		pool := sandbox.ExtensionObject("SandboxWarmPool")
		if err := r.Get(t.Context(), client.ObjectKey{Namespace: definition.Namespace, Name: definition.Status.Prewarming.PoolName}, pool); err != nil {
			t.Fatal(err)
		}
		replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		if replicas != int64(definition.Spec.Prewarming.Replicas) {
			t.Fatal("small requests stranded the shared budget", definition.Name, replicas)
		}
		// Reconciliation of a Definition cannot overwrite the Group decision.
		reconcileDefinition(t, r, definition)
	}
	group.Spec.Prewarming.MaxTotalReplicas = ptr.To(int32(1))
	if err := r.Update(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	for _, definition := range []*v1.AgentDefinition{first, second} {
		reconcileDefinition(t, r, definition)
	}
	limited := meta.FindStatusCondition(second.Status.Conditions, "Prepared")
	if second.Status.Prewarming.RequestedReplicas != 3 || second.Status.Prewarming.DesiredReplicas != 0 || limited == nil || limited.Status != metav1.ConditionFalse || limited.Reason != "BudgetLimited" {
		t.Fatal("budget shortage was not observable", second.Status)
	}
	pools := sandbox.ExtensionList("SandboxWarmPool")
	if err := r.List(t.Context(), pools); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, pool := range pools.Items {
		replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
		total += replicas
	}
	if total != 1 {
		t.Fatal("Definitions multiplied the Group total budget", total)
	}
	group.Spec.AllocationPaused = true
	if _, err := manager.Prune(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	if err := r.List(t.Context(), pools); err != nil || len(pools.Items) != 0 {
		t.Fatal("paused, unused proactive capacity was retained", err)
	}
}
