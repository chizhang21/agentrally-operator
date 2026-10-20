package agentclaim

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func reconcileAllocation(t *testing.T, r *Reconciler, w *v1.AgentClaim) {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
		t.Fatal(err)
	}
}

func allocationObject(t *testing.T, c client.Client, w *v1.AgentClaim, kind, name string) *unstructured.Unstructured {
	t.Helper()
	object := sandbox.ExtensionObject(kind)
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestPoolSelectionPersistenceFailureCreatesNoCapacity(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	c, scheme := testutil.Client(t, group, ns, w)
	patchErr := apierrors.NewServiceUnavailable("status unavailable")
	guarded := interceptor.NewClient(c, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if name == "status" {
				return patchErr
			}
			return c.SubResource(name).Patch(ctx, object, patch, opts...)
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("capacity was created before selection was persisted")
			return nil
		},
	})
	r := &Reconciler{Client: guarded, Scheme: scheme}
	if err := r.ensureAllocationPool(ctx, w, executionConfiguration{Template: source, Service: true, Revision: "revision"}); !errors.Is(err, patchErr) {
		t.Fatalf("expected status persistence failure, got %v", err)
	}
	if getAgentClaim(t, c, w).Status.Provisioning != nil {
		t.Fatal("failed status patch persisted a pool selection")
	}
	for _, kind := range []string{"SandboxTemplate", "SandboxWarmPool"} {
		list := sandbox.ExtensionList(kind)
		if err := c.List(ctx, list, client.InNamespace(w.Namespace)); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("failed status patch left %s resources", kind)
		}
	}
}

func TestRevisionPersistenceFailureStopsAllocation(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source))
	patchErr := apierrors.NewServiceUnavailable("status unavailable")
	patches := 0
	guarded := interceptor.NewClient(c, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if claim, ok := object.(*v1.AgentClaim); name == "status" && ok && claim.Status.DesiredRevision != "" {
				patches++
				return patchErr
			}
			return c.SubResource(name).Patch(ctx, object, patch, opts...)
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("allocation started before the desired revision was persisted")
			return nil
		},
	})
	r := &Reconciler{Client: guarded, Scheme: scheme}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)})
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected API write failure, got %v", err)
	}
	if patches != 1 {
		t.Fatalf("write failure was converted into another status transition: %d patches", patches)
	}
	if claim := getAgentClaim(t, c, w); claim.Status.DesiredRevision != "" || claim.Status.Provisioning != nil {
		t.Fatal("failed revision write advanced allocation state")
	}
}

func TestMandatoryPoolCreationPrecedesClaimAndPreservesReferences(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: w.Namespace, UID: "secret-uid"}, Data: map[string][]byte{"key": []byte("test-credential-never-copy")}}
	w.Spec.ModelProvider = &v1.ModelProvider{BaseURL: "https://model.example/v1", Model: "test", APIKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "key"}}
	w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: workspaceVolumeSpec(), MountPath: "/workspace"}
	source.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	source.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}}}}
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source), secret)
	var creates []string
	guarded := interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
		kind := object.GetObjectKind().GroupVersionKind().Kind
		if kind == "Sandbox" {
			t.Fatal("Arally created a Sandbox")
		}
		if _, ok := object.(*corev1.Pod); ok {
			t.Fatal("Arally created a Pod")
		}
		if _, ok := object.(*corev1.PersistentVolumeClaim); ok {
			t.Fatal("Arally created a PVC instead of delegating to Core")
		}
		creates = append(creates, kind)
		selected := getAgentClaim(t, c, w).Status.Provisioning
		if selected == nil || selected.PoolName == "" || selected.TemplateName == "" || selected.StartedAt == nil {
			t.Fatal("resource write preceded persisted pool selection")
		}
		if kind == "SandboxClaim" {
			allocationObject(t, c, w, "SandboxWarmPool", selected.PoolName)
		}
		return c.Create(ctx, object, opts...)
	}})
	r := &Reconciler{Client: guarded, Scheme: scheme}
	reconcileAllocation(t, r, w)
	if !reflect.DeepEqual(creates, []string{"SandboxTemplate", "SandboxWarmPool", "SandboxClaim"}) {
		t.Fatalf("creation order: %v", creates)
	}
	state := getAgentClaim(t, c, w)
	pool := allocationObject(t, c, w, "SandboxWarmPool", state.Status.Provisioning.PoolName)
	template := allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
	if replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas"); replicas != 0 {
		t.Fatal("prewarming must remain opt-in for application startup side effects")
	}
	for _, resource := range []*unstructured.Unstructured{pool, template} {
		owner := metav1.GetControllerOf(resource)
		if owner == nil || owner.UID != group.UID || owner.Name != group.Name || owner.Kind != "AgentGroup" || owner.APIVersion != v1.GroupVersion.String() || resource.GetLabels()[allocation.ManagedLabel] != "true" {
			t.Fatal("shared capacity is not exactly Group-owned")
		}
	}
	claim := getClaim(t, c, w)
	ref, _, _ := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name")
	if ref != pool.GetName() || controller.CheckOwner(claim, w) != nil {
		t.Fatal("allocation Claim is not linked or owned")
	}
	if _, exists, _ := unstructured.NestedSlice(claim.Object, "spec", "env"); exists {
		t.Fatal("literal Claim env forces cold allocation")
	}
	if _, exists, _ := unstructured.NestedSlice(claim.Object, "spec", "volumeClaimTemplates"); exists {
		t.Fatal("per-Claim storage would bypass warm allocation")
	}
	fields, _, _ := unstructured.NestedMap(template.Object, "spec", "podTemplate")
	pod := &corev1.PodTemplateSpec{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, pod); err != nil {
		t.Fatal(err)
	}
	var keyRef *corev1.SecretKeySelector
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "OPENAI_API_KEY" {
			keyRef = env.ValueFrom.SecretKeyRef
		}
	}
	claims, err := sandbox.TemplateVolumeClaims(template)
	if err != nil || len(claims) != 1 || claims[0].Name != "arally-workspace" || !reflect.DeepEqual(claims[0].Spec, w.Spec.Workspace.VolumeClaimTemplate) {
		t.Fatal("workspace was not carried by the shared volume template", claims, err)
	}
	if keyRef == nil || keyRef.Name != secret.Name || keyRef.Key != "key" || pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name != secret.Name || len(pod.Spec.Volumes) != 0 {
		t.Fatal("credentials or instance-independent storage configuration lost")
	}
	encoded, _ := json.Marshal(template.Object)
	if strings.Contains(string(encoded), "test-credential-never-copy") {
		t.Fatal("Secret contents copied into template")
	}
	reconcileAllocation(t, r, w)
	if len(creates) != 3 {
		t.Fatalf("pending allocation recreated resources: %v", creates)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object()); !apierrors.IsNotFound(err) {
		t.Fatal("Arally allocated execution without upstream")
	}
}

func TestAutomaticPoolsShareConfigurationButSeparateCredentialsAndWorkspaces(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	secrets := []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: w.Namespace, UID: "s1"}, Data: map[string][]byte{"key": []byte("one")}}, {ObjectMeta: metav1.ObjectMeta{Name: "two", Namespace: w.Namespace, UID: "s2"}, Data: map[string][]byte{"key": []byte("two")}}}
	w.Spec.ModelProvider = &v1.ModelProvider{BaseURL: "https://model.example", Model: "test", APIKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "one"}, Key: "key"}}
	w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: workspaceVolumeSpec(), MountPath: "/workspace"}
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source), secrets[0], secrets[1])
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileAllocation(t, r, w)
	first := getAgentClaim(t, c, w).Status.Provisioning.PoolName
	for i, kind := range []string{"same", "secret", "workspace", "rotation"} {
		other := w.DeepCopy()
		other.Name = kind
		other.ResourceVersion = ""
		other.UID = types.UID(kind)
		if kind == "secret" {
			other.Spec.ModelProvider.APIKeySecretRef.Name = "two"
		}
		if kind == "workspace" {
			other.Spec.Workspace.VolumeClaimTemplate.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
		}
		if kind == "rotation" {
			current := &corev1.Secret{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(secrets[0]), current); err != nil {
				t.Fatal(err)
			}
			current.Data["key"] = []byte("rotated")
			if err := c.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		reconcileAllocation(t, r, other)
		name := getAgentClaim(t, c, other).Status.Provisioning.PoolName
		if (name == first) != (i == 0) {
			t.Fatalf("%s pool reuse=%v", kind, name == first)
		}
	}
	pools := sandbox.ExtensionList("SandboxWarmPool")
	if err := c.List(ctx, pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 4 {
		t.Fatalf("expected four configuration pools, got %d", len(pools.Items))
	}
}

func TestSharedPoolKeepsEachAllocatedWorkspacePrivate(t *testing.T) {
	group, ns, first, _ := testutil.Fixture()
	first.Spec.TemplateRef, first.Spec.Template = nil, "coding"
	first.Spec.Runtime = &v1.AgentRuntime{Image: "example.com/coding:v1"}
	first.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: workspaceVolumeSpec(), MountPath: "/workspace"}
	second := first.DeepCopy()
	second.Name, second.UID = "second", "second-uid"
	c, scheme := testutil.Client(t, group, ns, first, second)
	r := &Reconciler{Client: c, Scheme: scheme}
	poolName := ""
	var previousPVC string
	for _, claim := range []*v1.AgentClaim{first, second} {
		reconcileWithUpstreamSimulation(t, r, claim)
		pod := ownedExecutionPod(t, c, claim)
		reconcileWithUpstreamSimulation(t, r, claim)
		state := getAgentClaim(t, c, claim)
		testutil.Condition(t, state.Status.Conditions, "ConfigurationApplied", metav1.ConditionTrue)
		pvcName := state.Status.VolumeClaims["arally-workspace"]
		if pvcName == "" || pvcName == previousPVC {
			t.Fatal("allocations shared or failed to report a workspace PVC", state.Status.VolumeClaims)
		}
		previousPVC = pvcName
		if poolName != "" && state.Status.Provisioning.PoolName != poolName {
			t.Fatal("instance-specific PVC names split identical configurations into different pools")
		}
		poolName = state.Status.Provisioning.PoolName
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == "arally-workspace" {
				if pod.Spec.Volumes[i].PersistentVolumeClaim.ClaimName != pvcName {
					t.Fatal("reported workspace differs from the owned Pod mount")
				}
				pod.Spec.Volumes[i].PersistentVolumeClaim.ClaimName = "another-users-volume"
			}
		}
		config, err := r.resolveExecutionConfiguration(ctx, state, sandbox.Running)
		if err != nil {
			t.Fatal(err)
		}
		desired := sandbox.ExecutionPodTemplate(state.Status.SandboxName, config.Template, config.VolumeClaimTemplates)
		if observed := observeConfiguration(desired, pod, config.AgentContainer, config.Revision); observed.Condition.Reason != "RestartRequired" {
			t.Fatal("a wrong instantiated PVC was accepted as the desired workspace", observed)
		}
		pool := allocationObject(t, c, claim, "SandboxWarmPool", poolName)
		if err := unstructured.SetNestedField(pool.Object, int64(2), "spec", "replicas"); err != nil {
			t.Fatal(err)
		}
		if err := c.Update(ctx, pool); err != nil {
			t.Fatal(err)
		}
		reconcileWithUpstreamSimulation(t, r, claim)
		if got, _, _ := unstructured.NestedInt64(allocationObject(t, c, claim, "SandboxWarmPool", poolName).Object, "spec", "replicas"); got != 2 {
			t.Fatal("workspace allocation reset explicitly enabled prewarming")
		}
	}
}

func TestNativeVolumeTemplatesSurviveAllocationAndRejectStorageMismatch(t *testing.T) {
	group, ns, w, pod := testutil.Fixture()
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	source := nativeTemplate(w, pod)
	claims := []corev1.PersistentVolumeClaimTemplate{{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: workspaceVolumeSpec()}}
	if err := sandbox.SetTemplateVolumeClaims(source, claims); err != nil {
		t.Fatal(err)
	}
	c, scheme := testutil.Client(t, group, ns, w, source)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	current, err := sandbox.Decode(getSandbox(t, c, w))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAssignedSandbox(current, pod, true, claims); err != nil {
		t.Fatal("native storage template was lost during allocation", err)
	}
	current.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
	if reason, _ := diagnostics.Classify(validateAssignedSandbox(current, pod, true, claims)); reason != "PoolTemplateMismatch" {
		t.Fatal("assignment accepted a different storage blueprint", reason)
	}
}

func TestMandatoryPoolHandlesTasksSuspensionAndMissingNamedPool(t *testing.T) {
	for _, tc := range []struct {
		name             string
		restart          corev1.RestartPolicy
		suspended, named bool
		replicas         int64
	}{
		{"resident", corev1.RestartPolicyAlways, false, false, 0},
		{"task", corev1.RestartPolicyNever, false, false, 0},
		{"retry-task", corev1.RestartPolicyOnFailure, false, false, 0},
		{"named-missing", corev1.RestartPolicyAlways, false, true, 0},
		{"initial-suspension", corev1.RestartPolicyAlways, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, ns, w, source := testutil.Fixture()
			source.Spec.RestartPolicy = tc.restart
			if tc.suspended {
				w.Spec.OperatingMode = sandbox.Suspended
			}
			if tc.named {
				w.Spec.Provisioning = &v1.AgentClaimProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "chosen"}}
			}
			c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source))
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileAllocation(t, r, w)
			state := getAgentClaim(t, c, w)
			if tc.suspended {
				if state.Status.Provisioning != nil {
					t.Fatal("initial suspension started allocation")
				}
				return
			}
			pool := allocationObject(t, c, w, "SandboxWarmPool", state.Status.Provisioning.PoolName)
			replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
			if replicas != tc.replicas || (tc.named && pool.GetName() != "chosen") {
				t.Fatalf("pool config: %+v", pool.Object["spec"])
			}
			// No readyReplicas exists: upstream Claim must still be created for stock-miss allocation.
			getClaim(t, c, w)
		})
	}
}

func TestManagedPoolRetriesAndRejectsSnapshotOwnershipCollision(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	source.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source))
	poolWrites := 0
	r := &Reconciler{Scheme: scheme, Client: interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if o.GetObjectKind().GroupVersionKind().Kind == "SandboxWarmPool" {
			poolWrites++
			if poolWrites == 1 {
				return errors.New("temporary create failure")
			}
		}
		return c.Create(ctx, o, opts...)
	}})}
	reconcileAllocation(t, r, w)
	state := getAgentClaim(t, c, w)
	started := state.Status.Provisioning.StartedAt.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(desiredPoolClaim(w)), sandbox.ExtensionObject("SandboxClaim")); !apierrors.IsNotFound(err) {
		t.Fatal("Claim created despite failed pool creation")
	}
	template := allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
	// Mimic API-server CRD defaulting on the already-created native snapshot.
	fields, _, _ := unstructured.NestedMap(template.Object, "spec", "podTemplate")
	pod := &corev1.PodTemplateSpec{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.Containers[0].Stdin = false
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		t.Fatal(err)
	}
	fields["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["stdin"] = false
	fields["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["ports"].([]interface{})[0].(map[string]interface{})["protocol"] = "TCP"
	if err := unstructured.SetNestedMap(template.Object, fields, "spec", "podTemplate"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, template); err != nil {
		t.Fatal(err)
	}
	reconcileAllocation(t, r, w)
	getClaim(t, c, w)
	if !getAgentClaim(t, c, w).Status.Provisioning.StartedAt.Equal(started) {
		t.Fatal("retry reset allocation budget")
	}
	// A missing snapshot can be reconstructed without rewriting or scaling the pool.
	pool := allocationObject(t, c, w, "SandboxWarmPool", state.Status.Provisioning.PoolName)
	if err := unstructured.SetNestedField(pool.Object, int64(3), "spec", "replicas"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, template); err != nil {
		t.Fatal(err)
	}
	claim := getClaim(t, c, w)
	if err := c.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	state = getAgentClaim(t, c, w)
	state.Status.Provisioning.ClaimName = ""
	if err := c.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileAllocation(t, r, w)
	allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
	pool = allocationObject(t, c, w, "SandboxWarmPool", pool.GetName())
	replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	if replicas != 3 {
		t.Fatal("pool replica count overwritten")
	}
	// Retry must not adopt a template name collision, even with the same spec.
	if err := c.Delete(ctx, getClaim(t, c, w)); err != nil {
		t.Fatal(err)
	}
	state = getAgentClaim(t, c, w)
	state.Status.Provisioning.ClaimName = ""
	if err := c.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	template = allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
	template.SetOwnerReferences(nil)
	if err := c.Update(ctx, template); err != nil {
		t.Fatal(err)
	}
	reconcileAllocation(t, r, w)
	condition := meta.FindStatusCondition(getAgentClaim(t, c, w).Status.Conditions, "ConfigurationAccepted")
	if condition == nil || condition.Reason != "PoolOwnershipConflict" {
		t.Fatalf("collision accepted: %+v", condition)
	}
}

func TestAutomaticPoolAllocationTimeoutLatchesWithoutDirectFallback(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	now := time.Now().UTC()
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source))
	r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
	reconcileAllocation(t, r, w)
	now = now.Add(61 * time.Second)
	reconcileAllocation(t, r, w)
	state := getAgentClaim(t, c, w)
	if state.Status.Provisioning.TimedOutAt == nil {
		t.Fatal("timeout not persisted")
	}
	for range 2 {
		reconcileAllocation(t, r, w)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(desiredPoolClaim(w)), sandbox.ExtensionObject("SandboxClaim")); !apierrors.IsNotFound(err) {
		t.Fatal("timed out allocation was recreated")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object()); !apierrors.IsNotFound(err) {
		t.Fatal("timeout created direct Sandbox")
	}
	gr := &allocation.Manager{Client: c, Scheme: scheme}
	for range 3 {
		if _, err := gr.Prune(ctx, group); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: state.Status.Provisioning.PoolName}, sandbox.ExtensionObject("SandboxWarmPool")); !apierrors.IsNotFound(err) {
		t.Fatal("timed-out request kept unused warm capacity")
	}
	state = getAgentClaim(t, c, w)
	state.Spec.RestartToken = "retry"
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcileAllocation(t, r, w)
	}
	getClaim(t, c, w)
	if getAgentClaim(t, c, w).Status.Provisioning.TimedOutAt != nil {
		t.Fatal("explicit retry remains latched")
	}
}

func TestGroupPrunesOnlyUnusedManagedCapacityAfterPoolCleanup(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source))
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileAllocation(t, r, w)
	state := getAgentClaim(t, c, w)
	pool := allocationObject(t, c, w, "SandboxWarmPool", state.Status.Provisioning.PoolName)
	template := allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
	external := pool.DeepCopy()
	external.SetName("external")
	external.SetResourceVersion("")
	external.SetUID("")
	external.SetOwnerReferences(nil)
	external.SetLabels(nil)
	external.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": w.Spec.TemplateRef.Name}, "replicas": int64(0)}
	if err := c.Create(ctx, external); err != nil {
		t.Fatal(err)
	}
	foreign := template.DeepCopy()
	foreign.SetName("foreign")
	foreign.SetResourceVersion("")
	foreign.SetUID("")
	owners := foreign.GetOwnerReferences()
	owners[0].UID = "another-group"
	foreign.SetOwnerReferences(owners)
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	gr := &allocation.Manager{Client: c, Scheme: scheme}
	prune := func() bool {
		t.Helper()
		pending, err := gr.Prune(ctx, group)
		if err != nil {
			t.Fatal(err)
		}
		return pending
	}
	if prune() {
		t.Fatal("live AgentClaim lost shared capacity")
	}
	// Native claims protect the pool even when the Arally AgentClaim has expired.
	state.Status.Lifecycle = &v1.AgentClaimLifecycleStatus{ExpiredAt: ptr.To(metav1.Now())}
	if err := c.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	if prune() {
		t.Fatal("upstream Claim lost its pool")
	}
	if err := c.Delete(ctx, getClaim(t, c, w)); err != nil {
		t.Fatal(err)
	}
	// Deletion of a pool must wait for its warm children before deleting the snapshot.
	pool.SetUID("pool-uid")
	pool.SetFinalizers([]string{"upstream-cleanup"})
	if err := c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !prune() {
		t.Fatal("unused pool not deleted")
	}
	allocationObject(t, c, w, "SandboxTemplate", template.GetName())
	pool = allocationObject(t, c, w, "SandboxWarmPool", pool.GetName())
	pool.SetFinalizers(nil)
	if err := c.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !prune() {
		t.Fatal("orphan snapshot not deleted")
	}
	if prune() {
		t.Fatal("cleanup did not settle")
	}
	allocationObject(t, c, w, "SandboxTemplate", w.Spec.TemplateRef.Name) // User source remains independent.
	allocationObject(t, c, w, "SandboxWarmPool", external.GetName())
	allocationObject(t, c, w, "SandboxTemplate", foreign.GetName())
	// Persisted template selection also protects the gap before pool creation.
	orphan := template.DeepCopy()
	orphan.SetResourceVersion("")
	orphan.SetUID("")
	orphan.SetName("in-flight")
	if err := c.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	state = getAgentClaim(t, c, w)
	state.Status.Lifecycle = nil
	state.Status.Provisioning.TemplateName = orphan.GetName()
	if err := c.Status().Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	if prune() {
		t.Fatal("in-flight allocation snapshot pruned")
	}
	allocationObject(t, c, w, "SandboxTemplate", orphan.GetName())
}

func TestPendingAllocationDoesNotChangePoolWhenConfigurationChanges(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	c, scheme := testutil.Client(t, group, ns, w, nativeTemplate(w, source))
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileAllocation(t, r, w)
	poolName := getAgentClaim(t, c, w).Status.Provisioning.PoolName
	source.Spec.Containers[0].Image = "example/agent:v2"
	updateTemplate(t, c, w, source)
	reconcileAllocation(t, r, w)
	state := getAgentClaim(t, c, w)
	condition := meta.FindStatusCondition(state.Status.Conditions, "ConfigurationAccepted")
	if condition == nil || condition.Reason != "PoolTemplateMismatch" || condition.Status != metav1.ConditionFalse {
		t.Fatalf("pending configuration change accepted: %+v", condition)
	}
	ref, _, _ := unstructured.NestedString(getClaim(t, c, w).Object, "spec", "warmPoolRef", "name")
	if state.Status.Provisioning.PoolName != poolName || ref != poolName {
		t.Fatal("pending allocation switched pool")
	}
	pools := sandbox.ExtensionList("SandboxWarmPool")
	if err := c.List(ctx, pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 1 {
		t.Fatal("configuration change silently created more capacity")
	}
}

func TestSandboxFeedbackMapsThroughItsUpstreamClaim(t *testing.T) {
	_, _, w, _ := testutil.Fixture()
	other := w.DeepCopy()
	other.Name = "other"
	other.UID = "other-uid"
	c, scheme := indexedClient(t, w, other)
	r := &Reconciler{Client: c, Scheme: scheme}
	object := sandbox.Object()
	object.SetNamespace(w.Namespace)
	object.SetName("dynamic-upstream-name")
	object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: sandbox.ExtensionsGroupVersion.String(), Kind: "SandboxClaim", Name: desiredPoolClaim(w).GetName(), UID: "upstream-uid", Controller: ptr.To(true)}})
	requests := r.mapClaimSandbox(ctx, object)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(w) {
		t.Fatalf("incorrect allocation event mapping: %+v", requests)
	}
	object.SetNamespace("another-namespace")
	if len(r.mapClaimSandbox(ctx, object)) != 0 {
		t.Fatal("event mapping crossed namespace boundary")
	}
}
