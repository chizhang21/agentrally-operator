package packaging_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func renderedCRD(t *testing.T, plural string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	out, exists := renderObjects(t)["CustomResourceDefinition/"+plural+".arally.io"]
	if !exists {
		t.Fatalf("chart omitted %s CRD", plural)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(out, crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

// Fake clients do not apply CRD pruning. Exercise the API-server's pruning code
// so labels, annotations and native scheduling/container fields survive admission.
func TestNativeObjectsSurviveStructuralPruning(t *testing.T) {
	for _, sampleType := range []struct{ kind, plural, file string }{{"agentgroup", "agentgroups", "group.yaml"}, {"agentdefinition", "agentdefinitions", "definition.yaml"}, {"agentinstance", "agentinstances", "instance.yaml"}, {"clustercapabilities", "clustercapabilities", ""}, {"agentworkspace", "agentworkspaces", ""}} {
		kind := sampleType.kind
		t.Run(kind, func(t *testing.T) {
			crd := renderedCRD(t, sampleType.plural)
			internal := &apiextensions.CustomResourceDefinition{}
			if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(crd, internal, nil); err != nil {
				t.Fatal(err)
			}
			// The API server populates this status during CRD creation.
			internal.Status.StoredVersions = []string{"v1alpha1"}
			if errors := validation.ValidateCustomResourceDefinition(context.Background(), internal); len(errors) != 0 {
				t.Fatal(errors.ToAggregate())
			}
			schema := &apiextensions.JSONSchemaProps{}
			if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil); err != nil {
				t.Fatal(err)
			}
			structural, err := structuralschema.NewStructural(schema)
			if err != nil {
				t.Fatal(err)
			}
			if errors := structuralschema.ValidateStructural(field.NewPath("schema"), structural); len(errors) != 0 {
				t.Fatal(errors)
			}
			var sample []byte
			if kind == "agentworkspace" {
				sample, err = yaml.Marshal(&v1.AgentWorkspace{TypeMeta: metav1.TypeMeta{APIVersion: v1.GroupVersion.String(), Kind: "AgentWorkspace"}, ObjectMeta: metav1.ObjectMeta{Name: "workspace"}, Spec: v1.AgentWorkspaceSpec{GroupName: "demo", GroupUID: "group", Namespace: "agentrally-demo", NamespaceUID: "namespace", InstanceName: "instance", InstanceUID: "instance-uid", Definition: v1.DefinitionBinding{Name: "agent", UID: "definition", Revision: "revision", TemplateName: "template"}, RevisionUID: "template-uid", RetentionSeconds: 3600, VolumeClaimTemplate: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}})
			} else if sampleType.file == "" {
				sample, err = yaml.Marshal(&v1.ClusterCapabilities{TypeMeta: metav1.TypeMeta{APIVersion: v1.GroupVersion.String(), Kind: "ClusterCapabilities"}, ObjectMeta: metav1.ObjectMeta{Name: "cluster"}})
			} else {
				sample, err = os.ReadFile("../../examples/basic/" + sampleType.file)
			}
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]interface{}
			if err := yaml.Unmarshal(sample, &object); err != nil {
				t.Fatal(err)
			}
			if kind == "agentgroup" {
				object["spec"].(map[string]interface{})["allocationPaused"] = true
				object["spec"].(map[string]interface{})["prewarming"] = map[string]interface{}{"replicas": int64(2), "idleTTLSeconds": int64(300)}
				object["spec"].(map[string]interface{})["limitRange"] = map[string]interface{}{"limits": []interface{}{map[string]interface{}{
					"type": "Container", "default": map[string]interface{}{"cpu": "1", "memory": "128Mi"},
				}}}
				object["status"] = map[string]interface{}{"summary": map[string]interface{}{
					"instances": map[string]interface{}{"total": int64(2), "ready": int64(1), "available": int64(1), "pending": int64(1), "suspended": int64(0), "expired": int64(0)},
					"pools":     map[string]interface{}{"total": int64(1), "desiredSpareReplicas": int64(2), "readySpareReplicas": int64(1)},
					"storage":   map[string]interface{}{"pvcs": int64(2), "requested": "2Gi", "capacity": "1Gi"},
				}}
				object["spec"].(map[string]interface{})["interactionDefaults"] = map[string]interface{}{
					"httpRoute": map[string]interface{}{
						"gatewayRef": map[string]interface{}{"name": "shared", "namespace": "gateway-system", "sectionName": "https"},
						"domain":     "agents.example.com",
					},
				}
			}
			if kind == "agentinstance" {
				workload := &v1.AgentInstance{}
				if err := yaml.Unmarshal(sample, workload); err != nil {
					t.Fatal(err)
				}
				workload.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute", Port: 8080, Hostname: "agent.example.com", Scheme: "HTTPS",
					GatewayRef: &v1.GatewayReference{Name: "shared", Namespace: "gateway-system", SectionName: "https"}}
				ttl, timeout := int32(300), int32(60)
				now := metav1.Now()
				workload.Spec.RestartToken = "request-1"
				workload.Spec.Workspace = &v1.WorkspaceMount{MountPath: "/workspace", VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				}}
				workload.Spec.Lifecycle = &v1.AgentInstanceLifecycle{ShutdownTime: &now, TTLSecondsAfterFinished: &ttl, ShutdownPolicy: "Retain"}
				workload.Spec.Provisioning = &v1.AgentInstanceProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "agents"}, ClaimTimeoutSeconds: &timeout}
				workload.Status = v1.AgentInstanceStatus{DesiredRevision: "desired", AppliedRevision: "applied", PodName: "instance", PodUID: "pod-uid", LastRestartToken: "request-1",
					Lifecycle:    &v1.AgentInstanceLifecycleStatus{FinishedAt: &now, FinishedReason: "PodSucceeded", ExpiresAt: &now, ExpiredAt: &now, Reason: "ShutdownTimeReached"},
					Provisioning: &v1.AgentInstanceProvisioningStatus{Mode: "Pool", PoolName: "pool", TemplateName: "snapshot", ConfigurationRevision: "revision", ClaimName: "claim", StartedAt: &now, CompletedAt: &now, TimedOutAt: &now, LaunchType: "warm"},
					Timings:      &v1.AgentInstanceTimings{PodCreatedAt: &now, PodReadyAt: &now, SuspensionStartedAt: &now, SuspendedAt: &now, CleanupStartedAt: &now, CleanupCompletedAt: &now}}
				workload.Status.Interaction = &v1.InteractionEndpointStatus{ServiceName: "access", RouteName: "access", URL: "https://agent.example.com", Port: 8080, GatewayRef: workload.Spec.Interaction.GatewayRef.DeepCopy()}
				workload.Status.VolumeClaims = map[string]string{"agentrally-workspace": "agentrally-workspace-instance"}
				object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(workload)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "clustercapabilities" {
				report := &v1.ClusterCapabilities{}
				if err := yaml.Unmarshal(sample, report); err != nil {
					t.Fatal(err)
				}
				now := metav1.Now()
				report.Spec = v1.ClusterCapabilitiesSpec{RuntimeMappings: []v1.RuntimeMapping{{RuntimeClassName: "custom", Family: "kata", Category: "VirtualMachine", ImageContract: "LinuxOCI"}}, StorageMappings: []v1.StorageMapping{{StorageClassName: "opaque", Type: "Filesystem"}, {StorageClassName: "devices", Type: "Block"}}}
				report.Status = v1.ClusterCapabilitiesStatus{ObservedAt: &now, ObservedGeneration: 1,
					CatalogRevision: "catalog-revision", Sections: []v1.DiscoverySectionObservation{{Name: "RuntimeDiscoveryReady", LastAttemptAt: &now, LastSuccessAt: &now}},
					DefaultRuntime:   &v1.RuntimeObservation{Name: "default", Family: "unknown", Category: "Unknown", ImageContract: "Unknown", Status: "Unknown", EligibleNodes: 1, Source: "NativeAPI"},
					Runtimes:         []v1.RuntimeObservation{{Name: "kata", Handler: "kata-qemu", Family: "kata", Category: "VirtualMachine", ImageContract: "LinuxOCI", Status: "Observed", EligibleNodes: 1}},
					NodeRuntimes:     []v1.NodeRuntimeObservation{{Implementation: "containerd", Version: "2.1.0", Nodes: 1}},
					StorageClasses:   []v1.StorageObservation{{Name: "files", Provisioner: "smb.csi.k8s.io", CSIDriver: "smb.csi.k8s.io", Type: "Filesystem", Source: "BuiltInCatalog", Status: "Observed", Default: true, DriverDeclared: true, DriverRegistered: true}},
					CSIDrivers:       []string{"nfs.csi.k8s.io"},
					CSIDriverDetails: []v1.CSIDriverObservation{{Name: "nfs.csi.k8s.io", Declared: true, RegisteredNodes: 1, TopologyKeys: []string{"zone"}}},
					NetworkPolicy:    &v1.NetworkPolicyObservation{Status: "Supported", Reason: "ReadyProviderConfigurationObserved", Providers: []v1.NetworkPolicyProviderObservation{{Name: "calico-node", Namespace: "kube-system", Provider: "Calico", Status: "Supported"}}}}
				report.Status.Capabilities = []v1.CapabilityObservation{{Name: "Filesystem", DisplayName: "Filesystem volumes", Category: "Storage", Description: "File service configuration", Status: "Observed",
					Resources: []v1.CapabilityReference{{Kind: "StorageClass", Name: "files", Status: "Observed", Source: "BuiltInCatalog"}},
				}}
				report.Status.Runtimes[0].Source = "InstallerMetadata"
				report.Status.Runtimes[0].HandlerEvidence = &v1.RuntimeHandlerEvidence{ReportedNodes: 1}
				report.Status.Runtimes[0].Features = &v1.RuntimeFeaturesObservation{UserNamespaces: v1.RuntimeFeatureObservation{ReportedSupportedNodes: 1}, RecursiveReadOnlyMounts: v1.RuntimeFeatureObservation{UnknownNodes: 1}}
				report.Status.RuntimeHandlers = []v1.NodeRuntimeHandlerObservation{{Name: "", ReportedNodes: 1, Features: *report.Status.Runtimes[0].Features}}
				report.Status.Capabilities = append(report.Status.Capabilities, v1.CapabilityObservation{Name: "VirtualMachineIsolation", DisplayName: "VM isolation", Category: "Isolation", Description: "Guest kernel isolation", Status: "Observed",
					Resources: []v1.CapabilityReference{{Kind: "RuntimeClass", Name: "custom", Status: "Observed", Source: "RuntimeClassDeclared"}},
				})
				object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(report)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := runtime.DeepCopyJSON(object)
			if kind == "clustercapabilities" {
				storage := object["status"].(map[string]interface{})["storageClasses"].([]interface{})[0].(map[string]interface{})
				storage["reclaimPolicy"] = "Delete"
				storage["volumeBindingMode"] = "WaitForFirstConsumer"
			}
			pruning.Prune(object, structural, true)
			if !reflect.DeepEqual(before["status"], object["status"]) {
				t.Fatal("CRD discarded current observation fields or retained retired fields")
			}
			if !reflect.DeepEqual(before["spec"], object["spec"]) {
				t.Fatal("CRD pruned native desired-state fields")
			}
			validator, _, err := apiservervalidation.NewSchemaValidator(schema)
			if err != nil {
				t.Fatal(err)
			}
			if errs := apiservervalidation.ValidateCustomResource(field.NewPath("resource"), object, validator); len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if !crd.Spec.Versions[0].Served || !crd.Spec.Versions[0].Storage || crd.Spec.Versions[0].Subresources.Status == nil {
				t.Fatal("native status/API contract missing")
			}
		})
	}
}
func TestCapabilityAdmission(t *testing.T) {
	crd := renderedCRD(t, "clustercapabilities")
	schema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := structuralcel.NewValidator(structural, true, 1_000_000)
	if validator == nil {
		t.Fatal("capability CEL rules missing")
	}
	for _, tc := range []struct {
		name, resourceName, category, contract string
		valid                                  bool
	}{
		{"canonical", "cluster", "VirtualMachine", "LinuxOCI", true},
		{"other-name", "duplicate", "VirtualMachine", "LinuxOCI", false},
		{"wasm", "cluster", "WebAssembly", "WasmWASI", true},
		{"vm-unknown-image", "cluster", "VirtualMachine", "Unknown", true},
		{"wasm-unknown-image", "cluster", "WebAssembly", "Unknown", true},
		{"unknown-isolation", "cluster", "Unknown", "LinuxOCI", true},
		{"wasm-oci", "cluster", "WebAssembly", "LinuxOCI", false},
		{"vm-wasm", "cluster", "VirtualMachine", "WasmWASI", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := map[string]interface{}{
				"metadata": map[string]interface{}{"name": tc.resourceName},
				"spec": map[string]interface{}{"runtimeMappings": []interface{}{map[string]interface{}{
					"runtimeClassName": "custom", "family": "custom", "category": tc.category, "imageContract": tc.contract,
				}}},
			}
			errs, _ := validator.Validate(context.Background(), field.NewPath("resource"), structural, object, nil, 1_000_000)
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("valid=%v, errors=%v", tc.valid, errs)
			}
		})
	}
}

func TestInteractionAdmission(t *testing.T) {
	crd := renderedCRD(t, "agentinstances")
	public := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["interaction"]
	schema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&public, schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(schema)
	if err != nil {
		t.Fatal(err)
	}
	cel := structuralcel.NewValidator(structural, false, 1_000_000)
	if cel == nil {
		t.Fatal("interaction CEL rules missing")
	}
	for _, tc := range []struct {
		name  string
		value map[string]interface{}
		valid bool
	}{
		{"http-type-only", map[string]interface{}{"type": "HTTPRoute"}, true},
		{"openshift-type-only", map[string]interface{}{"type": "OpenShiftRoute"}, true},
		{"zero-port", map[string]interface{}{"type": "HTTPRoute", "port": int64(0)}, false},
		{"http", map[string]interface{}{"type": "HTTPRoute", "port": int64(8080), "hostname": "agent.example.com", "gatewayRef": map[string]interface{}{"name": "shared"}}, true},
		{"openshift-assigned-host", map[string]interface{}{"type": "OpenShiftRoute", "port": int64(8080)}, true},
		{"missing-gateway", map[string]interface{}{"type": "HTTPRoute", "port": int64(8080), "hostname": "agent.example.com"}, true},
		{"missing-host", map[string]interface{}{"type": "HTTPRoute", "port": int64(8080), "gatewayRef": map[string]interface{}{"name": "shared"}}, true},
		{"openshift-gateway", map[string]interface{}{"type": "OpenShiftRoute", "port": int64(8080), "gatewayRef": map[string]interface{}{"name": "shared"}}, false},
		{"invalid-port", map[string]interface{}{"type": "OpenShiftRoute", "port": int64(65536)}, false},
		{"invalid-host", map[string]interface{}{"type": "OpenShiftRoute", "port": int64(8080), "hostname": "https://agent.example.com"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := apiservervalidation.ValidateCustomResource(field.NewPath("interaction"), tc.value, validator)
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("interaction"), structural, tc.value, nil, 1_000_000)
			errs = append(errs, celErrs...)
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("valid=%v, errors=%v", tc.valid, errs)
			}
		})
	}
}

func TestExecutionPolicyAdmission(t *testing.T) {
	crd := renderedCRD(t, "agentinstances")
	public := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	schema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&public, schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(schema)
	if err != nil {
		t.Fatal(err)
	}
	cel := structuralcel.NewValidator(structural, false, 1_000_000)
	base := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "team"}, "definitionRef": map[string]interface{}{"name": "agent"}}
	pool := func(name string, timeout int64) map[string]interface{} {
		return map[string]interface{}{"warmPoolRef": map[string]interface{}{"name": name}, "claimTimeoutSeconds": timeout}
	}
	for _, tc := range []struct {
		name                    string
		lifecycle, provisioning interface{}
		oldPool                 interface{}
		valid                   bool
	}{
		{"zero-ttl", map[string]interface{}{"ttlSecondsAfterFinished": int64(0), "shutdownPolicy": "Retain"}, nil, nil, true},
		{"maximum-lifetime", map[string]interface{}{"maxLifetimeSeconds": int64(60)}, nil, nil, true},
		{"zero-maximum-lifetime", map[string]interface{}{"maxLifetimeSeconds": int64(0)}, nil, nil, false},
		{"negative-ttl", map[string]interface{}{"ttlSecondsAfterFinished": int64(-1)}, nil, nil, false},
		{"unknown-shutdown-policy", map[string]interface{}{"shutdownPolicy": "Pause"}, nil, nil, false},
		{"pool", nil, pool("agents", 60), nil, true},
		{"auto-pool", nil, map[string]interface{}{}, nil, true},
		{"auto-pool-timeout", nil, map[string]interface{}{"claimTimeoutSeconds": int64(120)}, nil, true},
		{"empty-pool", nil, pool("", 60), nil, false},
		{"zero-timeout", nil, pool("agents", 0), nil, false},
		{"oversized-timeout", nil, pool("agents", 3601), nil, false},
		{"same-pool", nil, pool("agents", 90), pool("agents", 60), true},
		{"changed-pool", nil, pool("other", 60), pool("agents", 60), false},
		{"removed-pool", nil, nil, pool("agents", 60), false},
		{"removed-pool-ref", nil, map[string]interface{}{"claimTimeoutSeconds": int64(60)}, pool("agents", 60), false},
		{"added-pool-ref", nil, pool("agents", 60), map[string]interface{}{"claimTimeoutSeconds": int64(60)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := runtime.DeepCopyJSON(base)
			if tc.lifecycle != nil {
				value["lifecycle"] = tc.lifecycle
			}
			if tc.provisioning != nil {
				value["provisioning"] = tc.provisioning
			}
			var old map[string]interface{}
			if tc.oldPool != nil {
				old = runtime.DeepCopyJSON(base)
				old["provisioning"] = tc.oldPool
			}
			errs := apiservervalidation.ValidateCustomResource(field.NewPath("spec"), value, validator)
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("spec"), structural, value, old, 1_000_000)
			errs = append(errs, celErrs...)
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("valid=%v errors=%v", tc.valid, errs)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		next, old map[string]interface{}
		valid     bool
	}{
		{"default timeout added", map[string]interface{}{"claimTimeoutSeconds": int64(300)}, nil, true},
		{"timeout override removed", nil, map[string]interface{}{"claimTimeoutSeconds": int64(300)}, true},
		{"pool added to default Claim", pool("agents", 60), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, old := runtime.DeepCopyJSON(base), runtime.DeepCopyJSON(base)
			if tc.next != nil {
				next["provisioning"] = tc.next
			}
			if tc.old != nil {
				old["provisioning"] = tc.old
			}
			errs, _ := cel.Validate(context.Background(), field.NewPath("spec"), structural, next, old, 1_000_000)
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("valid=%v errors=%v", tc.valid, errs)
			}
		})
	}

}

// Exercise API-server admission, including CEL, so a YAML-only caller receives
// the same contract as a typed Go caller before any controller creates resources.
func admissionChecker(t *testing.T, plural string) func(map[string]interface{}) bool {
	checker := admissionTransitionChecker(t, plural)
	return func(value map[string]interface{}) bool { return checker(value, nil) }
}

func admissionTransitionChecker(t *testing.T, plural string) func(map[string]interface{}, map[string]interface{}) bool {
	t.Helper()
	crd := renderedCRD(t, plural)
	public := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	schema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&public, schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(schema)
	if err != nil {
		t.Fatal(err)
	}
	cel := structuralcel.NewValidator(structural, false, 1_000_000)
	return func(value, old map[string]interface{}) bool {
		errs := apiservervalidation.ValidateCustomResource(field.NewPath("spec"), value, validator)
		if cel != nil {
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("spec"), structural, value, old, 1_000_000)
			errs = append(errs, celErrs...)
		}
		return len(errs) == 0
	}
}

func TestGroupRuntimePolicyAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentgroups")
	for _, tc := range []struct {
		name  string
		value map[string]interface{}
		valid bool
	}{
		{"allowed-runtime", map[string]interface{}{"executionPolicy": map[string]interface{}{"allowedRuntimeClasses": []interface{}{"kata-qemu"}, "requireImageDigest": true}}, true},
		{"invalid-runtime", map[string]interface{}{"executionPolicy": map[string]interface{}{"allowedRuntimeClasses": []interface{}{"other/kata"}}}, false},
		{"empty-runtime-list", map[string]interface{}{"executionPolicy": map[string]interface{}{"allowedRuntimeClasses": []interface{}{}}}, false},
		{"zero-spare-budget", map[string]interface{}{"prewarming": map[string]interface{}{"replicas": int64(2), "maxTotalReplicas": int64(0)}}, true},
		{"negative-spare-budget", map[string]interface{}{"prewarming": map[string]interface{}{"replicas": int64(2), "maxTotalReplicas": int64(-1)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if accepts(tc.value) != tc.valid {
				t.Fatalf("unexpected admission for %#v", tc.value)
			}
		})
	}
}

func TestTemplateReferenceAndSecurityAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentdefinitions")
	base := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "demo"}, "templateRef": map[string]interface{}{"name": "agent-template"}}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]interface{})
		valid bool
	}{
		{"native-reference", func(v map[string]interface{}) {}, true},
		{"missing-template", func(v map[string]interface{}) { delete(v, "templateRef") }, false},
		{"empty-template", func(v map[string]interface{}) { v["templateRef"] = map[string]interface{}{"name": ""} }, false},
		{"invalid-template", func(v map[string]interface{}) { v["templateRef"] = map[string]interface{}{"name": "other/template"} }, false},
		{"missing-name", func(v map[string]interface{}) { v["templateRef"] = map[string]interface{}{} }, false},
		{"sidecar-container", func(v map[string]interface{}) { v["agentContainer"] = "sidecar" }, true},
		{"declared-invocation", func(v map[string]interface{}) { v["invocationProtocol"] = "HTTPJSONSSEV1" }, true},
		{"unknown-invocation", func(v map[string]interface{}) { v["invocationProtocol"] = "invented" }, false},
		{"root-restricted", func(v map[string]interface{}) {
			v["security"] = map[string]interface{}{"profile": "Restricted", "runAsUser": int64(0)}
		}, false},
		{"anyuid-dedicated-account", func(v map[string]interface{}) {
			v["security"] = map[string]interface{}{"profile": "AnyUID", "serviceAccountName": "legacy"}
		}, true},
		{"anyuid-default-account", func(v map[string]interface{}) {
			v["security"] = map[string]interface{}{"profile": "AnyUID", "serviceAccountName": "default"}
		}, false},
		{"anyuid-explicit", func(v map[string]interface{}) {
			v["security"] = map[string]interface{}{"profile": "AnyUID", "serviceAccountName": "legacy", "runAsUser": int64(0)}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := runtime.DeepCopyJSON(base)
			tc.edit(value)
			if got := accepts(value); got != tc.valid {
				t.Fatalf("admission=%v want %v", got, tc.valid)
			}
		})
	}
}

func TestBuiltinTemplateAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentdefinitions")
	base := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "demo"}, "template": "generic", "container": map[string]interface{}{"image": "example.com/agent:v1"}}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]interface{})
		valid bool
	}{
		{"generic", func(v map[string]interface{}) {}, true},
		{"assistant", func(v map[string]interface{}) { v["template"] = "assistant" }, true},
		{"coding", func(v map[string]interface{}) { v["template"] = "coding" }, true},
		{"proactive-capacity", func(v map[string]interface{}) { v["prewarming"] = map[string]interface{}{"replicas": int64(1)} }, true},
		{"disable-proactive-capacity", func(v map[string]interface{}) { v["prewarming"] = map[string]interface{}{"replicas": int64(0)} }, true},
		{"negative-capacity", func(v map[string]interface{}) { v["prewarming"] = map[string]interface{}{"replicas": int64(-1)} }, false},
		{"missing-capacity", func(v map[string]interface{}) { v["prewarming"] = map[string]interface{}{} }, false},
		{"unknown", func(v map[string]interface{}) { v["template"] = "unknown" }, false},
		{"both-selectors", func(v map[string]interface{}) { v["templateRef"] = map[string]interface{}{"name": "generic"} }, false},
		{"missing-runtime", func(v map[string]interface{}) { delete(v, "container") }, false},
		{"missing-image", func(v map[string]interface{}) { v["container"] = map[string]interface{}{} }, false},
		{"empty-image", func(v map[string]interface{}) { v["container"] = map[string]interface{}{"image": ""} }, false},
		{"whitespace-image", func(v map[string]interface{}) { v["container"] = map[string]interface{}{"image": "agent v1"} }, false},
		{"other-container", func(v map[string]interface{}) { v["agentContainer"] = "sidecar" }, false},
		{"native-with-runtime", func(v map[string]interface{}) {
			delete(v, "template")
			v["templateRef"] = map[string]interface{}{"name": "custom"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := runtime.DeepCopyJSON(base)
			tc.edit(value)
			if accepts(value) != tc.valid {
				t.Fatalf("unexpected admission result for %+v", value)
			}
		})
	}
}

func TestGroupNamespaceAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentgroups")
	if !accepts(map[string]interface{}{}) || !accepts(map[string]interface{}{"namespace": "agentrally-demo"}) || accepts(map[string]interface{}{"namespace": "default"}) || accepts(map[string]interface{}{"podSecurity": "Privileged"}) {
		t.Fatal("namespace/security admission does not enforce the group contract")
	}
}

func TestConnectorOnlyPolicyAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentgroups")
	base := map[string]interface{}{"invocationAccess": map[string]interface{}{"mode": "ConnectorOnly"}}
	if !accepts(base) {
		t.Fatal("managed ingress without explicit policy was rejected")
	}
	base["networkPolicy"] = map[string]interface{}{"podSelector": map[string]interface{}{}, "egress": []interface{}{map[string]interface{}{}}}
	if !accepts(base) {
		t.Fatal("managed ingress with egress was rejected")
	}
	policy := base["networkPolicy"].(map[string]interface{})
	policy["ingress"] = []interface{}{map[string]interface{}{}}
	if accepts(base) {
		t.Fatal("explicit ingress bypass was accepted")
	}
	delete(policy, "ingress")
	policy["podSelector"] = map[string]interface{}{"matchLabels": map[string]interface{}{"app": "partial"}}
	if accepts(base) {
		t.Fatal("partial selector would change egress scope")
	}
}

func TestInstanceDefinitionAndWorkspaceAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentinstances")
	base := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "demo"}, "definitionRef": map[string]interface{}{"name": "assistant"}}
	if !accepts(base) {
		t.Fatal("definition reference rejected")
	}
	for _, edit := range []func(map[string]interface{}){
		func(value map[string]interface{}) { delete(value, "definitionRef") },
		func(value map[string]interface{}) {
			value["definitionRef"] = map[string]interface{}{"name": "other/assistant"}
		},
		func(value map[string]interface{}) { value["revision"] = "latest" },
		func(value map[string]interface{}) {
			value["workspace"] = map[string]interface{}{"mountPath": "/workspace"}
		},
		func(value map[string]interface{}) {
			value["workspace"] = map[string]interface{}{"mountPath": "/workspace", "existingClaimRef": map[string]interface{}{"name": "state"}, "volumeClaimTemplate": map[string]interface{}{}}
		},
	} {
		value := runtime.DeepCopyJSON(base)
		edit(value)
		if accepts(value) {
			t.Fatalf("invalid instance accepted: %+v", value)
		}
	}
	base["workspace"] = map[string]interface{}{"mountPath": "/workspace", "existingClaimRef": map[string]interface{}{"name": "state"}}
	if !accepts(base) {
		t.Fatal("retained workspace rejected")
	}
	definitionAccepts := admissionChecker(t, "agentdefinitions")
	if definitionAccepts(map[string]interface{}{"agentGroupRef": base["agentGroupRef"], "template": "assistant", "container": map[string]interface{}{"image": "example/assistant:v1"}, "workspace": base["workspace"]}) {
		t.Fatal("reusable definition accepted a user-specific workspace")
	}
}

func TestPersistentWorkspaceModeCannotChange(t *testing.T) {
	group := admissionTransitionChecker(t, "agentgroups")
	if group(map[string]interface{}{"persistentWorkspaces": true}, map[string]interface{}{"persistentWorkspaces": false}) {
		t.Fatal("existing Group can opt into different ownership")
	}
	instance := admissionTransitionChecker(t, "agentinstances")
	legacy := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "team"}, "definitionRef": map[string]interface{}{"name": "agent"}}
	persistent := runtime.DeepCopyJSON(legacy)
	persistent["workspace"] = map[string]interface{}{"mountPath": "/workspace", "persistence": map[string]interface{}{"retentionSeconds": int64(3600)}, "volumeClaimTemplate": map[string]interface{}{"accessModes": []interface{}{"ReadWriteOnce"}, "resources": map[string]interface{}{"requests": map[string]interface{}{"storage": "1Gi"}}}}
	if !instance(persistent, nil) || !instance(persistent, persistent) {
		t.Fatal("valid persistent configuration rejected")
	}
	if instance(persistent, legacy) || instance(legacy, persistent) {
		t.Fatal("existing Instance can change storage mode")
	}
	changed := runtime.DeepCopyJSON(persistent)
	changed["workspace"].(map[string]interface{})["persistence"].(map[string]interface{})["retentionSeconds"] = int64(0)
	if instance(changed, persistent) {
		t.Fatal("persistent retention configuration is mutable")
	}
}
