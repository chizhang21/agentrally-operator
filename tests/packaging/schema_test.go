package packaging_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
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
func TestNativeSampleSurvivesStructuralPruning(t *testing.T) {
	for _, sampleType := range []struct{ kind, plural, file string }{{"agentgroup", "agentgroups", "group.yaml"}, {"agentclaim", "agentclaims", "agent.yaml"}, {"clustercapabilities", "clustercapabilities", ""}} {
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
			if sampleType.file == "" {
				sample, err = yaml.Marshal(&v1.ClusterCapabilities{TypeMeta: metav1.TypeMeta{APIVersion: v1.GroupVersion.String(), Kind: "ClusterCapabilities"}, ObjectMeta: metav1.ObjectMeta{Name: "cluster"}})
			} else {
				sample, err = os.ReadFile("../../examples/base/" + sampleType.file)
			}
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]interface{}
			if err := yaml.Unmarshal(sample, &object); err != nil {
				t.Fatal(err)
			}
			if kind == "agentgroup" {
				object["spec"].(map[string]interface{})["interactionDefaults"] = map[string]interface{}{
					"httpRoute": map[string]interface{}{
						"gatewayRef": map[string]interface{}{"name": "shared", "namespace": "gateway-system", "sectionName": "https"},
						"domain":     "agents.example.com",
					},
				}
			}
			if kind == "agentclaim" {
				workload := &v1.AgentClaim{}
				if err := yaml.Unmarshal(sample, workload); err != nil {
					t.Fatal(err)
				}
				workload.Spec.Interaction = &v1.InteractionEndpoint{Type: "HTTPRoute", Port: 8080, Hostname: "agent.example.com", Scheme: "HTTPS",
					GatewayRef: &v1.GatewayReference{Name: "shared", Namespace: "gateway-system", SectionName: "https"}}
				ttl, timeout := int32(300), int32(60)
				now := metav1.Now()
				workload.Spec.RestartToken = "request-1"
				workload.Spec.Lifecycle = &v1.AgentClaimLifecycle{ShutdownTime: &now, TTLSecondsAfterFinished: &ttl, ShutdownPolicy: "Retain"}
				workload.Spec.Provisioning = &v1.AgentClaimProvisioning{WarmPoolRef: &v1.WarmPoolReference{Name: "agents"}, ClaimTimeoutSeconds: &timeout}
				workload.Status = v1.AgentClaimStatus{DesiredRevision: "desired", AppliedRevision: "applied", PodName: "instance", PodUID: "pod-uid", LastRestartToken: "request-1",
					Lifecycle:    &v1.AgentClaimLifecycleStatus{FinishedAt: &now, FinishedReason: "PodSucceeded", ExpiresAt: &now, ExpiredAt: &now, Reason: "ShutdownTimeReached"},
					Provisioning: &v1.AgentClaimProvisioningStatus{Mode: "Pool", PoolName: "pool", TemplateName: "snapshot", ConfigurationRevision: "revision", ClaimName: "claim", StartedAt: &now, CompletedAt: &now, TimedOutAt: &now}}
				workload.Status.Interaction = &v1.InteractionEndpointStatus{ServiceName: "access", RouteName: "access", URL: "https://agent.example.com", Port: 8080, GatewayRef: workload.Spec.Interaction.GatewayRef.DeepCopy()}
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
				report.Status = v1.ClusterCapabilitiesStatus{ObservedAt: &now, ObservedGeneration: 1,
					DefaultRuntime: &v1.RuntimeObservation{Name: "default", Family: "container", Category: "Container", ImageContract: "LinuxOCI", Status: "Observed", EligibleNodes: 1},
					Runtimes:       []v1.RuntimeObservation{{Name: "kata", Handler: "kata-qemu", Family: "kata", Category: "VirtualMachine", ImageContract: "LinuxOCI", Status: "Observed", EligibleNodes: 1}},
					NodeRuntimes:   []v1.NodeRuntimeObservation{{Implementation: "containerd", Version: "2.1.0", Nodes: 1}},
					StorageClasses: []v1.StorageObservation{{Name: "nfs", Provisioner: "nfs.csi.k8s.io", Type: "NFS", Source: "Provisioner", Status: "Observed", Default: true, DriverRegistered: true}},
					CSIDrivers:     []string{"nfs.csi.k8s.io"},
					NetworkPolicy:  &v1.NetworkPolicyObservation{Status: "Supported", Reason: "ReadyProviderConfigurationObserved", Providers: []v1.NetworkPolicyProviderObservation{{Name: "calico-node", Namespace: "kube-system", Provider: "Calico", Status: "Supported"}}}}
				object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(report)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := runtime.DeepCopyJSON(object)
			pruning.Prune(object, structural, true)
			if !reflect.DeepEqual(before["status"], object["status"]) {
				t.Fatal("CRD pruned observation fields")
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
func TestInteractionAdmission(t *testing.T) {
	crd := renderedCRD(t, "agentclaims")
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
	crd := renderedCRD(t, "agentclaims")
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
	base := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "team"}, "templateRef": map[string]interface{}{"name": "agent-template"}}
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
	return func(value map[string]interface{}) bool {
		errs := apiservervalidation.ValidateCustomResource(field.NewPath("spec"), value, validator)
		if cel != nil {
			celErrs, _ := cel.Validate(context.Background(), field.NewPath("spec"), structural, value, nil, 1_000_000)
			errs = append(errs, celErrs...)
		}
		return len(errs) == 0
	}
}

func TestTemplateReferenceAndSecurityAdmission(t *testing.T) {
	accepts := admissionChecker(t, "agentclaims")
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
	accepts := admissionChecker(t, "agentclaims")
	base := map[string]interface{}{"agentGroupRef": map[string]interface{}{"name": "demo"}, "template": "generic", "runtime": map[string]interface{}{"image": "example.com/agent:v1"}}
	for _, tc := range []struct {
		name  string
		edit  func(map[string]interface{})
		valid bool
	}{
		{"generic", func(v map[string]interface{}) {}, true},
		{"assistant", func(v map[string]interface{}) { v["template"] = "assistant" }, true},
		{"coding", func(v map[string]interface{}) { v["template"] = "coding" }, true},
		{"unknown", func(v map[string]interface{}) { v["template"] = "unknown" }, false},
		{"both-selectors", func(v map[string]interface{}) { v["templateRef"] = map[string]interface{}{"name": "generic"} }, false},
		{"missing-runtime", func(v map[string]interface{}) { delete(v, "runtime") }, false},
		{"missing-image", func(v map[string]interface{}) { v["runtime"] = map[string]interface{}{} }, false},
		{"empty-image", func(v map[string]interface{}) { v["runtime"] = map[string]interface{}{"image": ""} }, false},
		{"whitespace-image", func(v map[string]interface{}) { v["runtime"] = map[string]interface{}{"image": "agent v1"} }, false},
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
	if !accepts(map[string]interface{}{}) || !accepts(map[string]interface{}{"namespace": "arally-demo"}) || accepts(map[string]interface{}{"namespace": "default"}) || accepts(map[string]interface{}{"podSecurity": "Privileged"}) {
		t.Fatal("namespace/security admission does not enforce the group contract")
	}
}

func TestAllAgentExamplesSurviveAdmissionAndPruning(t *testing.T) {
	accepts := admissionChecker(t, "agentclaims")
	paths, err := filepath.Glob("../../examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	nestedPaths, err := filepath.Glob("../../examples/*/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, nestedPaths...)
	if len(paths) == 0 {
		t.Fatal("AgentClaim sample resources are missing")
	}
	crd := renderedCRD(t, "agentclaims")
	schema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(schema)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]interface{}
		if err := yaml.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		if object["kind"] != "AgentClaim" {
			continue
		}
		checked++
		t.Run(filepath.Base(path), func(t *testing.T) {
			if !accepts(object["spec"].(map[string]interface{})) {
				t.Fatal("example fails CRD admission")
			}
			before := runtime.DeepCopyJSON(object)
			pruning.Prune(object, structural, true)
			if !reflect.DeepEqual(before, object) {
				t.Fatal("example contains unsupported fields lost at admission")
			}
		})
	}
	if checked == 0 {
		t.Fatal("AgentClaim sample resources are missing")
	}
}

func TestClaimSchemaDoesNotDuplicateNativePodConfiguration(t *testing.T) {
	crd := renderedCRD(t, "agentclaims")
	props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	for _, field := range []string{"parameters", "podTemplate", "service"} {
		if _, exists := props[field]; exists {
			t.Fatalf("native configuration duplicated in Claim field %s", field)
		}
	}
}
