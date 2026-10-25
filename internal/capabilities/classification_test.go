package capabilities

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRuntimeClassificationRequiresMatchingLabelsAndExecutor(t *testing.T) {
	node := linuxNode("worker")
	runtime := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "isolated"}, Handler: "kata-qemu-tdx-runtime-rs",
		Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"isolated": ""}}}
	if matchesRuntimeNode(*node, runtime) {
		t.Fatal("missing selector label was treated as an empty-valued label")
	}
	node.Labels["isolated"] = ""
	if !matchesRuntimeNode(*node, runtime) {
		t.Fatal("present empty-valued selector label did not match")
	}
	for _, test := range []struct{ handler, category string }{
		{"kata-qemu-tdx-runtime-rs", "VirtualMachine"}, {"kata-qemu-coco-dev", "VirtualMachine"},
		{"kata-remote", "VirtualMachine"}, {"spin", "WebAssembly"},
		{"containerd", "Unknown"}, {"cri-o", "Unknown"}, {"custom-kata-lookalike", "Unknown"},
	} {
		t.Run(test.handler, func(t *testing.T) {
			runtime.Handler = test.handler
			observed := findRuntime(t, Discover(t.Context(), testClient(t, node, runtime)), runtime.Name)
			if observed.Category != test.category {
				t.Fatalf("handler category: %+v", observed)
			}
		})
	}
}

func TestStorageClassificationUsesKubernetesMountInterface(t *testing.T) {
	for _, test := range []struct {
		provisioner, key, value, kind string
	}{
		{"nfs.csi.k8s.io", "", "", "Filesystem"},
		{"efs.csi.aws.com", "", "", "Filesystem"},
		{"rook-ceph.cephfs.csi.ceph.com", "", "", "Filesystem"},
		{"smb.csi.k8s.io", "", "", "Filesystem"},
		{"file.csi.azure.com", "Protocol", "nfs", "Filesystem"},
		{"csi.san.synology.com", "protocol", "iscsi", "Filesystem"},
		{"csi.san.synology.com", "protocol", "nfs", "Filesystem"},
		{"ebs.csi.eks.amazonaws.com", "", "", "Filesystem"},
		{"hostpath.csi.k8s.io", "", "", "Filesystem"},
		{"rancher.io/local-path", "", "", "Filesystem"},
		{"s3.csi.aws.com", "", "", "Filesystem"},
		{"csi.vsphere.vmware.com", "", "", "Filesystem"},
		{"csi.trident.netapp.io", "", "", "Filesystem"},
		{"everest-csi-provisioner", "csi.storage.k8s.io/csi-driver-name", "disk.csi.everest.io", "Filesystem"},
		{"everest-csi-provisioner", "csi.storage.k8s.io/csi-driver-name", "obs.csi.everest.io", "Filesystem"},
		{"everest-csi-provisioner", "", "", "Unknown"},
		{"kubernetes.io/no-provisioner", "", "", "Unknown"},
		{"unrelated-nfs-client.example.com", "", "", "Unknown"},
		{"unrelated-local-path.example.com", "", "", "Unknown"},
	} {
		t.Run(test.provisioner+test.key+test.value, func(t *testing.T) {
			sc := class("workspace", test.provisioner)
			if test.key != "" {
				sc.Parameters = map[string]string{test.key: test.value}
			}
			if got := classify(*sc); got != test.kind {
				t.Fatalf("Kubernetes interface classification: got %s, want %s", got, test.kind)
			}
		})
	}
}

func TestAggregateStorageProvisionerUsesSelectedNodeDriver(t *testing.T) {
	sc := class("workspace", "everest-csi-provisioner")
	sc.Parameters = map[string]string{"csi.storage.k8s.io/csi-driver-name": "disk.csi.everest.io"}
	registration := csiRegistration("disk.csi.everest.io")
	observed := findStorage(t, Discover(t.Context(), testClient(t, sc, registration)), sc.Name)
	if observed.Type != "Filesystem" || observed.CSIDriver != "disk.csi.everest.io" || observed.DriverDeclared || !observed.DriverRegistered || observed.Status != "Observed" {
		t.Fatalf("aggregate provisioner was confused with the node driver: %+v", observed)
	}
	sc.Parameters["csi.storage.k8s.io/csi-driver-name"] = "unrecognized-value"
	observed = findStorage(t, Discover(t.Context(), testClient(t, sc, registration)), sc.Name)
	if observed.Type != "Unknown" || observed.CSIDriver != "" || observed.DriverRegistered {
		t.Fatal("unrecognized driver selection inherited a different backend's evidence", observed)
	}
}

func TestStorageDistinguishesDriverDeclarationAndNodeRegistration(t *testing.T) {
	for _, test := range []struct {
		name                 string
		declared, registered bool
	}{
		{"declaration only", true, false}, {"registration only", false, true}, {"both", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sc := class("workspace", "nfs.csi.k8s.io")
			objects := []client.Object{sc}
			if test.declared {
				objects = append(objects, driver(sc.Provisioner))
			}
			if test.registered {
				objects = append(objects, csiRegistration(sc.Provisioner))
			}
			status := Discover(t.Context(), testClient(t, objects...))
			observed := findStorage(t, status, sc.Name)
			if observed.DriverDeclared != test.declared || observed.DriverRegistered != test.registered || (observed.Status == "Observed") != test.registered {
				t.Fatalf("declaration/registration conflated: %+v", observed)
			}
			if len(status.CSIDriverDetails) != 1 || status.CSIDriverDetails[0].Declared != test.declared {
				t.Fatalf("node-only driver inventory lost: %+v", status.CSIDriverDetails)
			}
		})
	}
	unknown := findStorage(t, Discover(t.Context(), testClient(t, class("custom", "example.com/external-provisioner"))), "custom")
	if unknown.CSIDriver != "" || unknown.DriverDeclared || unknown.DriverRegistered {
		t.Fatal("unrecognized external provisioner was asserted to be a CSI driver", unknown)
	}
}

func csiRegistration(name string) *storagev1.CSINode {
	return &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: storagev1.CSINodeSpec{
		Drivers: []storagev1.CSINodeDriver{{Name: name, NodeID: "private-node-id"}},
	}}
}

func ciliumDaemon(config string) *appsv1.DaemonSet {
	ds := daemon("cilium", "cilium-agent")
	ds.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "cilium-config", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: config}},
	}}}
	ds.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "cilium-config", MountPath: "/tmp/cilium/config-map"}}
	ds.Spec.Template.Spec.Containers[0].Args = []string{"--config-dir=/tmp/cilium/config-map"}
	return ds
}

func TestNetworkPolicyConfigurationUsesEffectiveArguments(t *testing.T) {
	for _, test := range []struct {
		name, container, want string
		command, args         []string
	}{
		{name: "kube-router", container: "kube-router", args: []string{"--run-firewall=true", "--run-firewall=false"}, want: "Unsupported"},
		{name: "kube-router", container: "kube-router", command: []string{"/usr/local/bin/kube-router", "--run-firewall=false"}, want: "Unsupported"},
		{name: "aws-node", container: "aws-eks-nodeagent", args: []string{"--enable-network-policy=true", "--enable-network-policy=false"}, want: "Unsupported"},
		{name: "cilium", container: "cilium-agent", args: []string{"--enable-policy", "never"}, want: "Unsupported"},
		{name: "cilium", container: "cilium-agent", args: []string{"--enable-k8s-networkpolicy=false", "--enable-k8s-networkpolicy=true"}, want: "Supported"},
		{name: "cilium", container: "cilium-agent", args: []string{"--enable-policy", "unrecognized"}, want: "Unknown"},
		{name: "kube-router", container: "kube-router", command: []string{"sh", "-c", "exec kube-router --run-firewall=false"}, want: "Unknown"},
	} {
		t.Run(test.name+test.container+test.want, func(t *testing.T) {
			ds := daemon(test.name, test.container)
			if test.name == "cilium" {
				ds = ciliumDaemon("cilium-config")
			}
			ds.Spec.Template.Spec.Containers[0].Command = test.command
			ds.Spec.Template.Spec.Containers[0].Args = append(ds.Spec.Template.Spec.Containers[0].Args, test.args...)
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}, Data: map[string]string{"enable-policy": "default"}}
			observed := Discover(t.Context(), testClient(t, ds, cm)).NetworkPolicy
			if len(observed.Providers) != 1 || observed.Providers[0].Status != test.want || observed.EnforcementVerified {
				t.Fatalf("effective provider configuration: %+v", observed)
			}
		})
	}
}

func TestCiliumDoesNotAssumeHowMountedConfigurationIsLoaded(t *testing.T) {
	for _, scenario := range []string{"unused mount", "projected keys", "binary data", "extra config file", "unresolved config file", "shadowed policy file"} {
		t.Run(scenario, func(t *testing.T) {
			ds := ciliumDaemon("cilium-config")
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: ds.Namespace}, Data: map[string]string{"enable-policy": "default"}}
			switch scenario {
			case "unused mount":
				ds.Spec.Template.Spec.Containers[0].Args = nil
			case "projected keys":
				cm.Data = map[string]string{"custom": "never"}
				ds.Spec.Template.Spec.Volumes[0].ConfigMap.Items = []corev1.KeyToPath{{Key: "custom", Path: "enable-policy"}}
			case "binary data":
				cm.BinaryData = map[string][]byte{"enable-k8s-networkpolicy": []byte("false")}
			case "extra config file":
				ds.Spec.Template.Spec.Containers[0].Args = append(ds.Spec.Template.Spec.Containers[0].Args, "--config=/etc/cilium/config.yaml")
			case "unresolved config file":
				ds.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CILIUM_CONFIG", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "other"}, Key: "path"}}}}
			case "shadowed policy file":
				ds.Spec.Template.Spec.Containers[0].VolumeMounts = append(ds.Spec.Template.Spec.Containers[0].VolumeMounts,
					corev1.VolumeMount{Name: "cilium-config", MountPath: "/tmp/cilium/config-map/policy-audit-mode", SubPath: "audit"})
			}
			observed := Discover(t.Context(), testClient(t, ds, cm)).NetworkPolicy
			if observed.Status != "Unknown" || len(observed.Providers) != 1 || observed.Providers[0].Status != "Unknown" {
				t.Fatal("unresolved effective configuration became positive evidence", observed)
			}
		})
	}
}

func TestNetworkRecognitionHandlesRenamedAndCoexistingProviders(t *testing.T) {
	renamed := ciliumDaemon("custom-cilium-config")
	renamed.Name = "renamed-agent"
	renamed.Spec.Template.Labels = map[string]string{"k8s-app": "cilium"}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "custom-cilium-config", Namespace: renamed.Namespace}, Data: map[string]string{"enable-policy": "default"}}
	for _, ds := range []*appsv1.DaemonSet{renamed, daemon("kube-network-policies", "kube-network-policies"), daemon("kube-flannel-ds", "kube-network-policies")} {
		t.Run(ds.Name, func(t *testing.T) {
			observed := Discover(t.Context(), testClient(t, ds, config)).NetworkPolicy
			if observed.Status != "Supported" || observed.EnforcementVerified || len(observed.Providers) != 1 {
				t.Fatalf("documented policy installation not recognized: %+v", observed)
			}
		})
	}
	observed := Discover(t.Context(), testClient(t, daemon("kindnet", "kindnet-cni"))).NetworkPolicy
	if len(observed.Providers) != 1 || observed.Providers[0].Status != "Unknown" {
		t.Fatal("version-dependent kindnet implementation became an unconditional negative")
	}
	// A missing optional Cilium config must not erase another ready provider.
	observed = Discover(t.Context(), testClient(t, ciliumDaemon("missing"), daemon("calico-node", "calico-node"))).NetworkPolicy
	if observed.Status != "Supported" || len(observed.Providers) != 2 {
		t.Fatal("missing optional provider configuration hid a coexisting engine", observed)
	}
}
