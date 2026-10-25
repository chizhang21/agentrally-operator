package capabilities

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1.AddToScheme, corev1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
func linuxNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelOSStable: "linux", "pool": "agents"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, NodeInfo: corev1.NodeSystemInfo{ContainerRuntimeVersion: "containerd://2.1.0"}}}
}
func class(name, provisioner string) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner}
}
func driver(name string) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: name}}
}
func daemon(name, container string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Generation: 2}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container}}}}}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 1, NumberReady: 1}}
}
func findRuntime(t *testing.T, s v1.ClusterCapabilitiesStatus, name string) v1.RuntimeObservation {
	t.Helper()
	for _, r := range s.Runtimes {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("runtime %s missing", name)
	return v1.RuntimeObservation{}
}
func findStorage(t *testing.T, s v1.ClusterCapabilitiesStatus, name string) v1.StorageObservation {
	t.Helper()
	for _, r := range s.StorageClasses {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("storage %s missing", name)
	return v1.StorageObservation{}
}
func requireCondition(t *testing.T, s v1.ClusterCapabilitiesStatus, name string, want metav1.ConditionStatus) {
	t.Helper()
	condition := meta.FindStatusCondition(s.Conditions, name)
	if condition == nil || condition.Status != want {
		t.Fatalf("%s want %s, got %+v", name, want, condition)
	}
}

func TestRuntimeDiscoveryPreservesFamiliesAndChecksScheduling(t *testing.T) {
	kata := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata-qemu", Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "agents"}}}
	alias := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "alias", Labels: map[string]string{"arally.io/runtime-type": "Kata"}}, Handler: "opaque"}
	unknown := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "unknown"}, Handler: "kata-unrecognized-special"}
	wasm := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "wasm"}, Handler: "wasmedge"}
	gvisor := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}
	tainted := linuxNode("tainted")
	tainted.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "agents", Effect: corev1.TaintEffectNoSchedule}}
	unschedulable := linuxNode("cordoned")
	unschedulable.Spec.Unschedulable = true
	windows := linuxNode("windows")
	windows.Labels[corev1.LabelOSStable] = "windows"
	c := testClient(t, linuxNode("ready"), tainted, unschedulable, windows, kata, alias, unknown, wasm, gvisor)
	status := Discover(context.Background(), c)
	if status.DefaultRuntime.Category != "Unknown" || status.DefaultRuntime.ImageContract != "Unknown" {
		t.Fatal("CRI version was treated as proof of default executor identity")
	}
	if status.DefaultRuntime.EligibleNodes != 1 || findRuntime(t, status, "kata").EligibleNodes != 1 || findRuntime(t, status, "alias").Family != "unknown" {
		t.Fatalf("eligible runtime evidence wrong: %+v", status)
	}
	if r := findRuntime(t, status, "unknown"); r.Status != "Unknown" || r.ImageContract != "Unknown" {
		t.Fatal("unknown handler claimed as an executable backend")
	}
	if r := findRuntime(t, status, "wasm"); r.Category != "WebAssembly" || r.ImageContract != "WasmWASI" {
		t.Fatal("Wasm contract lost")
	}
	if r := findRuntime(t, status, "gvisor"); r.Category != "UserSpaceKernel" {
		t.Fatal("gVisor isolation category lost")
	}
	if len(status.NodeRuntimes) != 1 || status.NodeRuntimes[0].Implementation != "containerd" || status.NodeRuntimes[0].Nodes != 4 {
		t.Fatal("CRI inventory must remain distinct from execution backend")
	}
	kata.Scheduling.Tolerations = []corev1.Toleration{{Key: "dedicated", Value: "agents", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual}}
	if err := c.Update(context.Background(), kata); err != nil {
		t.Fatal(err)
	}
	if got := findRuntime(t, Discover(context.Background(), c), "kata").EligibleNodes; got != 2 {
		t.Fatalf("RuntimeClass toleration ignored: %d", got)
	}
	kata.Scheduling.NodeSelector["pool"] = "missing"
	if err := c.Update(context.Background(), kata); err != nil {
		t.Fatal(err)
	}
	if r := findRuntime(t, Discover(context.Background(), c), "kata"); r.Status != "Unknown" || r.Reason != "NoEligibleNodes" {
		t.Fatal("unschedulable class advertised")
	}
	if err := c.Delete(context.Background(), kata); err != nil {
		t.Fatal(err)
	}
	if len(Discover(context.Background(), c).Runtimes) != 4 {
		t.Fatal("deleted runtime retained")
	}
}

func TestRuntimeDiscoveryDoesNotAssumeComparisonTolerations(t *testing.T) {
	for _, test := range []struct {
		operator corev1.TolerationOperator
		value    string
	}{{corev1.TolerationOpLt, "3"}, {corev1.TolerationOpGt, "1"}} {
		t.Run(string(test.operator), func(t *testing.T) {
			node := linuxNode("tainted")
			node.Spec.Taints = []corev1.Taint{{Key: "capacity", Value: "2", Effect: corev1.TaintEffectNoSchedule}}
			kata := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata-qemu", Scheduling: &nodev1.Scheduling{
				Tolerations: []corev1.Toleration{{Key: "capacity", Operator: test.operator, Value: test.value, Effect: corev1.TaintEffectNoSchedule}},
			}}
			observed := findRuntime(t, Discover(t.Context(), testClient(t, node, kata)), "kata")
			if observed.EligibleNodes != 0 || observed.Status != "Unknown" || observed.Reason != "NoEligibleNodes" {
				t.Fatalf("unverified scheduler feature advertised an eligible runtime: %+v", observed)
			}
		})
	}
}

func TestStorageDiscoveryPreservesNativeEvidenceAndIndependentCSIInventory(t *testing.T) {
	local := class("local", "rancher.io/local-path")
	local.Labels = map[string]string{"arally.io/storage-type": "Block"}
	nfs := class("nfs", "nfs.csi.k8s.io")
	nfs.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	nfs.VolumeBindingMode = ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)
	nfs.ReclaimPolicy = ptr.To(corev1.PersistentVolumeReclaimDelete)
	opaque := class("opaque", "opaque.example")
	opaque.Labels = map[string]string{"arally.io/storage-type": "Block"}
	registration := csiRegistration(nfs.Provisioner)
	c := testClient(t, local, nfs, opaque, class("unknown", "unmapped.example"), class("legacy", "k8s-sigs.io/nfs-subdir-external-provisioner"), driver("nfs.csi.k8s.io"), driver("opaque.example"), registration)
	status := Discover(context.Background(), c)
	if s := findStorage(t, status, "local"); s.Type != "Filesystem" || s.Source != "BuiltInCatalog" || s.CSIDriver != "" || s.Status != "Observed" {
		t.Fatal("local-path directory interface was not recognized")
	}
	if s := findStorage(t, status, "nfs"); s.Type != "Filesystem" || !s.Default || !s.DriverRegistered {
		t.Fatal("native storage attributes lost")
	}
	encoded, err := json.Marshal(status.StorageClasses)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("volumeBindingMode")) || bytes.Contains(encoded, []byte("reclaimPolicy")) {
		t.Fatal("removed StorageClass settings remained in the public inventory")
	}
	if s := findStorage(t, status, "opaque"); s.Type != "Unknown" || s.Source != "NativeAPI" {
		t.Fatal("administrator label unexpectedly changed embedded recognition")
	}
	if s := findStorage(t, status, "legacy"); s.Status != "Observed" || s.DriverRegistered {
		t.Fatal("legacy NFS unnecessarily requires CSIDriver")
	}
	if s := findStorage(t, status, "unknown"); s.Type != "Unknown" || s.Status != "Unknown" {
		t.Fatal("unknown backend silently hidden or inferred")
	}
	if !reflect.DeepEqual(status.CSIDrivers, []string{"nfs.csi.k8s.io", "opaque.example"}) {
		t.Fatal("CSI inventory not stable")
	}
	if err := c.Delete(context.Background(), driver("nfs.csi.k8s.io")); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if s := findStorage(t, Discover(context.Background(), c), "nfs"); s.Status != "Unknown" || s.DriverRegistered || s.Reason != "CSIDriverNotRegistered" {
		t.Fatal("removed CSI driver still advertised as registered")
	}
	if err := c.Delete(context.Background(), nfs); err != nil {
		t.Fatal(err)
	}
	if len(Discover(context.Background(), c).StorageClasses) != 4 {
		t.Fatal("deleted StorageClass retained")
	}
}

func TestNetworkDiscoveryIsConfigurationEvidenceOnly(t *testing.T) {
	for _, tc := range []struct{ name, container, want string }{{"calico-node", "calico-node", "Supported"}, {"ovnkube-node", "ovnkube-controller", "Supported"}, {"antrea-agent", "antrea-agent", "Supported"}, {"kindnet", "kindnet", "Unknown"}, {"kube-flannel-ds", "flannel", "Unknown"}, {"unrecognized", "agent", "Unknown"}, {"calico-node", "wrong-container", "Unknown"}} {
		t.Run(tc.name+tc.container, func(t *testing.T) {
			s := Discover(context.Background(), testClient(t, daemon(tc.name, tc.container)))
			if s.NetworkPolicy.Status != tc.want || s.NetworkPolicy.EnforcementVerified {
				t.Fatalf("wrong policy evidence: %+v", s.NetworkPolicy)
			}
			requireCondition(t, s, "NetworkPolicyDiscoveryReady", metav1.ConditionTrue)
		})
	}
	cilium := ciliumDaemon("cilium-config")
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: cilium.Namespace}, Data: map[string]string{"enable-policy": "never"}}
	c := testClient(t, cilium, config)
	if s := Discover(context.Background(), c); s.NetworkPolicy.Status != "Unknown" || s.NetworkPolicy.Providers[0].Status != "Unsupported" {
		t.Fatal("disabled Cilium advertised")
	}
	coexisting := Discover(context.Background(), testClient(t, daemon("kube-flannel-ds", "flannel"), daemon("custom-policy-engine", "agent")))
	if coexisting.NetworkPolicy.Status != "Unknown" || coexisting.NetworkPolicy.Providers[0].Status != "Unsupported" {
		t.Fatal("negative-only evidence incorrectly excluded an unrecognized coexisting engine")
	}
	config.Data["enable-policy"] = "default"
	if err := c.Update(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if s := Discover(context.Background(), c); s.NetworkPolicy.Status != "Supported" {
		t.Fatal("enabled Cilium not recognized")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cilium), cilium); err != nil {
		t.Fatal(err)
	}
	cilium.Status.ObservedGeneration = 1
	if err := c.Status().Update(context.Background(), cilium); err != nil {
		t.Fatal(err)
	}
	if s := Discover(context.Background(), c); s.NetworkPolicy.Status != "Unknown" {
		t.Fatal("stale provider readiness advertised")
	}
	if err := c.Delete(context.Background(), cilium); err != nil {
		t.Fatal(err)
	}
	if s := Discover(context.Background(), c); s.NetworkPolicy.Status != "Unknown" || len(s.NetworkPolicy.Providers) != 0 {
		t.Fatal("deleted provider retained")
	}
}

func TestSectionFailuresDoNotExposePreviousSuccess(t *testing.T) {
	base := testClient(t, linuxNode("worker"), class("nfs", "nfs.csi.k8s.io"), driver("nfs.csi.k8s.io"), daemon("calico-node", "calico-node"))
	for _, section := range []string{"runtime", "storage", "network"} {
		t.Run(section, func(t *testing.T) {
			reader := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if (section == "runtime" && reflect.TypeOf(list) == reflect.TypeOf(&nodev1.RuntimeClassList{})) || (section == "storage" && reflect.TypeOf(list) == reflect.TypeOf(&storagev1.CSIDriverList{})) || (section == "network" && reflect.TypeOf(list) == reflect.TypeOf(&appsv1.DaemonSetList{})) {
					return errors.New("permission denied")
				}
				return c.List(ctx, list, opts...)
			}})
			s := Discover(context.Background(), reader)
			failed := map[string]Section{"runtime": Runtime, "storage": Storage, "network": Network}[section]
			for _, current := range []Section{Runtime, Storage, Network} {
				want := metav1.ConditionTrue
				if current == failed {
					want = metav1.ConditionFalse
				}
				requireCondition(t, s, current.Condition(), want)
			}
			if section == "runtime" && (s.DefaultRuntime != nil || len(s.Runtimes) != 0 || len(s.NodeRuntimes) != 0) {
				t.Fatal("runtime failure exposed partial inventory")
			}
			if section == "storage" && (len(s.StorageClasses) != 0 || len(s.CSIDrivers) != 0) {
				t.Fatal("storage failure exposed partial inventory")
			}
			if section == "network" && (s.NetworkPolicy.Status != "Unknown" || len(s.NetworkPolicy.Providers) != 0) {
				t.Fatal("network failure exposed partial inventory")
			}
			if section != "storage" && len(s.StorageClasses) != 1 {
				t.Fatal("unrelated storage section lost")
			}
			if section != "runtime" && s.DefaultRuntime == nil {
				t.Fatal("unrelated runtime section lost")
			}
		})
	}
	cilium := ciliumDaemon("cilium-config")
	s := Discover(context.Background(), testClient(t, cilium))
	requireCondition(t, s, "NetworkPolicyDiscoveryReady", metav1.ConditionTrue)
	if s.NetworkPolicy.Status != "Unknown" || len(s.NetworkPolicy.Providers) != 1 || s.NetworkPolicy.Providers[0].Status != "Unknown" {
		t.Fatal("missing CNI configuration was reported as a configured policy engine")
	}
}

// Kubernetes transport/admission errors may include private URLs or response bodies.
func TestDiscoveryDoesNotPublishRawReaderErrors(t *testing.T) {
	base := testClient(t)
	reader := interceptor.NewClient(base, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("private-server-detail: https://internal.example/api?credential=example-only")
	}})
	status := Discover(context.Background(), reader)
	for _, section := range []Section{Runtime, Storage, Network} {
		requireCondition(t, status, section.Condition(), metav1.ConditionFalse)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("private-server-detail")) || bytes.Contains(encoded, []byte("internal.example")) {
		t.Fatal("raw reader error entered the capability report")
	}
}

func TestNetworkDiscoveryRequiresTrustedNamespace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted bool
	}{
		{"tenant", false}, {"administrator-approved", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := daemon("calico-node", "calico-node")
			ds.Namespace = tc.name
			ds.Spec.Template.Spec.Containers[0].Image = "busybox:1.36"
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ds.Namespace}}
			if tc.trusted {
				ns.Labels = map[string]string{"arally.io/network-discovery": "trusted"}
			}
			status := Discover(context.Background(), testClient(t, ds, ns))
			if (status.NetworkPolicy.Status == "Supported") != tc.trusted {
				t.Fatalf("unexpected provider trust: %+v", status.NetworkPolicy)
			}
		})
	}
}
