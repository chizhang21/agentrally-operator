package metrics_test

import (
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVolumeAttributionPinsIdentityAndDeduplicatesAliases(t *testing.T) {
	for _, observedUID := range []types.UID{"pinned", "replacement", ""} {
		t.Run(string(observedUID), func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := v1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			instance := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "agents", UID: "instance"},
				Spec: v1.AgentInstanceSpec{DefinitionRef: v1.AgentDefinitionReference{Name: "desired"}},
				Status: v1.AgentInstanceStatus{Definition: &v1.DefinitionBinding{Name: "accepted", UID: "definition"},
					Workspace:    &v1.WorkspaceBinding{ClaimName: "workspace", UID: "pinned"},
					VolumeClaims: map[string]string{"workspace": "workspace", "alias": "workspace"}}}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance)
			if observedUID != "" {
				builder = builder.WithObjects(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: "agents", UID: observedUID}})
			}
			registry := prometheus.NewRegistry()
			registry.MustRegister(metrics.New(builder.Build()))
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, family := range families {
				if family.GetName() != "agentrally_instance_volume_info" {
					continue
				}
				found = true
				if len(family.Metric) != 1 {
					t.Fatal("volume aliases were counted more than once")
				}
				labels := map[string]string{}
				for _, label := range family.Metric[0].Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["agentdefinition"] != "accepted" || labels["persistentvolumeclaim_uid"] != "pinned" || (labels["resolved"] == "true") != (observedUID == "pinned") {
					t.Fatal("volume attributed to an unaccepted Definition or a replaced/missing claim", labels)
				}
			}
			if !found {
				t.Fatal("expected PVC attribution is missing")
			}
		})
	}
}
