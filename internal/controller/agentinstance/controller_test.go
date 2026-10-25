package agentinstance

import (
	"context"
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/controller/agentgroup"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAgentInstanceDelegatesLifecycleAndPreservesCoreFields(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace}, Spec: source.Spec}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w, pod)
	mutations := 0
	patches := 0
	guarded := interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if _, ok := o.(*corev1.Pod); ok {
				t.Fatal("AgentRally created a Pod")
			}
			mutations++
			return c.Create(ctx, o, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			if _, ok := o.(*corev1.Pod); ok {
				t.Fatal("AgentRally updated a Pod")
			}
			if u, ok := o.(*unstructured.Unstructured); ok && u.GroupVersionKind().Kind == "Sandbox" {
				old := sandbox.Object()
				if err := c.Get(ctx, client.ObjectKeyFromObject(u), old); err != nil {
					return err
				}
				if !reflect.DeepEqual(old.Object["spec"], u.Object["spec"]) {
					u.SetGeneration(old.GetGeneration() + 1)
				}
			}
			mutations++
			return c.Update(ctx, o, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			if _, ok := o.(*corev1.Pod); ok {
				t.Fatal("AgentRally deleted a Pod")
			}
			return c.Delete(ctx, o, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, o client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			patches++
			return c.SubResource(name).Patch(ctx, o, p, opts...)
		},
	})
	r := &Reconciler{Client: guarded, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	sb := getSandbox(t, c, w)
	if err := checkClaimSandboxOwner(sb, getClaim(t, c, w)); err != nil {
		t.Fatal(err)
	}
	decoded, err := sandbox.Decode(sb)
	if err != nil {
		t.Fatal(err)
	}
	expectedTemplate := source.DeepCopy()
	expectedTemplate.Spec.AutomountServiceAccountToken = ptr.To(false)
	revision, err := configRevision(ctx, c, w.Namespace, allocation.Configuration{PodTemplate: expectedTemplate, AgentContainer: definition.Spec.AgentContainer, Definition: getAgentInstance(t, c, w).Status.Definition})
	if err != nil {
		t.Fatal(err)
	}
	if err := podconfig.Stamp(expectedTemplate, definition.Spec.AgentContainer, revision); err != nil {
		t.Fatal(err)
	}
	if decoded.Spec.OperatingMode != sandbox.Running || !decoded.Spec.Service || !reflect.DeepEqual(decoded.Spec.PodTemplate, *expectedTemplate) {
		t.Fatalf("native template/defaults changed: %+v", decoded.Spec)
	}
	sb.SetGeneration(4)
	if err := unstructured.SetNestedField(sb.Object, "upstream-value", "spec", "extensionField"); err != nil {
		t.Fatal(err)
	}
	sb.SetAnnotations(map[string]string{"upstream": "keep"})
	if err := c.Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	sb.Object["status"] = map[string]interface{}{"service": "core-service", "serviceFQDN": "core-service.team-agents.svc", "conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": int64(4), "reason": "PodReady", "message": "Ready", "lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z")}}}
	if err := c.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	current := getAgentInstance(t, c, w)
	// The same-name Pod is not owned by this Sandbox. Do not accept Core's
	// optimistic Ready condition as proof that this unrelated Pod is ours.
	testutil.Condition(t, current.Status.Conditions, "Ready", metav1.ConditionUnknown)
	assertReason(t, current, "Ready", "PodObservationBlocked")
	if current.Status.ServiceName != "core-service" {
		t.Fatal("Core service status lost")
	}
	beforeMutations, beforePatches := mutations, patches
	reconcileWithUpstreamSimulation(t, r, w)
	if mutations != beforeMutations || patches != beforePatches {
		t.Fatal("unchanged reconcile wrote resources/status")
	}
	source.Spec.Containers[0].Image = "example/agent:v2"
	updateTemplate(t, c, w, source)
	current.Spec.OperatingMode = sandbox.Suspended
	current.Generation++
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	sb = getSandbox(t, c, w)
	decoded, err = sandbox.Decode(sb)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Spec.PodTemplate.Spec.Containers[0].Image != "example/agent:v1" || decoded.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("assigned Sandbox should preserve its configuration and apply suspension")
	}
	if sb.GetAnnotations()["upstream"] != "keep" || sb.Object["spec"].(map[string]interface{})["extensionField"] != "upstream-value" {
		t.Fatal("upstream fields overwritten")
	}
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "ConfigurationAccepted", metav1.ConditionTrue)
	original := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), original); err != nil {
		t.Fatal(err)
	}
	if original.Spec.Containers[0].Image != "example/agent:v1" {
		t.Fatal("AgentRally rolled the Pod instead of delegating to Core")
	}
}

func TestModelAndWorkspaceReferencesAllowFirstConsumerAndKeepTemplate(t *testing.T) {
	_, _, definition, w, source := testutil.Fixture()
	definition.Spec.ModelProvider = &v1.ModelProvider{BaseURL: "https://model.example/v1", Model: "test-model", APIKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "model-key"}, Key: "token"}}
	w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: ptr.To(workspaceVolumeSpec()), MountPath: "/workspace", ReadOnly: true}
	source.Spec.Containers = append(source.Spec.Containers, corev1.Container{Name: "sidecar", Image: "sidecar"})
	definition.Spec.AgentContainer = "sidecar"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-key", Namespace: w.Namespace}, Data: map[string][]byte{"token": []byte("do-not-copy")}}
	c, _ := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), secret)
	original := w.DeepCopy()
	resolved, err := desiredExecution(ctx, c, w)
	if err != nil {
		t.Fatal(err)
	}
	template := resolved.PodTemplate
	if !reflect.DeepEqual(w, original) {
		t.Fatal("mutated user's template")
	}
	if !reflect.DeepEqual(template.Spec.Containers[0], source.Spec.Containers[0]) {
		t.Fatal("modified non-agent container")
	}
	container := template.Spec.Containers[1]
	found := false
	for _, env := range container.Env {
		if env.Name == "OPENAI_API_KEY" {
			found = true
			if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef.Name != "model-key" || *env.ValueFrom.SecretKeyRef.Optional {
				t.Fatal("credential was copied instead of referenced")
			}
		}
	}
	if !found || len(template.Spec.Volumes) != 0 || len(resolved.VolumeClaimTemplates) != 1 || resolved.VolumeClaimTemplates[0].Name != "agentrally-workspace" || !container.VolumeMounts[0].ReadOnly {
		t.Fatal("model/workspace wiring missing")
	}
	secret.Data["token"] = nil
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := desiredPodTemplate(ctx, c, w); err == nil {
		t.Fatal("empty model credential accepted")
	}
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := desiredPodTemplate(ctx, c, w); err == nil {
		t.Fatal("missing secret accepted")
	}
}

func TestAgentInstanceDependencyBlockingAndWatchMapping(t *testing.T) {
	for _, tc := range []string{"missing-group", "wrong-namespace", "stale-group", "missing-secret"} {
		t.Run(tc, func(t *testing.T) {
			group, ns, definition, w, source := testutil.Fixture()
			objects := []client.Object{ns, w}
			switch tc {
			case "wrong-namespace":
				group.Spec.Namespace = "other"
			case "stale-group":
				group.Generation++
			case "missing-secret":
				definition.Spec.ModelProvider = &v1.ModelProvider{BaseURL: "https://model.example", Model: "test", APIKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "key"}}
			}
			if tc != "missing-group" {
				objects = append(objects, group)
			}
			c, scheme := clientWithPublishedDefinitions(t, append(objects, definition, nativeTemplate(definition, source))...)
			r := &Reconciler{Client: c, Scheme: scheme}
			result := reconcileWithUpstreamSimulation(t, r, w)
			if result.RequeueAfter == 0 {
				t.Fatal("blocked dependency not retried")
			}
			testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "ConfigurationAccepted", metav1.ConditionFalse)
			if err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object()); !apierrors.IsNotFound(err) {
				t.Fatalf("blocked workload created Sandbox: %v", err)
			}
		})
	}
	group, _, definition, w, source := testutil.Fixture()
	w.Status.VolumeClaims = map[string]string{"agentrally-workspace": "workspace"}
	unrelated := w.DeepCopy()
	unrelated.Name = "unrelated"
	unrelated.Spec.Workspace = nil
	unrelated.Status.VolumeClaims = nil
	c, scheme := indexedClient(t, nativeTemplate(definition, source), w, unrelated)
	r := &Reconciler{Client: c, Scheme: scheme}
	for _, obj := range []client.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}} {
		requests := r.mapDependency(ctx, obj)
		if len(requests) != 1 || requests[0].Name != w.Name {
			t.Fatalf("incorrect reference mapping: %+v", requests)
		}
	}
	if len(r.mapDependency(ctx, group)) != 2 {
		t.Fatal("group changes must enqueue all referenced workloads")
	}
}

func TestForeignResourcesAreNeverAdoptedOrDeleted(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	foreign := desiredPoolClaim(w)
	foreign.SetNamespace(w.Namespace)
	foreign.SetUID("foreign")
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w, foreign)
	r := &Reconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(getAgentInstance(t, c, w).Status.Conditions, "ConfigurationAccepted"); cond == nil || cond.Reason != "PoolOwnershipConflict" || cond.Status != metav1.ConditionFalse {
		t.Fatalf("foreign Claim accepted: %+v", cond)
	}
	current := getAgentInstance(t, c, w)
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err == nil {
		t.Fatal("foreign Claim deletion accepted")
	}
	currentClaim := desiredPoolClaim(w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(currentClaim), currentClaim); err != nil {
		t.Fatal(err)
	}
	if currentClaim.GetUID() != "foreign" {
		t.Fatal("foreign resource changed")
	}
	ns.OwnerReferences = nil
	c, scheme = clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns)
	gr := &agentgroup.Reconciler{Client: c, Scheme: scheme}
	if _, err := gr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err == nil {
		t.Fatal("unrelated namespace adopted")
	}
}

func TestGroupNamespaceOwnershipFailureIsReturned(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	ns.OwnerReferences = nil
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err == nil {
		t.Fatal("namespace ownership failure was treated as pending group readiness")
	}
	condition := meta.FindStatusCondition(getAgentInstance(t, c, w).Status.Conditions, "ConfigurationAccepted")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ReconcileError" {
		t.Fatalf("ownership failure did not block reconciliation: %+v", condition)
	}
	claim := desiredPoolClaim(w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), claim); !apierrors.IsNotFound(err) {
		t.Fatalf("allocation proceeded despite foreign namespace ownership: %v", err)
	}
}

func TestAgentInstanceForegroundDeletionWaitsAndRetainsPVC(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: w.Namespace}}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w, pvc)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	sb := getClaim(t, c, w)
	sb.SetUID("claim-workload-uid")
	sb.SetFinalizers([]string{"core-cleanup"})
	if err := c.Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	checked := false
	r.Client = interceptor.NewClient(c, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
		if o.GetObjectKind().GroupVersionKind().Kind != "SandboxClaim" {
			return c.Delete(ctx, o, opts...)
		}
		options := &client.DeleteOptions{}
		for _, opt := range opts {
			opt.ApplyToDelete(options)
		}
		if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "claim-workload-uid" {
			t.Fatal("unsafe delete options")
		}
		checked = true
		return c.Delete(ctx, o, opts...)
	}})
	current := getAgentInstance(t, c, w)
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	if reconcileWithUpstreamSimulation(t, r, w).RequeueAfter == 0 || !checked {
		t.Fatal("did not wait for Core cleanup")
	}
	sb = getClaim(t, c, w)
	if sb.GetDeletionTimestamp() == nil {
		t.Fatal("Claim not deleting")
	}
	sb.SetFinalizers(nil)
	if err := c.Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), &v1.AgentInstance{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workload finalizer stuck: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("workspace deleted: %v", err)
	}
}
