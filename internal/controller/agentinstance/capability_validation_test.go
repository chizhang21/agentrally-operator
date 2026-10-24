package agentinstance

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"
	"arally/operator/internal/controller/agentgroup"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func capabilityReport(network string) *v1.ClusterCapabilities {
	now := metav1.Now()
	return &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: 1}, Status: v1.ClusterCapabilitiesStatus{
		ObservedGeneration: 1, ObservedAt: &now,
		Sections: []v1.DiscoverySectionObservation{{Name: "RuntimeDiscoveryReady", LastAttemptAt: &now, LastSuccessAt: &now}, {Name: "StorageDiscoveryReady", LastAttemptAt: &now, LastSuccessAt: &now}, {Name: "NetworkPolicyDiscoveryReady", LastAttemptAt: &now, LastSuccessAt: &now}}, NetworkPolicy: &v1.NetworkPolicyObservation{Status: network},
		Runtimes:       []v1.RuntimeObservation{{Name: "kata", Handler: "kata-qemu", Family: "kata", Status: "Observed", EligibleNodes: 1}},
		StorageClasses: []v1.StorageObservation{{Name: "nfs", Provisioner: "nfs.csi.k8s.io", Type: "NFS", Status: "Observed", DriverRegistered: true}},
		Conditions: []metav1.Condition{
			{Type: "RuntimeDiscoveryReady", Status: metav1.ConditionTrue, ObservedGeneration: 1},
			{Type: "StorageDiscoveryReady", Status: metav1.ConditionTrue, ObservedGeneration: 1},
			{Type: "NetworkPolicyDiscoveryReady", Status: metav1.ConditionTrue, ObservedGeneration: 1},
		}}}
}

func TestCapabilitiesBlockMissingRuntimeAndRecover(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	source.Spec.RuntimeClassName = ptr.To("kata")
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w, capabilityReport("Supported"))
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", metav1.ConditionFalse)
	if err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object()); !apierrors.IsNotFound(err) {
		t.Fatal("created Sandbox with missing RuntimeClass", err)
	}
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata-qemu", Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "agents"}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"kubernetes.io/os": "linux", "pool": "agents"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	for _, o := range []client.Object{rc, node} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	reconcileWithUpstreamSimulation(t, r, w)
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", metav1.ConditionTrue)
	getSandbox(t, c, w)
}

func TestCapabilitiesRejectRawBlockWorkspaceWithoutWaitingForBinding(t *testing.T) {
	for _, mode := range []corev1.PersistentVolumeMode{corev1.PersistentVolumeBlock, corev1.PersistentVolumeFilesystem} {
		t.Run(string(mode), func(t *testing.T) {
			g, ns, definition, w, source := testutil.Fixture()
			w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: ptr.To(workspaceVolumeSpec()), MountPath: "/workspace"}
			w.Spec.Workspace.VolumeClaimTemplate.VolumeMode = &mode
			w.Spec.Workspace.VolumeClaimTemplate.StorageClassName = ptr.To("nfs")
			sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: "nfs.csi.k8s.io", VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w, sc, capabilityReport("Supported"))
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object())
			if mode == corev1.PersistentVolumeBlock {
				assertReason(t, getAgentInstance(t, c, w), "Ready", "InvalidConfiguration")
				if !apierrors.IsNotFound(err) {
					t.Fatal("block PVC used as directory", err)
				}
			} else {
				if err != nil {
					t.Fatal("first consumer was blocked", err)
				}
				testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", metav1.ConditionTrue)
			}
		})
	}
}

func TestNetworkEvidenceBlocksOnlyConfirmedUnsupportedNewSandbox(t *testing.T) {
	for _, scenario := range []string{"unsupported", "unknown", "stale", "wrong-generation", "failed-section", "future", "missing", "supported"} {
		t.Run(scenario, func(t *testing.T) {
			g, ns, definition, w, source := testutil.Fixture()
			g.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}}
			report := capabilityReport("Supported")
			switch scenario {
			case "unsupported":
				report.Status.NetworkPolicy.Status = "Unsupported"
			case "unknown":
				report.Status.NetworkPolicy.Status = "Unknown"
			case "stale":
				old := metav1.NewTime(time.Now().Add(-3 * time.Minute))
				report.Status.ObservedAt = &old
				report.Status.Sections[2].LastSuccessAt = &old
				report.Status.NetworkPolicy.Status = "Unsupported"
			case "wrong-generation":
				report.Generation = 2
				report.Status.NetworkPolicy.Status = "Unsupported"
			case "future":
				future := metav1.NewTime(time.Now().Add(time.Minute))
				report.Status.ObservedAt = &future
				report.Status.Sections[2].LastSuccessAt = &future
				report.Status.NetworkPolicy.Status = "Unsupported"
			case "missing":
				report.Status.ObservedAt = nil
			case "failed-section":
				meta.FindStatusCondition(report.Status.Conditions, "NetworkPolicyDiscoveryReady").Status = metav1.ConditionFalse
				report.Status.NetworkPolicy.Status = "Unsupported"
			}
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w, report)
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			want := metav1.ConditionUnknown
			if scenario == "supported" {
				want = metav1.ConditionTrue
			}
			if scenario == "unsupported" {
				want = metav1.ConditionFalse
			}
			testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", want)
			err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object())
			if scenario == "unsupported" {
				if !apierrors.IsNotFound(err) {
					t.Fatal("unsupported policy deployment not blocked", err)
				}
			} else if err != nil {
				t.Fatal("uncertainty incorrectly blocked deployment", err)
			}
		})
	}
}

func TestRuntimeValidationDefersSchedulingToKubernetes(t *testing.T) {
	for _, scenario := range []string{"no-nodes", "selector-conflict", "custom-handler"} {
		t.Run(scenario, func(t *testing.T) {
			g, ns, definition, w, source := testutil.Fixture()
			source.Spec.RuntimeClassName = ptr.To("kata")
			rc := &nodev1.RuntimeClass{
				ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata-qemu",
				Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "agents"}},
			}
			report := capabilityReport("Supported")
			report.Status.Runtimes[0].EligibleNodes = 0
			if scenario == "selector-conflict" {
				source.Spec.NodeSelector = map[string]string{"pool": "other"}
			}
			if scenario == "custom-handler" {
				rc.Handler = "custom"
				report.Status.Runtimes[0].Handler = "custom"
				report.Status.Runtimes[0].Family = "unknown"
			}
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w, rc, report)
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			want := metav1.ConditionTrue
			if scenario == "selector-conflict" {
				want = metav1.ConditionFalse
			}
			if scenario == "custom-handler" {
				want = metav1.ConditionUnknown
			}
			testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", want)
			err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object())
			if want == metav1.ConditionFalse && !apierrors.IsNotFound(err) {
				t.Fatal("contradictory selectors submitted", err)
			}
			if want != metav1.ConditionFalse && err != nil {
				t.Fatal("scheduling observation blocked allocation", err)
			}
		})
	}
}

func TestCapabilityChangesPreserveRunningSandboxAndPermitSuspension(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	g.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{}
	report := capabilityReport("Supported")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-key", Namespace: w.Namespace}, Data: map[string][]byte{"key": []byte("test-key")}}
	definition.Spec.ModelProvider = &v1.ModelProvider{BaseURL: "https://models.example/v1", Model: "test", APIKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name}, Key: "key"}}
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w, report, secret)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	before := getSandbox(t, c, w)
	report.Status.NetworkPolicy.Status = "Unsupported"
	if err := c.Status().Update(ctx, report); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", metav1.ConditionFalse)
	if getSandbox(t, c, w).GetResourceVersion() != before.GetResourceVersion() {
		t.Fatal("capability change mutated running Sandbox")
	}
	// Reject a template change while preserving the existing instance.
	w = getAgentInstance(t, c, w)
	source.Spec.Containers[0].Image = "example/agent:v2"
	updateTemplate(t, c, w, source)
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if getSandbox(t, c, w).GetResourceVersion() != before.GetResourceVersion() {
		t.Fatal("incompatible reconfiguration replaced existing intent")
	}
	w = getAgentInstance(t, c, w)
	source.Spec.Containers[0].Image = "example/agent:v1"
	updateTemplate(t, c, w, source)
	// Missing credentials cannot prevent stopping an already-owned instance.
	w = getAgentInstance(t, c, w)
	if err := c.Delete(ctx, secret); err != nil {
		t.Fatal(err)
	}
	w.Spec.OperatingMode = sandbox.Suspended
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	current, err := sandbox.Decode(getSandbox(t, c, w))
	if err != nil {
		t.Fatal(err)
	}
	if current.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("suspension blocked by dependencies")
	}
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "ConfigurationAccepted", metav1.ConditionFalse)
	w = getAgentInstance(t, c, w)
	secret.ResourceVersion = ""
	if err := c.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	w.Spec.OperatingMode = sandbox.Running
	if err := c.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	current, err = sandbox.Decode(getSandbox(t, c, w))
	if err != nil {
		t.Fatal(err)
	}
	if current.Spec.OperatingMode != sandbox.Suspended {
		t.Fatal("unsupported policy allowed resume")
	}
	report.Status.NetworkPolicy.Status = "Supported"
	if err := c.Status().Update(ctx, report); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	current, err = sandbox.Decode(getSandbox(t, c, w))
	if err != nil {
		t.Fatal(err)
	}
	if current.Spec.OperatingMode != sandbox.Running {
		t.Fatal("resume did not recover")
	}
}

func TestNativeVolumeModesAndBoundPVCWithRemovedClass(t *testing.T) {
	for _, scenario := range []string{"block-device", "filesystem-device", "missing-pvc", "bound-removed-class", "unbound-static-class"} {
		t.Run(scenario, func(t *testing.T) {
			g, ns, definition, w, source := testutil.Fixture()
			pod := &source.Spec
			pod.Volumes = []corev1.Volume{{Name: "mounted-data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
			pod.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "mounted-data", DevicePath: "/dev/data"}}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: w.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{VolumeMode: ptr.To(corev1.PersistentVolumeBlock), StorageClassName: ptr.To("nfs")}}
			sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: "nfs.csi.k8s.io"}
			objects := []client.Object{g, ns, w, capabilityReport("Supported")}
			want := metav1.ConditionTrue
			switch scenario {
			case "filesystem-device":
				pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
				want = metav1.ConditionFalse
			case "missing-pvc":
				want = metav1.ConditionFalse
			case "unbound-static-class":
				want = metav1.ConditionUnknown
			case "bound-removed-class":
				pvc.Status.Phase = corev1.ClaimBound
				want = metav1.ConditionUnknown
			}
			if scenario != "missing-pvc" {
				objects = append(objects, pvc)
			}
			if scenario != "bound-removed-class" && scenario != "unbound-static-class" {
				objects = append(objects, sc)
			}
			c, scheme := clientWithPublishedDefinitions(t, append(objects, definition, nativeTemplate(definition, source))...)
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", want)
			err := c.Get(ctx, client.ObjectKeyFromObject(w), sandbox.Object())
			if want == metav1.ConditionFalse && !apierrors.IsNotFound(err) {
				t.Fatal("invalid volume submitted", err)
			}
			if want != metav1.ConditionFalse && err != nil {
				t.Fatal("usable storage blocked", err)
			}
		})
	}
}

func TestSandboxSchedulingFeedbackIsCurrentAndIndependentOfReady(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	object := getSandbox(t, c, w)
	object.SetGeneration(3)
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	object = getSandbox(t, c, w)
	status := []metav1.Condition{
		{Type: "PodScheduled", Status: metav1.ConditionFalse, ObservedGeneration: 3, Reason: "Unschedulable", Message: "insufficient cpu", LastTransitionTime: metav1.Now()},
		{Type: "Ready", Status: metav1.ConditionUnknown, ObservedGeneration: 3, Reason: "WaitingForPod", LastTransitionTime: metav1.Now()},
	}
	current, err := sandbox.Decode(object)
	if err != nil {
		t.Fatal(err)
	}
	current.Status.Conditions = status
	encoded, err := sandbox.Encode(current)
	if err != nil {
		t.Fatal(err)
	}
	object.Object["status"] = encoded.Object["status"]
	if err := c.Status().Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	observed := getAgentInstance(t, c, w)
	testutil.Condition(t, observed.Status.Conditions, "PodScheduled", metav1.ConditionFalse)
	testutil.Condition(t, observed.Status.Conditions, "Ready", metav1.ConditionUnknown)
	scheduled := meta.FindStatusCondition(observed.Status.Conditions, "PodScheduled")
	if scheduled.Reason != "Unschedulable" || scheduled.Message != "insufficient cpu" || scheduled.ObservedGeneration != w.Generation {
		t.Fatalf("feedback lost: %+v", scheduled)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if getAgentInstance(t, c, w).GetResourceVersion() != observed.GetResourceVersion() {
		t.Fatal("unchanged feedback rewrote status")
	}
	object = getSandbox(t, c, w)
	object.SetGeneration(4)
	if err := c.Update(ctx, object); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	if meta.FindStatusCondition(getAgentInstance(t, c, w).Status.Conditions, "PodScheduled") != nil {
		t.Fatal("stale scheduling feedback retained")
	}
}

func TestGroupNetworkSupportDoesNotConflateResourceReadiness(t *testing.T) {
	for _, tc := range []struct {
		network string
		want    metav1.ConditionStatus
	}{{"Supported", metav1.ConditionTrue}, {"Unsupported", metav1.ConditionFalse}, {"Unknown", metav1.ConditionUnknown}} {
		t.Run(tc.network, func(t *testing.T) {
			g, ns, definition, _, _ := testutil.Fixture()
			g.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{}
			c, scheme := clientWithPublishedDefinitions(t, definition, g, ns, capabilityReport(tc.network))
			r := &agentgroup.Reconciler{Client: c, Scheme: scheme}
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(g)})
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter != controller.DependencyRetry {
				t.Fatal("network evidence not periodically refreshed")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
				t.Fatal(err)
			}
			testutil.Condition(t, g.Status.Conditions, "Ready", metav1.ConditionTrue)
			testutil.Condition(t, g.Status.Conditions, "NetworkPolicySupported", tc.want)
			g.Spec.NetworkPolicy = nil
			if err := c.Update(ctx, g); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(g)}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
				t.Fatal(err)
			}
			if meta.FindStatusCondition(g.Status.Conditions, "NetworkPolicySupported") != nil {
				t.Fatal("removed policy retained support condition")
			}
		})
	}
}

func TestNativeReadFailureRemainsUnknown(t *testing.T) {
	g, ns, definition, w, source := testutil.Fixture()
	source.Spec.RuntimeClassName = ptr.To("kata")
	c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w, capabilityReport("Supported"))
	reader := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if reflect.TypeOf(object) == reflect.TypeOf(&nodev1.RuntimeClass{}) {
			return errors.New("permission denied")
		}
		return c.Get(ctx, key, object, opts...)
	}})
	r := &Reconciler{Client: c, Reader: reader, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	testutil.Condition(t, getAgentInstance(t, c, w).Status.Conditions, "CapabilityValidated", metav1.ConditionUnknown)
	getSandbox(t, c, w)
}
