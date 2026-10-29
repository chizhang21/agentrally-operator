package capabilities

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNetworkCatalogFlagsAndControllerPresence(t *testing.T) {
	for _, tc := range []struct {
		name, container string
		args            []string
		want            string
	}{
		{"aws-node", "aws-network-policy-agent", []string{"--enable-network-policy=true"}, "Supported"},
		{"aws-node", "aws-eks-nodeagent", nil, "Unknown"},
		{"aws-node", "aws-node", nil, "Unsupported"},
		{"kube-router", "kube-router", nil, "Supported"},
		{"kube-router", "kube-router", []string{"--run-firewall=true"}, "Supported"},
		{"weave-net", "weave", nil, "Unsupported"},
		{"weave-net", "weave-npc", nil, "Supported"},
		{"cilium", "cilium-agent", []string{"--enable-k8s-networkpolicy=false"}, "Unsupported"},
	} {
		t.Run(tc.name+tc.container+tc.want, func(t *testing.T) {
			ds := daemon(tc.name, tc.container)
			if tc.name == "cilium" {
				ds = ciliumDaemon("cilium-config")
			}
			ds.Spec.Template.Spec.Containers[0].Args = append(ds.Spec.Template.Spec.Containers[0].Args, tc.args...)
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}}
			s := Discover(context.Background(), testClient(t, ds, cm))
			want := tc.want
			if want == "Unsupported" {
				want = "Unknown"
			}
			if s.NetworkPolicy.Status != want || s.NetworkPolicy.EnforcementVerified {
				t.Fatalf("configuration evidence: %+v", s.NetworkPolicy)
			}
			if len(s.NetworkPolicy.Providers) != 1 || s.NetworkPolicy.Providers[0].Status != tc.want {
				t.Fatalf("provider-scoped result lost: %+v", s.NetworkPolicy.Providers)
			}
		})
	}
}
