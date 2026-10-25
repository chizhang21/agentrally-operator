package capabilities

import (
	"reflect"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestRuntimeClassificationDeclarationsAndInstallerEvidence(t *testing.T) {
	declaration := `{"version":"v1","family":"new-engine","category":"VirtualMachine","imageContract":"Unknown"}`
	for _, test := range []struct {
		name, handler, declaration, family, category, contract, source, reason string
		installer                                                              bool
	}{
		{name: "new-family", handler: "opaque", declaration: declaration, family: "new-engine", category: "VirtualMachine", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeImageContractUnknown"},
		{name: "explicit-override", handler: "runc", declaration: declaration, family: "new-engine", category: "VirtualMachine", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeImageContractUnknown"},
		{name: "installer-variant", handler: "kata-future-custom-debug", installer: true, family: "kata", category: "VirtualMachine", contract: "Unknown", source: "InstallerMetadata", reason: "RuntimeImageContractUnknown"},
		{name: "known-installer-handler", handler: "kata-qemu", installer: true, family: "kata", category: "VirtualMachine", contract: "LinuxOCI", source: "BuiltInCatalog"},
		{name: "unlabelled-prefix", handler: "kata-future-custom-debug", family: "unknown", category: "Unknown", contract: "Unknown", source: "NativeAPI", reason: "RuntimeTypeUnrecognized"},
		{name: "conflicting-automatic-signals", handler: "runsc", installer: true, family: "unknown", category: "Unknown", contract: "Unknown", source: "InstallerMetadata", reason: "RuntimeClassificationConflict"},
		{name: "known-vm-image-runtime", handler: "crun-vm", family: "crun-vm", category: "VirtualMachine", contract: "Unknown", source: "BuiltInCatalog", reason: "RuntimeImageContractUnknown"},
		{name: "invalid-declaration-no-fallback", handler: "runc", declaration: `{"version":"v1","family":"x","category":"VirtualMachine"}`, family: "unknown", category: "Unknown", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeDeclarationInvalid"},
		{name: "unknown-version", handler: "runc", declaration: strings.Replace(declaration, `"v1"`, `"v2"`, 1), family: "unknown", category: "Unknown", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeDeclarationVersionUnsupported"},
		{name: "duplicate-field", handler: "runc", declaration: strings.Replace(declaration, `"category":"VirtualMachine"`, `"category":"Container","category":"VirtualMachine"`, 1), family: "unknown", category: "Unknown", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeDeclarationInvalid"},
		{name: "unknown-field", handler: "runc", declaration: strings.Replace(declaration, `"version":"v1"`, `"version":"v1","verified":true`, 1), family: "unknown", category: "Unknown", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeDeclarationInvalid"},
		{name: "bounded-declaration", handler: "runc", declaration: declaration + strings.Repeat(" ", 4096), family: "unknown", category: "Unknown", contract: "Unknown", source: "RuntimeClassDeclared", reason: "RuntimeDeclarationInvalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rc := nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "arbitrary-name"}, Handler: test.handler}
			if test.declaration != "" {
				rc.Annotations = map[string]string{"arally.io/runtime-classification": test.declaration}
			}
			if test.installer {
				rc.Labels = map[string]string{"app.kubernetes.io/managed-by": "kata-deploy"}
				rc.Scheduling = &nodev1.Scheduling{NodeSelector: map[string]string{"katacontainers.io/kata-runtime": "true"}}
			}
			got := describeRuntime(rc)
			if got.Family != test.family || got.Category != test.category || got.ImageContract != test.contract || got.Source != test.source || got.Reason != test.reason {
				t.Fatalf("classification evidence: %+v", got)
			}
		})
	}
}

func TestRuntimeNodeEvidenceRetainsCoverageAndUnknownIdentity(t *testing.T) {
	a, b, c, other, old := linuxNode("a"), linuxNode("b"), linuxNode("c"), linuxNode("other"), linuxNode("old")
	a.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{
		{Name: "opaque", Features: &corev1.NodeRuntimeHandlerFeatures{UserNamespaces: ptr.To(true), RecursiveReadOnlyMounts: ptr.To(false)}},
		{Name: "", Features: &corev1.NodeRuntimeHandlerFeatures{UserNamespaces: ptr.To(true)}},
	}
	b.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{{Name: "opaque", Features: &corev1.NodeRuntimeHandlerFeatures{UserNamespaces: ptr.To(false)}}}
	c.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{{Name: "opaque"}}
	other.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{{Name: "runc"}}
	excluded := a.DeepCopy()
	excluded.Name, excluded.Labels["pool"] = "excluded", "elsewhere"
	unready := a.DeepCopy()
	unready.Name, unready.Status.Conditions[0].Status = "unready", corev1.ConditionFalse
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "custom"}, Handler: "opaque", Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "agents"}}}
	status := Discover(t.Context(), testClient(t, a, b, c, other, old, excluded, unready, rc))
	got := findRuntime(t, status, rc.Name)
	wantEvidence := &v1.RuntimeHandlerEvidence{ReportedNodes: 3, MissingFromReportedInventoryNodes: 1, NoInventoryNodes: 1}
	wantFeatures := &v1.RuntimeFeaturesObservation{
		UserNamespaces:          v1.RuntimeFeatureObservation{ReportedSupportedNodes: 1, ReportedUnsupportedNodes: 1, UnknownNodes: 3},
		RecursiveReadOnlyMounts: v1.RuntimeFeatureObservation{ReportedUnsupportedNodes: 1, UnknownNodes: 4},
	}
	if got.EligibleNodes != 5 || !reflect.DeepEqual(got.HandlerEvidence, wantEvidence) || !reflect.DeepEqual(got.Features, wantFeatures) {
		t.Fatalf("node reports lost coverage or included excluded nodes: %+v, %+v, %+v", got, got.HandlerEvidence, got.Features)
	}
	if got.Family != "unknown" || got.Status != "Unknown" || status.DefaultRuntime.Category != "Unknown" {
		t.Fatal("native handler reports were treated as implementation identity")
	}
	if len(status.RuntimeHandlers) != 3 || status.RuntimeHandlers[0].Name != "" || status.RuntimeHandlers[1].Name != "opaque" || status.RuntimeHandlers[1].ReportedNodes != 5 {
		t.Fatal("raw handler inventory lost reports or inferred scheduling availability", status.RuntimeHandlers)
	}
	featureFound := false
	for _, capability := range status.Capabilities {
		featureFound = featureFound || capability.Name == "UserNamespaces"
		if capability.Name == "UserNamespaces" && (capability.Status != "Observed" || capability.Resources[0].Source != "NativeAPI") {
			t.Fatal("feature summary lost native provenance", capability)
		}
	}
	if !featureFound {
		t.Fatal("native feature capability was not derived")
	}
}

func TestRuntimeMappingsCannotEraseHandlerMismatch(t *testing.T) {
	node := linuxNode("worker")
	node.Status.RuntimeHandlers = []corev1.NodeRuntimeHandler{{Name: "runc"}}
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "custom", Annotations: map[string]string{"arally.io/runtime-classification": "invalid"}}, Handler: "opaque"}
	report := &v1.ClusterCapabilities{Spec: v1.ClusterCapabilitiesSpec{RuntimeMappings: []v1.RuntimeMapping{
		{RuntimeClassName: rc.Name, Family: "declared", Category: "VirtualMachine", ImageContract: "LinuxOCI"},
	}}}
	(Observer{Reader: testClient(t, node, rc)}).Refresh(t.Context(), report, All, true)
	got := findRuntime(t, report.Status, rc.Name)
	if got.Family != "declared" || got.Source != "AdministratorDeclared" || got.Status != "Unknown" || got.Reason != "RuntimeHandlerNotReported" || got.HandlerEvidence.MissingFromReportedInventoryNodes != 1 {
		t.Fatal("mapping bypassed native evidence or failed to override invalid metadata", got)
	}
	copy := report.DeepCopy()
	copy.Status.Runtimes[0].HandlerEvidence.ReportedNodes++
	copy.Status.Runtimes[0].Features.UserNamespaces.UnknownNodes++
	if reflect.DeepEqual(copy.Status.Runtimes[0], report.Status.Runtimes[0]) {
		t.Fatal("runtime evidence shares mutable state across DeepCopy")
	}
}
