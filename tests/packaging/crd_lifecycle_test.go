//go:build integration

package packaging_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/capabilities"
	"agentrally/operator/internal/controller/agentgroup"
	"agentrally/operator/internal/controller/clustercapabilities"
	"agentrally/operator/internal/podconfig"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

// Exercise the Helm CLI against an isolated API server, never the user's cluster.
func TestHelmCRDLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("set KUBEBUILDER_ASSETS to envtest binaries")
	}
	stage, _ := packageStage(t)
	environment := &envtest.Environment{BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS")}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	kubeconfig := isolatedKubeconfig(t, cfg)
	chart := filepath.Join(stage, "chart")
	runHelm(t, kubeconfig, "install", "agentrally-operator", chart, "-n", "agentrally-system", "--create-namespace")

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1.AddToScheme, apiextensionsv1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	k8s, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("manifest-admission", func(t *testing.T) { verifyManifestAdmission(t, k8s) })
	verifyReportBootstrap(t, cfg, scheme, k8s, kubeconfig)
	t.Run("runtime-capability-admission", func(t *testing.T) { verifyRuntimeCapabilityAdmission(t, k8s) })
	t.Run("storage-capability-admission", func(t *testing.T) { verifyStorageCapabilityAdmission(t, k8s) })
	verifyPodAdmissionDefaults(t, k8s)
	t.Run("group-limit-range", func(t *testing.T) { verifyGroupLimitRangeConvergence(t, k8s, scheme) })

	group := &v1.AgentGroup{}
	group.Name = "retained"
	if err := k8s.Create(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	uid := verifySchemaUpgrade(t, k8s, kubeconfig, chart)
	crd := &apiextensionsv1.CustomResourceDefinition{}
	key := client.ObjectKey{Name: "agentinstances.arally.io"}
	runHelm(t, kubeconfig, "uninstall", "agentrally-operator", "-n", "agentrally-system")
	if err := k8s.Get(context.Background(), key, crd); err != nil || crd.UID != uid {
		t.Fatalf("uninstall removed CRD: %v", err)
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(group), &v1.AgentGroup{}); err != nil {
		t.Fatalf("uninstall removed existing AgentGroup: %v", err)
	}
	verifyLegacyAdoption(t, k8s, kubeconfig, chart)
	if err := k8s.Get(context.Background(), key, crd); err != nil || crd.UID != uid || crd.Annotations["meta.helm.sh/release-name"] != "agentrally-legacy" || crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description != "schema updated by helm upgrade" {
		t.Fatalf("retained CRD was not adopted in place: %v", err)
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(group), &v1.AgentGroup{}); err != nil {
		t.Fatalf("adoption removed existing AgentGroup: %v", err)
	}
}

func verifyManifestAdmission(t *testing.T, kube client.Client) {
	t.Helper()
	ctx := t.Context()
	namespaces := map[string]bool{}
	admitted := 0
	for _, example := range manifestResources(t) {
		object := example.object
		// The source template belongs to upstream Extensions, whose CRD is
		// not installed by this Operator-only Helm lifecycle fixture.
		if object.GroupVersionKind().Group == "extensions.agents.x-k8s.io" {
			continue
		}
		t.Run(example.path, func(t *testing.T) {
			if namespace := object.GetNamespace(); namespace != "" && !namespaces[namespace] {
				if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
					t.Fatal(err)
				}
				namespaces[namespace] = true
			}
			// Alternatives can share names. Dry-run exercises real admission
			// without storing sample declarations or requiring controllers.
			if err := kube.Create(ctx, object, client.DryRunAll); err != nil {
				t.Fatalf("example API-server admission failed: %v", err)
			}
			admitted++
		})
	}
	t.Logf("API-server dry-run admitted %d example resources", admitted)
}

func verifyGroupLimitRangeConvergence(t *testing.T, kube client.WithWatch, scheme *runtime.Scheme) {
	t.Helper()
	ctx := context.Background()
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "limit-defaults"}, Spec: v1.AgentGroupSpec{
		// envtest has no quota controller. Pending quota admission lets this test
		// exercise real Group policy without requiring allocation APIs.
		Quota: &corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("8")}},
	}}
	if err := kube.Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	updates := 0
	counted := interceptor.NewClient(kube, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
		if _, ok := object.(*corev1.LimitRange); ok {
			updates++
		}
		return c.Update(ctx, object, options...)
	}})
	r := &agentgroup.Reconciler{Client: counted, Reader: kube, Scheme: scheme}
	reconcile := func(t *testing.T) {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err != nil {
			t.Fatalf("Group reconciliation: %v; cause: %v", err, errors.Unwrap(err))
		}
	}
	for _, scenario := range []struct {
		name string
		item corev1.LimitRangeItem
	}{
		{"default-only", corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}},
		{"max-and-min", corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Max: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, Min: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}}},
		{"explicit-request", corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Default: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}},
		{"removed-defaults", corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Min: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}}},
	} {
		t.Log("LimitRange policy:", scenario.name)
		if err := kube.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil {
			t.Fatal(err)
		}
		group.Spec.LimitRange = &corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{scenario.item}}
		if err := kube.Update(ctx, group); err != nil {
			t.Fatal(err)
		}
		declared := group.Spec.LimitRange.DeepCopy()
		reconcile(t)
		before := updates
		reconcile(t)
		reconcile(t)
		if updates != before {
			t.Errorf("%s did not converge: %d unnecessary updates", scenario.name, updates-before)
		}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil || !equality.Semantic.DeepEqual(declared, group.Spec.LimitRange) {
			t.Fatal("native defaulting changed Group intent", err)
		}
	}
	limits := &corev1.LimitRange{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: group.NamespaceName(), Name: "agentrally-limits"}, limits); err != nil {
		t.Fatal(err)
	}
	if len(limits.Spec.Limits[0].Default) != 0 || limits.Spec.Limits[0].DefaultRequest.Cpu().Cmp(resource.MustParse("50m")) != 0 {
		t.Fatal("removed defaults survived the policy update")
	}
	limits.Spec.Limits[0].DefaultRequest[corev1.ResourceCPU] = resource.MustParse("200m")
	if err := kube.Update(ctx, limits); err != nil {
		t.Fatal(err)
	}
	reconcile(t)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(limits), limits); err != nil || limits.Spec.Limits[0].DefaultRequest.Cpu().Cmp(resource.MustParse("50m")) != 0 {
		t.Fatal("default normalization hid policy drift", err)
	}
}

// Real API defaults must not make a matching Pod appear to need a restart.
// Construct desired inputs independently and compare against API-server output.
func verifyPodAdmissionDefaults(t *testing.T, kube client.Client) {
	t.Helper()
	ctx := context.Background()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pod-defaults"}}
	if err := kube.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace.Name}}
	if err := kube.Create(ctx, account); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"http-probe", "downward-api", "projected-token"} {
		t.Run(scenario, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: scenario, Namespace: namespace.Name},
				Spec: corev1.PodSpec{
					ServiceAccountName:           "default",
					AutomountServiceAccountToken: ptr.To(false),
					Containers:                   []corev1.Container{{Name: "agent", Image: "example/agent:v1"}},
				},
			}
			switch scenario {
			case "http-probe":
				pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}},
				}
			case "downward-api":
				pod.Spec.Volumes = []corev1.Volume{{Name: "metadata", VolumeSource: corev1.VolumeSource{
					DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{{
						Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					}}},
				}}}
			case "projected-token":
				pod.Spec.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
					}}},
				}}}
			}
			desired := pod.Spec.DeepCopy()
			if err := kube.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if !podconfig.MatchesAdmitted(desired, &pod.Spec) {
				t.Fatal("API defaulting introduced false configuration drift")
			}
		})
	}
}

func verifySchemaUpgrade(t *testing.T, k8s client.Client, kubeconfig, chart string) types.UID {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	key := client.ObjectKey{Name: "agentinstances.arally.io"}
	if err := k8s.Get(context.Background(), key, crd); err != nil {
		t.Fatal(err)
	}
	uid := crd.UID
	const description = "schema updated by helm upgrade"
	source := filepath.Join(chart, "templates/crds/arally.io_agentinstances.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// Keep templated metadata while replacing only the schema in this chart copy.
	header, _, found := strings.Cut(string(data), "\nspec:\n")
	if !found {
		t.Fatal("generated CRD template has no spec")
	}
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = description
	data, err = yaml.Marshal(struct {
		Spec apiextensionsv1.CustomResourceDefinitionSpec `json:"spec"`
	}{Spec: crd.Spec})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, append([]byte(header+"\n"), data...), 0644); err != nil {
		t.Fatal(err)
	}
	runHelm(t, kubeconfig, "upgrade", "agentrally-operator", chart, "-n", "agentrally-system")
	if err := k8s.Get(context.Background(), key, crd); err != nil || crd.UID != uid || crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Description != description {
		t.Fatalf("CRD was not upgraded in place: %v", err)
	}
	return uid
}

func verifyLegacyAdoption(t *testing.T, k8s client.Client, kubeconfig, chart string) {
	t.Helper()
	for key := range renderChartObjects(t, chart) {
		if !strings.HasPrefix(key, "CustomResourceDefinition/") {
			continue
		}
		name := strings.TrimPrefix(key, "CustomResourceDefinition/")
		retained := &apiextensionsv1.CustomResourceDefinition{}
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, retained); err != nil {
			t.Fatal(err)
		}
		delete(retained.Annotations, "meta.helm.sh/release-name")
		delete(retained.Annotations, "meta.helm.sh/release-namespace")
		delete(retained.Labels, "app.kubernetes.io/managed-by")
		if err := k8s.Update(context.Background(), retained); err != nil {
			t.Fatal(err)
		}
		// An earlier manual SSA upgrade may also own the schema. Helm must still
		// adopt and update it through its normal patch operation without conflict.
		applied := &apiextensionsv1.CustomResourceDefinition{
			TypeMeta:   metav1.TypeMeta{APIVersion: apiextensionsv1.SchemeGroupVersion.String(), Kind: "CustomResourceDefinition"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       *retained.Spec.DeepCopy(),
		}
		applied.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "schema applied before adoption"
		if err := k8s.Patch(context.Background(), applied, client.Apply, client.FieldOwner("agentrally-operator-install"), client.ForceOwnership); err != nil {
			t.Fatal(err)
		}
	}
	// Old charts installed CRDs from crds/, outside the release manifest.
	stage, _ := packageStage(t)
	legacy := filepath.Join(stage, "chart")
	if err := os.Mkdir(filepath.Join(legacy, "crds"), 0755); err != nil {
		t.Fatal(err)
	}
	objects := renderChartObjects(t, legacy)
	for key, data := range objects {
		if !strings.HasPrefix(key, "CustomResourceDefinition/") {
			continue
		}
		plural := strings.TrimSuffix(strings.TrimPrefix(key, "CustomResourceDefinition/"), "."+v1.GroupVersion.Group)
		name := v1.GroupVersion.Group + "_" + plural + ".yaml"
		if err := os.WriteFile(filepath.Join(legacy, "crds", name), data, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(legacy, "templates/crds", name)); err != nil {
			t.Fatal(err)
		}
	}
	runHelm(t, kubeconfig, "install", "agentrally-legacy", legacy, "-n", "agentrally-system")
	runHelm(t, kubeconfig, "upgrade", "agentrally-legacy", chart, "-n", "agentrally-system", "--take-ownership")
	runHelm(t, kubeconfig, "upgrade", "agentrally-legacy", chart, "-n", "agentrally-system")
}

func isolatedKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"isolated": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"isolated": {
			ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"isolated": {Cluster: "isolated", AuthInfo: "isolated"}},
		CurrentContext: "isolated",
	}
	data, err := clientcmd.Write(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runHelm(t *testing.T, kubeconfig string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "helm", args...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KUBECONFIG=") && !strings.HasPrefix(entry, "HELM_KUBE") && !strings.HasPrefix(entry, "HELM_NAMESPACE=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "KUBECONFIG="+kubeconfig)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helm %s: %v\n%s", args[0], err, out)
	}
}

func verifyReportBootstrap(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme, k8s client.Client, kubeconfig string) {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&clustercapabilities.Reconciler{}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		report := &v1.ClusterCapabilities{}
		if err := k8s.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return report.Status.ObservedAt != nil, nil
	})
	cancel()
	select {
	case managerErr := <-done:
		if managerErr != nil {
			t.Fatal(managerErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("discovery controller did not stop")
	}
	if err != nil {
		t.Fatalf("controller did not bootstrap and refresh the default report: %v", err)
	}
	// Successful discovery with no policy provider must display independently
	// from capability support, using the API server's generated table columns.
	out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "arcap", "cluster").CombinedOutput()
	if err != nil {
		t.Fatalf("read capability table: %v\n%s", err, out)
	}
	rows := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(rows) != 2 || strings.Join(strings.Fields(rows[0]), " ") != "NAME RUNTIMESCAN STORAGESCAN NETWORKSCAN NETWORKPOLICY OBSERVED" ||
		!strings.HasPrefix(strings.Join(strings.Fields(rows[1]), " "), "cluster True True True Unknown ") {
		t.Fatalf("discovery success and provider support are not readable independently:\n%s", out)
	}
	query := `{range .status.capabilities[*]}{.category}{"\t"}{.displayName}{"\t"}{.status}{"\n"}{end}`
	out, err = exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "arcap", "cluster", "-o", "jsonpath="+query).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Isolation\tNode default runtime\tUnknown") || !strings.Contains(string(out), "Networking\tPod network policies\tUnknown") {
		t.Fatalf("read capability summary: %v\n%s", err, out)
	}
}

func verifyRuntimeCapabilityAdmission(t *testing.T, kube client.Client) {
	t.Helper()
	ctx := t.Context()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "runtime-reports", Labels: map[string]string{corev1.LabelOSStable: "linux"}}}
	if err := kube.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kube.Delete(context.Background(), node) }()
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.Architecture = "linux", "arm64"
	node.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("3")}
	node.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{
		{Name: "opaque", Features: &corev1.NodeRuntimeHandlerFeatures{UserNamespaces: ptr.To(true), RecursiveReadOnlyMounts: ptr.To(false)}},
		{Name: ""},
	}
	if err := kube.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "declared-runtime", Annotations: map[string]string{
		"arally.io/runtime-classification": `{"version":"v1","family":"future-engine","category":"VirtualMachine","imageContract":"Unknown"}`,
	}}, Handler: "opaque"}
	if err := kube.Create(ctx, rc); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kube.Delete(context.Background(), rc) }()
	report := &v1.ClusterCapabilities{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
		t.Fatal(err)
	}
	defer func() {
		report.Spec.RuntimeMappings = nil
		_ = kube.Update(context.Background(), report)
	}()
	(capabilities.Observer{Reader: kube}).Refresh(ctx, report, capabilities.Runtime, true)
	if err := kube.Status().Update(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(report), report); err != nil {
		t.Fatal(err)
	}
	if len(report.Status.Runtimes) != 1 || len(report.Status.RuntimeHandlers) != 2 {
		t.Fatal("schema discarded runtime inventories", report.Status)
	}
	if len(report.Status.Platforms) != 1 || report.Status.Platforms[0].OS != "linux" || report.Status.Platforms[0].Architecture != "arm64" || report.Status.Platforms[0].Nodes != 1 ||
		len(report.Status.ExtendedResources) != 1 || report.Status.ExtendedResources[0].Name != "nvidia.com/gpu" ||
		report.Status.ExtendedResources[0].Capacity.Cmp(resource.MustParse("4")) != 0 || report.Status.ExtendedResources[0].Allocatable.Cmp(resource.MustParse("3")) != 0 {
		t.Fatal("schema discarded node platforms or declared resources", report.Status)
	}
	observed := report.Status.Runtimes[0]
	if observed.Source != "RuntimeClassDeclared" || observed.Category != "VirtualMachine" || observed.ImageContract != "Unknown" ||
		observed.HandlerEvidence == nil || observed.HandlerEvidence.ReportedNodes != 1 || observed.Features == nil ||
		observed.Features.UserNamespaces.ReportedSupportedNodes != 1 || observed.Features.RecursiveReadOnlyMounts.ReportedUnsupportedNodes != 1 {
		t.Fatal("schema discarded partial classification or native reports", observed)
	}
	// The same independent Unknown contract must be accepted in the central mapping.
	report.Spec.RuntimeMappings = []v1.RuntimeMapping{{RuntimeClassName: rc.Name, Family: "future-engine", Category: "VirtualMachine", ImageContract: "Unknown"}}
	if err := kube.Update(ctx, report); err != nil {
		t.Fatal("partial administrator classification was rejected", err)
	}
	report.Spec.RuntimeMappings[0].ImageContract = "WasmWASI"
	if err := kube.Update(ctx, report); !apierrors.IsInvalid(err) {
		t.Fatal("contradictory known contract was admitted", err)
	}
}

func verifyStorageCapabilityAdmission(t *testing.T, kube client.Client) {
	t.Helper()
	ctx := t.Context()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "future-storage"}, Provisioner: "future.csi.example"}
	node := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "storage-reports"}, Spec: storagev1.CSINodeSpec{
		Drivers: []storagev1.CSINodeDriver{{Name: sc.Provisioner, NodeID: "private-node-id"}},
	}}
	for _, object := range []client.Object{sc, node} {
		if err := kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = kube.Delete(context.Background(), object) }()
	}
	report := &v1.ClusterCapabilities{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
		t.Fatal(err)
	}
	// No vendor catalog, CSIDriver object or filesystem hint is required to
	// persist native registration independently of an unknown volume interface.
	(capabilities.Observer{Reader: kube}).Refresh(ctx, report, capabilities.Storage, true)
	if err := kube.Status().Update(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(report), report); err != nil {
		t.Fatal(err)
	}
	if len(report.Status.StorageClasses) != 1 {
		t.Fatal("schema discarded native storage inventory", report.Status.StorageClasses)
	}
	observed := report.Status.StorageClasses[0]
	if observed.Type != "Unknown" || observed.Status != "Observed" || !observed.DriverRegistered || observed.DriverDeclared || observed.Reason != "StorageTypeUnrecognized" {
		t.Fatal("schema conflated native registration with volume classification", observed)
	}
}
