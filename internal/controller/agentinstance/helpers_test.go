package agentinstance

import (
	"context"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
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
)

var ctx = context.Background()

func workspaceVolumeSpec() corev1.PersistentVolumeClaimSpec {
	return corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}
}

func desiredExecutionForTest(ctx context.Context, c client.Reader, w *v1.AgentInstance) (*corev1.PodTemplateSpec, bool, error) {
	resolved, err := desiredExecution(ctx, c, w)
	if err != nil {
		return nil, false, err
	}
	return resolved.PodTemplate, resolved.Service, nil
}

func desiredPodTemplate(ctx context.Context, c client.Reader, workload *v1.AgentInstance) (*corev1.PodTemplateSpec, error) {
	template, _, err := desiredExecutionForTest(ctx, c, workload)
	return template, err
}

func nativeTemplate(definition *v1.AgentDefinition, pod *corev1.PodTemplateSpec) *unstructured.Unstructured {
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		panic(err)
	}
	object := sandbox.ExtensionObject("SandboxTemplate")
	object.SetName(definition.Spec.TemplateRef.Name)
	object.SetNamespace(definition.Namespace)
	object.Object["spec"] = map[string]interface{}{"podTemplate": fields, "service": true, "networkPolicyManagement": "Unmanaged"}
	return object
}

func updateTemplate(t *testing.T, c client.Client, w *v1.AgentInstance, pod *corev1.PodTemplateSpec) {
	t.Helper()
	definition := &v1.AgentDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Spec.DefinitionRef.Name}, definition); err != nil {
		t.Fatal(err)
	}
	object := sandbox.ExtensionObject("SandboxTemplate")
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: definition.Spec.TemplateRef.Name}, object); err != nil {
		t.Fatal(err)
	}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedMap(object.Object, fields, "spec", "podTemplate"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
}

// reconcileWithUpstreamSimulation includes a small upstream allocation simulation for default-pool
// scenarios. Named-pool and allocation contract tests drive the two controllers
// explicitly; raw Reconcile tests verify Arally never creates a Sandbox or Pod.
func reconcileWithUpstreamSimulation(t *testing.T, r *Reconciler, w *v1.AgentInstance) ctrl.Result {
	t.Helper()
	testutil.PublishDefinitions(t, r.Client, r.Scheme)
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)})
	if err != nil {
		t.Fatal(err)
	}
	if w.Spec.Provisioning == nil {
		simulateUpstreamGarbageCollection(t, r, w)
		state := &v1.AgentInstance{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(w), state); err == nil && state.DeletionTimestamp == nil && (state.Status.Lifecycle == nil || state.Status.Lifecycle.ExpiredAt == nil) && state.Status.Provisioning != nil && state.Status.Provisioning.ClaimName != "" && state.Status.Provisioning.TimedOutAt == nil {
			claim := desiredPoolClaim(state)
			if err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(claim), claim); err == nil {
				name, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
				if name == "" {
					simulateUpstreamAllocation(t, r, state, claim)
					result, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	return result
}

func simulateUpstreamAllocation(t *testing.T, r *Reconciler, w *v1.AgentInstance, claim *unstructured.Unstructured) {
	t.Helper()
	if claim.GetUID() == "" {
		claim.SetUID(types.UID("claim-" + string(w.UID)))
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	if err := r.interactionClient().Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: allocation.PoolName(w)}, pool); err != nil {
		t.Fatal(err)
	}
	name, _, _ := unstructured.NestedString(pool.Object, "spec", "sandboxTemplateRef", "name")
	template := sandbox.ExtensionObject("SandboxTemplate")
	if err := r.interactionClient().Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, template); err != nil {
		t.Fatal(err)
	}
	fields, _, _ := unstructured.NestedMap(template.Object, "spec", "podTemplate")
	pod := &corev1.PodTemplateSpec{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, pod); err != nil {
		t.Fatal(err)
	}
	service, _, _ := unstructured.NestedBool(template.Object, "spec", "service")
	claims, err := sandbox.TemplateVolumeClaims(template)
	if err != nil {
		t.Fatal(err)
	}
	object, err := sandbox.Encode(&sandbox.Sandbox{Metadata: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace, UID: "sandbox-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: sandbox.ExtensionsGroupVersion.String(), Kind: "SandboxClaim", Name: claim.GetName(), UID: claim.GetUID(), Controller: ptr.To(true)}}}, Spec: sandbox.Spec{PodTemplate: *pod, VolumeClaimTemplates: claims, Service: service, OperatingMode: sandbox.Running}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
	claim.Object["status"] = map[string]interface{}{"sandbox": map[string]interface{}{"name": w.Name}}
	if err := r.interactionClient().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	nativeReady(t, r.Client, object)
}

// The fake API server has no garbage collector. Simulate only children of the
// exact default-testutil.Fixture Claim, outside Arally's reconciliation contract.
func simulateUpstreamGarbageCollection(t *testing.T, r *Reconciler, w *v1.AgentInstance) {
	t.Helper()
	claim := desiredPoolClaim(w)
	if err := r.interactionClient().Get(ctx, client.ObjectKeyFromObject(claim), claim); !apierrors.IsNotFound(err) {
		return
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(sandbox.GroupVersion.WithKind("SandboxList"))
	if err := r.List(ctx, list, client.InNamespace(w.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		object := &list.Items[i]
		ref := metav1.GetControllerOf(object)
		if ref == nil || ref.Kind != "SandboxClaim" || ref.Name != claim.GetName() || ref.UID != types.UID("claim-"+string(w.UID)) {
			continue
		}
		if object.GetDeletionTimestamp() == nil {
			if err := r.Delete(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func getAgentInstance(t *testing.T, c client.Client, w *v1.AgentInstance) *v1.AgentInstance {
	t.Helper()
	current := &v1.AgentInstance{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func getSandbox(t *testing.T, c client.Client, w *v1.AgentInstance) *unstructured.Unstructured {
	t.Helper()
	current := sandbox.Object()
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func assertNoSandbox(t *testing.T, c client.Client, w *v1.AgentInstance) {
	t.Helper()
	object := sandbox.Object()
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), object); !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected Sandbox: %v", err)
	}
}

func assertReason(t *testing.T, w *v1.AgentInstance, kind, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(w.Status.Conditions, kind)
	if c == nil || c.Reason != reason {
		t.Fatalf("%s want %s, got %+v", kind, reason, c)
	}
}

func nativeReady(t *testing.T, c client.Client, object *unstructured.Unstructured) {
	t.Helper()
	object.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "reason": "PodReady", "message": "Ready", "observedGeneration": object.GetGeneration(), "lastTransitionTime": metav1.Now().Format(time.RFC3339)}}}
	if err := c.Status().Update(ctx, object); err != nil {
		t.Fatal(err)
	}
}

func ownedExecutionPod(t *testing.T, c client.Client, w *v1.AgentInstance) *corev1.Pod {
	t.Helper()
	object := getSandbox(t, c, w)
	object.SetUID("sandbox-uid")
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	current, err := sandbox.Decode(object)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: object.GetName(), Namespace: w.Namespace, UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: sandbox.GroupVersion.String(), Kind: "Sandbox", Name: object.GetName(), UID: object.GetUID(), Controller: ptr.To(true)}}}, Spec: sandbox.ExecutionPodTemplate(object.GetName(), &current.Spec.PodTemplate, current.Spec.VolumeClaimTemplates).Spec}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func setupPooledAgentInstance(t *testing.T) (*v1.AgentInstance, client.WithWatch, *runtime.Scheme) {
	t.Helper()
	group, ns, definition, w, source := testutil.Fixture()
	w.Spec.Provisioning = &v1.AgentInstanceProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "agents"}}
	definition.Spec.TemplateRef.Name = "agents"
	c, scheme := testutil.Client(t, definition, group, ns, w, nativeTemplate(definition, source))
	config, err := desiredExecution(ctx, c, w)
	if err != nil {
		t.Fatal(err)
	}
	config.AgentContainer = definition.Spec.AgentContainer
	revision, err := configRevision(ctx, c, w.Namespace, *config)
	if err != nil {
		t.Fatal(err)
	}
	seed := w.DeepCopy()
	seed.Spec.Provisioning = nil
	manager := allocation.Manager{Client: c, Scheme: scheme}
	selection, err := manager.Select(ctx, seed, *config, revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Ensure(ctx, selection, *config); err != nil {
		t.Fatal(err)
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName("agents")
	pool.SetNamespace(w.Namespace)
	pool.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": selection.TemplateName}, "replicas": int64(2)}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return w, c, scheme
}

func getClaim(t *testing.T, c client.Client, w *v1.AgentInstance) *unstructured.Unstructured {
	t.Helper()
	claim := desiredPoolClaim(w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	return claim
}

func assignPoolSandbox(t *testing.T, c client.Client, w *v1.AgentInstance, ready bool) *unstructured.Unstructured {
	t.Helper()
	claim := getClaim(t, c, w)
	claim.SetUID("claim-uid")
	claim.Object["status"] = map[string]interface{}{"sandbox": map[string]interface{}{"name": "allocated"}}
	if err := c.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	state := getAgentInstance(t, c, w)
	selected := allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
	template, _, err := sandbox.TemplatePod(selected)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.Encode(&sandbox.Sandbox{Metadata: metav1.ObjectMeta{Name: "allocated", Namespace: w.Namespace, UID: "allocated-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: sandbox.ExtensionsGroupVersion.String(), Kind: "SandboxClaim", Name: claim.GetName(), UID: claim.GetUID(), Controller: ptr.To(true)}}}, Spec: sandbox.Spec{PodTemplate: *template, Service: true, OperatingMode: sandbox.Running}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if ready {
		nativeReady(t, c, sb)
	}
	return sb
}

func saveDefinition(t *testing.T, c client.Client, desired *v1.AgentDefinition) {
	t.Helper()
	current := &v1.AgentDefinition{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		t.Fatal(err)
	}
	current.Spec = *desired.Spec.DeepCopy()
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	*desired = *current
}
