package capabilities

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Cilium's current chart generates configuration into an EmptyDir rather than
// mounting cilium-config directly in the agent.
func generatedCiliumDaemon() *appsv1.DaemonSet {
	ds := daemon("cilium", "cilium-agent")
	ds.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	ds.Spec.Template.Spec.Containers[0].VolumeMounts = mounts
	ds.Spec.Template.Spec.Containers[0].Args = []string{"--config-dir=/tmp/cilium/config-map"}
	ds.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "config", Command: []string{"cilium-dbg", "build-config", "--k8s-api-server-urls="}, VolumeMounts: mounts,
		Env: []corev1.EnvVar{{Name: "CILIUM_K8S_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}},
	}}
	return ds
}

func TestCiliumGeneratedConfigurationUsesSharedPathsAndSources(t *testing.T) {
	for _, scenario := range []string{"default", "disabled", "custom source and destination", "disconnected directory", "shadowed policy file", "unresolved directory", "other writer", "different API source", "repeated source", "unresolved source", "policy override", "legacy policy override", "unrelated override", "unselected override"} {
		t.Run(scenario, func(t *testing.T) {
			ds := generatedCiliumDaemon()
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}, Data: map[string]string{"enable-policy": "default"}}
			init := &ds.Spec.Template.Spec.InitContainers[0]
			objects := []client.Object{ds, cm}
			want := "Supported"
			switch scenario {
			case "disabled":
				cm.Data["enable-policy"], want = "never", "Unsupported"
			case "custom source and destination":
				cm.Name = "custom-config"
				init.Args = []string{"--source=config-map:custom-config", "--dest=/tmp/generated"}
				ds.Spec.Template.Spec.Containers[0].Args = []string{"--config-dir=/tmp/generated"}
			case "disconnected directory":
				init.VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/other"}}
				want = "Unknown"
			case "shadowed policy file":
				ds.Spec.Template.Spec.Containers[0].VolumeMounts = append(ds.Spec.Template.Spec.Containers[0].VolumeMounts,
					corev1.VolumeMount{Name: "tmp", MountPath: "/tmp/cilium/config-map/policy-audit-mode", SubPath: "audit"})
				want = "Unknown"
			case "unresolved directory":
				init.Args = []string{"--dest=/tmp/$(CONFIG_DIR)"}
				ds.Spec.Template.Spec.Containers[0].Args = []string{"--config-dir=/tmp/$(CONFIG_DIR)"}
				want = "Unknown"
			case "other writer":
				ds.Spec.Template.Spec.InitContainers = append(ds.Spec.Template.Spec.InitContainers, corev1.Container{Name: "customize", VolumeMounts: init.VolumeMounts})
				want = "Unknown"
			case "different API source":
				init.Args = []string{"--k8s-kubeconfig-path=/etc/remote-cluster"}
				want = "Unknown"
			case "repeated source":
				init.Args = []string{"--source=node:", "--source=config-map:cilium-config"}
				want = "Unknown"
			case "unresolved source":
				init.Env = append(init.Env, corev1.EnvVar{Name: "CILIUM_SOURCE", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "other"}, Key: "source"}}})
				want = "Unknown"
			case "policy override", "legacy policy override", "unrelated override", "unselected override":
				version := "v2"
				if scenario == "legacy policy override" {
					version = "v2alpha1"
				}
				defaults := map[string]interface{}{"policy-audit-mode": "true"}
				if scenario == "unrelated override" {
					defaults = map[string]interface{}{"debug": "true"}
				} else {
					want = "Unknown"
				}
				spec := map[string]interface{}{"nodeSelector": map[string]interface{}{}, "defaults": defaults}
				if scenario == "unselected override" {
					delete(spec, "nodeSelector")
					want = "Supported"
				}
				objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "cilium.io/" + version, "kind": "CiliumNodeConfig",
					"metadata": map[string]interface{}{"name": "custom-node", "namespace": ds.Namespace}, "spec": spec,
				}})
			}
			reader := testClient(t, objects...)
			if scenario == "legacy policy override" {
				// The fake client does not model served API versions.
				reader = interceptor.NewClient(reader, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if list.GetObjectKind().GroupVersionKind() == (schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNodeConfigList"}) {
						return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "cilium.io", Kind: "CiliumNodeConfig"}, SearchedVersions: []string{"v2"}}
					}
					return c.List(ctx, list, opts...)
				}})
			}
			observed := Discover(t.Context(), reader).NetworkPolicy
			if len(observed.Providers) != 1 || observed.Providers[0].Status != want || observed.EnforcementVerified {
				t.Fatalf("generated configuration: want %s, got %+v", want, observed)
			}
		})
	}
}

func TestCiliumAuditModeIsNotEnforcementConfiguration(t *testing.T) {
	for _, config := range []struct{ key, value, reason string }{
		{"policy-audit-mode", "true", "PolicyAuditModeEnabled"}, {"enable-k8s", "false", "NetworkPolicyDisabled"},
	} {
		t.Run(config.key, func(t *testing.T) {
			ds := ciliumDaemon("cilium-config")
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}, Data: map[string]string{config.key: config.value}}
			observed := Discover(t.Context(), testClient(t, ds, cm)).NetworkPolicy
			if len(observed.Providers) != 1 || observed.Providers[0].Status != "Unsupported" || observed.Providers[0].Reason != config.reason {
				t.Fatal("non-enforcing configuration advertised policy enforcement configuration", observed)
			}
		})
	}
}
