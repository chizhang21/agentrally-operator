package agentinstance

import (
	"context"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSnapshotSourcePreflightAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(*corev1.PersistentVolumeClaimSpec, *unstructured.Unstructured)
	}{
		{"local snapshot", "", nil},
		{"dataSourceRef", "", func(spec *corev1.PersistentVolumeClaimSpec, _ *unstructured.Unstructured) {
			spec.DataSource = nil
			spec.DataSourceRef = &corev1.TypedObjectReference{APIGroup: ptr.To(snapshotAPIGroup), Kind: "VolumeSnapshot", Name: "seed"}
		}},
		{"unknown restore size", "", func(_ *corev1.PersistentVolumeClaimSpec, s *unstructured.Unstructured) {
			unstructured.RemoveNestedField(s.Object, "status", "restoreSize")
		}},
		{"not ready", "SnapshotNotReady", func(_ *corev1.PersistentVolumeClaimSpec, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, false, "status", "readyToUse")
		}},
		{"missing", "SnapshotNotFound", func(_ *corev1.PersistentVolumeClaimSpec, s *unstructured.Unstructured) { s.SetName("other") }},
		{"terminating", "DependencyTerminating", func(_ *corev1.PersistentVolumeClaimSpec, s *unstructured.Unstructured) {
			s.SetDeletionTimestamp(ptr.To(metav1.Now()))
			s.SetFinalizers([]string{"snapshot-protection"})
		}},
		{"capacity", "SnapshotCapacityInsufficient", func(spec *corev1.PersistentVolumeClaimSpec, _ *unstructured.Unstructured) {
			spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("1Mi")
		}},
		{"invalid size", "SnapshotInvalid", func(_ *corev1.PersistentVolumeClaimSpec, s *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(s.Object, "invalid", "status", "restoreSize")
		}},
		{"cross namespace", "SnapshotNamespaceUnsupported", func(spec *corev1.PersistentVolumeClaimSpec, _ *unstructured.Unstructured) {
			spec.DataSource = nil
			spec.DataSourceRef = &corev1.TypedObjectReference{APIGroup: ptr.To(snapshotAPIGroup), Kind: "VolumeSnapshot", Name: "seed", Namespace: ptr.To("other")}
		}},
		{"conflicting sources", "InvalidConfiguration", func(spec *corev1.PersistentVolumeClaimSpec, _ *unstructured.Unstructured) {
			spec.DataSourceRef = &corev1.TypedObjectReference{APIGroup: ptr.To(snapshotAPIGroup), Kind: "VolumeSnapshot", Name: "other"}
		}},
		{"custom populator", "", func(spec *corev1.PersistentVolumeClaimSpec, _ *unstructured.Unstructured) {
			spec.DataSource = nil
			spec.DataSourceRef = &corev1.TypedObjectReference{APIGroup: ptr.To("other.io"), Kind: "VolumeSnapshot", Name: "seed"}
		}},
		{"missing API", "SnapshotAPIUnavailable", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group, ns, definition, w, pod := testutil.Fixture()
			volume := workspaceVolumeSpec()
			volume.DataSource = &corev1.TypedLocalObjectReference{APIGroup: ptr.To(snapshotAPIGroup), Kind: "VolumeSnapshot", Name: "seed"}
			snapshot := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{"readyToUse": true, "restoreSize": "32Mi"}}}
			snapshot.SetGroupVersionKind(schema.GroupVersionKind{Group: snapshotAPIGroup, Version: "v1", Kind: "VolumeSnapshot"})
			snapshot.SetName("seed")
			snapshot.SetNamespace(ns.Name)
			snapshot.SetUID("snapshot-uid")
			if tc.mutate != nil {
				tc.mutate(&volume, snapshot)
			}
			w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: &volume, MountPath: "/workspace"}
			builder, scheme := testutil.ClientBuilder(t)
			scheme.AddKnownTypeWithName(snapshot.GroupVersionKind(), &unstructured.Unstructured{})
			c := builder.WithObjects(definition, group, ns, w, nativeTemplate(definition, pod), snapshot).Build()
			reader := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
				if tc.name == "custom populator" && object.GetObjectKind().GroupVersionKind() == snapshot.GroupVersionKind() {
					t.Fatal("custom populator accessed the native VolumeSnapshot API")
				}
				if tc.name == "missing API" && object.GetObjectKind().GroupVersionKind() == snapshot.GroupVersionKind() {
					return &meta.NoKindMatchError{GroupKind: snapshot.GroupVersionKind().GroupKind()}
				}
				return c.Get(ctx, key, object, opts...)
			}})
			r := &Reconciler{Client: c, Reader: reader, Scheme: scheme}
			before := w.DeepCopy()
			reconcileAllocation(t, r, w)
			state := getAgentInstance(t, c, w)
			if tc.reason != "" {
				assertReason(t, state, "Ready", tc.reason)
				if state.Status.Provisioning != nil {
					t.Fatal("invalid snapshot started allocation")
				}
				return
			}
			if state.Status.Provisioning == nil {
				t.Fatalf("valid source did not start allocation: %+v", state.Status.Conditions)
			}
			template := allocationObject(t, c, w, "SandboxTemplate", state.Status.Provisioning.TemplateName)
			claims, err := sandbox.TemplateVolumeClaims(template)
			if err != nil || len(claims) != 1 || !reflect.DeepEqual(claims[0].Spec, volume) || !reflect.DeepEqual(before.Spec, getAgentInstance(t, c, w).Spec) {
				t.Fatal("snapshot source was not preserved in the native PVC template", err)
			}
			if tc.name == "custom populator" {
				return
			}
			config, err := desiredExecution(ctx, c, w)
			if err != nil {
				t.Fatal(err)
			}
			first, err := configRevision(ctx, c, ns.Name, *config)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.SetResourceVersion("")
			snapshot.SetUID("replacement-uid")
			if err := c.Create(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			next, err := configRevision(ctx, c, ns.Name, *config)
			if err != nil || first == next {
				t.Fatal("recreated snapshot reused old pool identity", err)
			}
		})
	}
	// No optional API access is needed for normal PVC templates.
	refs, err := snapshotReferences(ctx, nil, "agents", []corev1.PersistentVolumeClaimTemplate{{Spec: workspaceVolumeSpec()}}, nil)
	if err != nil || len(refs) != 0 {
		t.Fatal("ordinary PVCs acquired a snapshot API dependency", err)
	}
}

func TestSnapshotSourceLifecycle(t *testing.T) {
	for _, scenario := range []string{"deleted", "unready", "recreated"} {
		t.Run(scenario, func(t *testing.T) {
			group, ns, definition, w, pod := testutil.Fixture()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: ns.Name}, Data: map[string][]byte{"token": []byte("initial")}}
			pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, secretReference(secret.Name, "token", false))
			volume := workspaceVolumeSpec()
			volume.DataSource = &corev1.TypedLocalObjectReference{APIGroup: ptr.To(snapshotAPIGroup), Kind: "VolumeSnapshot", Name: "seed"}
			w.Spec.Workspace = &v1.WorkspaceMount{VolumeClaimTemplate: &volume, MountPath: "/workspace"}
			snapshot := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{"readyToUse": true, "restoreSize": "32Mi"}}}
			snapshot.SetGroupVersionKind(schema.GroupVersionKind{Group: snapshotAPIGroup, Version: "v1", Kind: "VolumeSnapshot"})
			snapshot.SetName("seed")
			snapshot.SetNamespace(ns.Name)
			snapshot.SetUID("snapshot-uid")
			builder, scheme := testutil.ClientBuilder(t)
			scheme.AddKnownTypeWithName(snapshot.GroupVersionKind(), &unstructured.Unstructured{})
			c := builder.WithObjects(definition, group, ns, w, secret, nativeTemplate(definition, pod), snapshot).Build()
			r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			state := getAgentInstance(t, c, w)
			if state.Status.Provisioning == nil || state.Status.Provisioning.CompletedAt == nil {
				t.Fatal("allocation did not complete")
			}
			initial := state.DeepCopy()
			state.Spec.OperatingMode = sandbox.Suspended
			if err := c.Update(ctx, state); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, state)
			if scenario == "unready" {
				_ = unstructured.SetNestedField(snapshot.Object, false, "status", "readyToUse")
				if err := c.Update(ctx, snapshot); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Delete(ctx, snapshot); err != nil {
					t.Fatal(err)
				}
				if scenario == "recreated" {
					snapshot.SetResourceVersion("")
					snapshot.SetUID("replacement-snapshot-uid")
					if err := c.Create(ctx, snapshot); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Use a fresh reconciler to ensure recovery uses persisted creation input.
			r = &Reconciler{Client: c, Reader: c, Scheme: scheme}
			state = getAgentInstance(t, c, w)
			state.Spec.OperatingMode = sandbox.Running
			if err := c.Update(ctx, state); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, state)
			object := getSandbox(t, c, w)
			mode, _, _ := unstructured.NestedString(object.Object, "spec", "operatingMode")
			if mode != sandbox.Running {
				t.Fatalf("snapshot source %s blocked resume: mode=%s conditions=%+v", scenario, mode, getAgentInstance(t, c, w).Status.Conditions)
			}
			ownedExecutionPod(t, c, w)
			nativeReady(t, c, object)
			reconcileWithUpstreamSimulation(t, r, state)
			state = getAgentInstance(t, c, w)
			testutil.Condition(t, state.Status.Conditions, "ConfigurationAccepted", metav1.ConditionTrue)
			testutil.Condition(t, state.Status.Conditions, "ConfigurationApplied", metav1.ConditionTrue)
			if state.Status.DesiredRevision != initial.Status.DesiredRevision || state.Status.SandboxName != initial.Status.SandboxName || !reflect.DeepEqual(state.Status.VolumeClaims, initial.Status.VolumeClaims) {
				t.Fatal("source lifecycle changed the existing execution's identity or workspace")
			}

			// A new allocation must inspect the current source, even with a shared pool.
			fresh := w.DeepCopy()
			fresh.Name, fresh.UID, fresh.ResourceVersion = "new-agent", "new-claim-uid", ""
			fresh.Status = v1.AgentInstanceStatus{}
			if err := c.Create(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			reconcileAllocation(t, r, fresh)
			newState := getAgentInstance(t, c, fresh)
			switch scenario {
			case "deleted":
				assertReason(t, newState, "Ready", "SnapshotNotFound")
			case "unready":
				assertReason(t, newState, "Ready", "SnapshotNotReady")
			case "recreated":
				if newState.Status.Provisioning == nil || newState.Status.Provisioning.PoolName == initial.Status.Provisioning.PoolName || newState.Status.DesiredRevision == initial.Status.DesiredRevision {
					t.Fatal("a recreated source reused old warmed storage")
				}
			}
			if scenario != "recreated" && newState.Status.Provisioning != nil {
				t.Fatal("unavailable initialization source started a new allocation")
			}

			if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
				t.Fatal(err)
			}
			secret.Data["token"] = []byte("rotated")
			if err := c.Update(ctx, secret); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, state)
			state = getAgentInstance(t, c, w)
			assertReason(t, state, "ConfigurationApplied", "RestartRequired")
			if state.Status.DesiredRevision == initial.Status.DesiredRevision {
				t.Fatal("retaining initialization sources hid credential rotation")
			}
		})
	}
}
