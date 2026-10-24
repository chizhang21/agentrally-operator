package agentinstance

import (
	"context"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAgentInstancePoolAllocationUsesClaimAndCoreOwnership(t *testing.T) {
	w, c, scheme := setupPooledAgentInstance(t)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	assertNoSandbox(t, c, w)
	claim := getClaim(t, c, w)
	if err := controller.CheckOwner(claim, w); err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(claim.Object, "spec")
	if len(spec) != 1 || spec["env"] != nil {
		t.Fatalf("unexpected Claim overrides: %v", spec)
	}
	sb := assignPoolSandbox(t, c, w, true)
	current, err := sandbox.Decode(sb)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sb.GetName(), Namespace: w.Namespace, UID: "warm-pod", OwnerReferences: []metav1.OwnerReference{{APIVersion: sandbox.GroupVersion.String(), Kind: "Sandbox", Name: sb.GetName(), UID: sb.GetUID(), Controller: ptr.To(true)}}}, Spec: current.Spec.PodTemplate.Spec}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	if state.Status.SandboxName != "allocated" || state.Status.Provisioning.CompletedAt == nil {
		t.Fatalf("binding not persisted: %+v", state.Status)
	}
	testutil.Condition(t, state.Status.Conditions, "Available", metav1.ConditionTrue)
	testutil.Condition(t, state.Status.Conditions, "Ready", metav1.ConditionTrue)
	assertReason(t, state, "ConfigurationApplied", "ConfigurationCurrent")
	// A running allocation survives edits replacement is explicit.

	resolved, err := resolveSandboxTemplate(ctx, c, w)
	if err != nil {
		t.Fatal(err)
	}
	resolved.PodTemplate.Spec.Containers[0].Image = "example/agent:v2"
	updateTemplate(t, c, w, resolved.PodTemplate)
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	assertNoSandbox(t, c, w)
	assertReason(t, getAgentInstance(t, c, w), "ConfigurationApplied", "ConfigurationCurrent")
	observed := sandbox.Object()
	if err := c.Get(ctx, client.ObjectKeyFromObject(sb), observed); err != nil {
		t.Fatal(err)
	}
	if err := checkClaimSandboxOwner(observed, getClaim(t, c, w)); err != nil {
		t.Fatal("Arally adopted Claim's Sandbox")
	}
}

func TestAgentInstancePoolCreationRetryRecovers(t *testing.T) {
	w, c, scheme := setupPooledAgentInstance(t)
	failed := false
	guarded := interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if o.GetObjectKind().GroupVersionKind().Kind == "SandboxClaim" && !failed {
			failed = true
			return apierrors.NewServiceUnavailable("fixture unavailable")
		}
		return c.Create(ctx, o, opts...)
	}})
	r := &Reconciler{Client: guarded, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	started := getAgentInstance(t, c, w).Status.Provisioning.StartedAt.DeepCopy()
	reconcileWithUpstreamSimulation(t, r, w)
	getClaim(t, c, w)
	state := getAgentInstance(t, c, w)
	if !state.Status.Provisioning.StartedAt.Equal(started) {
		t.Fatal("creation retry reset the allocation clock")
	}
}

func TestAgentInstancePoolTimeoutCannotAllocateAgain(t *testing.T) {
	w, c, scheme := setupPooledAgentInstance(t)
	anchor := time.Now().UTC()
	r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return anchor }}
	reconcileWithUpstreamSimulation(t, r, w)
	r = &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return anchor.Add(2 * time.Minute) }}
	for i := 0; i < 3; i++ {
		reconcileWithUpstreamSimulation(t, r, w)
	}
	state := getAgentInstance(t, c, w)
	if state.Status.Provisioning.TimedOutAt == nil {
		t.Fatal("timeout was not latched")
	}
	claim := desiredPoolClaim(w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), claim); !apierrors.IsNotFound(err) {
		t.Fatalf("timed out Claim was recreated: %v", err)
	}
	assertNoSandbox(t, c, w)
}

func TestAllocationCompletesWithoutCoreReadiness(t *testing.T) {
	w, c, scheme := setupPooledAgentInstance(t)
	now := time.Now().UTC()
	r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
	reconcileWithUpstreamSimulation(t, r, w)
	sb := assignPoolSandbox(t, c, w, false)
	// A delayed reconciliation must observe assignment before enforcing the budget.
	now = now.Add(2 * time.Minute)
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentInstance(t, c, w)
	if state.Status.Provisioning.CompletedAt == nil {
		t.Fatal("owned, matching allocation still waits for Core Ready")
	}
	testutil.Condition(t, state.Status.Conditions, "ProvisioningReady", metav1.ConditionTrue)
	completed := state.Status.Provisioning.CompletedAt.DeepCopy()
	now = now.Add(10 * time.Minute)
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentInstance(t, c, w)
	if state.Status.Provisioning.TimedOutAt != nil || !state.Status.Provisioning.CompletedAt.Equal(completed) {
		t.Fatal("startup delay reset or expired completed allocation")
	}
	getClaim(t, c, w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(sb), sb); err != nil {
		t.Fatal("unready execution was removed", err)
	}
	// An observation gap after assignment must not reopen the old budget either.
	if err := c.Delete(ctx, sb); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if getAgentInstance(t, c, w).Status.Provisioning.TimedOutAt != nil {
		t.Fatal("missing assigned Sandbox reopened the completed allocation budget")
	}
	getClaim(t, c, w)
}

func TestAllocationTimeoutSurvivesBlockedDependencies(t *testing.T) {
	for _, dependency := range []string{"group", "template"} {
		t.Run(dependency, func(t *testing.T) {
			w, c, scheme := setupPooledAgentInstance(t)
			now := time.Now().UTC()
			r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
			reconcileWithUpstreamSimulation(t, r, w)
			if dependency == "group" {
				group := &v1.AgentGroup{}
				if err := c.Get(ctx, client.ObjectKey{Name: w.Spec.AgentGroupRef.Name}, group); err != nil {
					t.Fatal(err)
				}
				group.Status.Conditions[0].Status = metav1.ConditionFalse
				if err := c.Status().Update(ctx, group); err != nil {
					t.Fatal(err)
				}
			} else {
				template := sandbox.ExtensionObject("SandboxTemplate")
				template.SetName(getAgentInstance(t, c, w).Status.Definition.TemplateName)
				template.SetNamespace(w.Namespace)
				if err := c.Delete(ctx, template); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(2 * time.Minute)
			reconcileWithUpstreamSimulation(t, r, w)
			if getAgentInstance(t, c, w).Status.Provisioning.TimedOutAt == nil {
				t.Fatal("dependency failure bypassed the persisted allocation deadline")
			}
			claim := desiredPoolClaim(w)
			if err := c.Get(ctx, client.ObjectKeyFromObject(claim), claim); !apierrors.IsNotFound(err) {
				t.Fatalf("expired native Claim was retained: %v", err)
			}
		})
	}
}

func TestSuspensionPrecedesUnsettledConfigurationValidation(t *testing.T) {
	w, c, scheme := setupPooledAgentInstance(t)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	sb := assignPoolSandbox(t, c, w, false)
	resolved, err := resolveSandboxTemplate(ctx, c, w)
	if err != nil {
		t.Fatal(err)
	}
	resolved.PodTemplate.Spec.Containers[0].Image = "example/agent:v2"
	updateTemplate(t, c, w, resolved.PodTemplate)
	state := getAgentInstance(t, c, w)
	state.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(sb), sb); err != nil {
		t.Fatal(err)
	}
	mode, _, _ := unstructured.NestedString(sb.Object, "spec", "operatingMode")
	if mode != sandbox.Suspended {
		t.Fatal("desired configuration drift prevented suspension of the owned execution")
	}
}

func TestAgentInstanceFailedPoolExecutionIsRetainedBeyondAllocationTimeout(t *testing.T) {
	w, c, scheme := setupPooledAgentInstance(t)
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC()
	r := &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return anchor }}
	reconcileWithUpstreamSimulation(t, r, w)
	sb := assignPoolSandbox(t, c, w, false)
	current, err := sandbox.Decode(sb)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sb.GetName(), Namespace: w.Namespace, UID: "failed-pod", OwnerReferences: []metav1.OwnerReference{{APIVersion: sandbox.GroupVersion.String(), Kind: "Sandbox", Name: sb.GetName(), UID: sb.GetUID(), Controller: ptr.To(true)}}}, Spec: current.Spec.PodTemplate.Spec}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodFailed
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	sb.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Finished", "status": "True", "reason": "PodFailed", "observedGeneration": sb.GetGeneration(), "lastTransitionTime": anchor.Format(time.RFC3339)}}}
	if err := c.Status().Update(ctx, sb); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	r = &Reconciler{Client: c, Scheme: scheme, Now: func() time.Time { return anchor.Add(2 * time.Minute) }}
	reconcileWithUpstreamSimulation(t, r, w)
	getClaim(t, c, w)
	assertNoSandbox(t, c, w)
	state := getAgentInstance(t, c, w)
	if state.Status.Provisioning.Mode != "Pool" || state.Status.Lifecycle.FinishedReason != "PodFailed" {
		t.Fatal("allocation timeout discarded failed execution")
	}
}

func TestAgentInstancePoolSuspensionSurvivesMissingCredentials(t *testing.T) {
	group, ns, definition, w, source := testutil.Fixture()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: w.Namespace}, Data: map[string][]byte{"token": []byte("fixture")}}
	source.Spec.Containers[0].Env = append(source.Spec.Containers[0].Env, secretReference(secret.Name, "token", false))
	c, scheme := clientWithPublishedDefinitions(t, definition, group, ns, w, secret, nativeTemplate(definition, source))
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	sb := getSandbox(t, c, w)
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	state := getAgentInstance(t, c, w)
	state.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if err := c.Get(ctx, client.ObjectKeyFromObject(sb), sb); err != nil {
		t.Fatal(err)
	}
	mode, _, _ := unstructured.NestedString(sb.Object, "spec", "operatingMode")
	if mode != sandbox.Suspended {
		t.Fatal("missing credentials prevented pool suspension")
	}
	assertReason(t, getAgentInstance(t, c, w), "ConfigurationAccepted", "SuspendedUsingExistingConfiguration")
}

func TestSuspensionSurvivesUnreadyGroup(t *testing.T) {
	group, ns, definition, w, template := testutil.Fixture()
	kube, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, template), group, ns, w)
	r := &Reconciler{Client: kube, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil {
		t.Fatal(err)
	}
	group.Status.Conditions[0].Status = metav1.ConditionFalse
	group.Status.Conditions[0].Reason = "QuotaPending"
	if err := kube.Status().Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	state := getAgentInstance(t, kube, w)
	state.Spec.OperatingMode = sandbox.Suspended
	if err := kube.Update(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w)}); err != nil {
		t.Fatal(err)
	}
	object, err := r.existingExecution(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := sandbox.Decode(object)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("existing execution was not suspended")
	}
}
