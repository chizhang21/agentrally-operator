package capabilities

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeAliasesAndUnrecognizedHandlers(t *testing.T) {
	for _, tc := range []struct{ handler, family, contract string }{
		{"kata-clh-azure-runtime-rs", "kata", "LinuxOCI"}, {"kata-dragonball", "kata", "LinuxOCI"},
		{"runsc-systrap", "gvisor", "LinuxOCI"}, {"wamr", "wamr", "WasmWASI"},
		{"kata-custom-unknown", "unknown", "Unknown"}, {"opaque", "unknown", "Unknown"},
	} {
		got := describeRuntime(nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"arally.io/runtime-type": "kata"}}, Handler: tc.handler})
		if got.Family != tc.family || got.ImageContract != tc.contract {
			t.Fatalf("%s: %+v", tc.handler, got)
		}
	}
}

func TestNetworkCatalogFlagsAndControllerPresence(t *testing.T) {
	for _, tc := range []struct {
		name, container string
		args            []string
		want            string
	}{
		{"aws-node", "aws-network-policy-agent", []string{"--enable-network-policy=true"}, "Supported"},
		{"aws-node", "aws-eks-nodeagent", nil, "Unsupported"},
		{"aws-node", "aws-node", nil, "Unsupported"},
		{"kube-router", "kube-router", nil, "Unknown"},
		{"kube-router", "kube-router", []string{"--run-firewall=true"}, "Supported"},
		{"weave-net", "weave", nil, "Unsupported"},
		{"weave-net", "weave-npc", nil, "Supported"},
		{"cilium", "cilium-agent", []string{"--enable-k8s-networkpolicy=false"}, "Unsupported"},
	} {
		t.Run(tc.name+tc.container+tc.want, func(t *testing.T) {
			ds := daemon(tc.name, tc.container)
			ds.Spec.Template.Spec.Containers[0].Args = tc.args
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}}
			s := Discover(context.Background(), testClient(t, ds, cm))
			if s.NetworkPolicy.Status != tc.want || s.NetworkPolicy.EnforcementVerified {
				t.Fatalf("configuration evidence: %+v", s.NetworkPolicy)
			}
		})
	}
}
