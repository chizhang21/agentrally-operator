package capabilities

import (
	"strings"
	"unicode/utf8"

	v1 "arally/operator/api/v1alpha1"

	nodev1 "k8s.io/api/node/v1"
	json "sigs.k8s.io/json"
)

const runtimeClassificationAnnotation = "arally.io/runtime-classification"

type runtimeClassification struct {
	Family        string `json:"family"`
	Category      string `json:"category"`
	ImageContract string `json:"imageContract"`
}

func (c runtimeClassification) valid() bool {
	return c.Family != "" && utf8.RuneCountInString(c.Family) <= 128 &&
		oneOf(c.Category, "Container", "UserSpaceKernel", "VirtualMachine", "WebAssembly", "Unknown") &&
		oneOf(c.ImageContract, "LinuxOCI", "WasmWASI", "Unknown") &&
		(c.Category == "Unknown" || c.ImageContract == "Unknown" || (c.Category == "WebAssembly") == (c.ImageContract == "WasmWASI"))
}

func (c runtimeClassification) apply(observed *v1.RuntimeObservation, source, rule string) {
	observed.Family, observed.Category, observed.ImageContract = c.Family, c.Category, c.ImageContract
	observed.Source, observed.Rule, observed.Status, observed.Reason = source, rule, "Observed", ""
	if c.Category == "Unknown" {
		observed.Status, observed.Reason = "Unknown", "RuntimeIsolationUnknown"
	} else if c.ImageContract == "Unknown" {
		observed.Reason = "RuntimeImageContractUnknown"
	}
}

func describeRuntime(r nodev1.RuntimeClass) v1.RuntimeObservation {
	c := v1.RuntimeObservation{Name: r.Name, Handler: r.Handler, Family: "unknown", Category: "Unknown", ImageContract: "Unknown", Status: "Unknown", Reason: "RuntimeTypeUnrecognized", Source: "NativeAPI", Scheduling: r.Scheduling.DeepCopy()}
	if r.Overhead != nil {
		c.Overhead = r.Overhead.PodFixed.DeepCopy()
	}
	if raw, present := r.Annotations[runtimeClassificationAnnotation]; present {
		c.Source, c.Rule, c.Reason = "RuntimeClassDeclared", "annotation/"+runtimeClassificationAnnotation, "RuntimeDeclarationInvalid"
		if len(raw) > 4096 {
			return c
		}
		var declaration struct {
			Version string `json:"version"`
			runtimeClassification
		}
		strict, err := json.UnmarshalStrict([]byte(raw), &declaration)
		if err != nil || len(strict) != 0 || !declaration.valid() {
			return c
		}
		if declaration.Version != "v1" {
			if declaration.Version != "" {
				c.Reason = "RuntimeDeclarationVersionUnsupported"
			}
			return c
		}
		declaration.runtimeClassification.apply(&c, c.Source, c.Rule)
		return c
	}

	exact, known := builtins.runtimeByHandler[strings.ToLower(r.Handler)]
	var metadata *runtimeDefinition
	for i := range builtins.Runtimes {
		rule := &builtins.Runtimes[i]
		if len(rule.Labels) == 0 || !matchesRuntimeLabels(rule.Labels, r.Labels) ||
			(len(rule.NodeSelector) > 0 && (r.Scheduling == nil || !matchesRuntimeLabels(rule.NodeSelector, r.Scheduling.NodeSelector))) {
			continue
		}
		if (known && exact.Family != rule.Family) || (metadata != nil && metadata.Family != rule.Family) {
			c.Source, c.Rule, c.Reason = "InstallerMetadata", "runtime/"+rule.Family, "RuntimeClassificationConflict"
			return c
		}
		metadata = rule
	}
	if known {
		exact.runtimeClassification.apply(&c, "BuiltInCatalog", "runtime/"+exact.Family)
	} else if metadata != nil {
		classification := metadata.runtimeClassification
		// Installer identity describes the family, not an unknown shim's image contract.
		classification.ImageContract = "Unknown"
		classification.apply(&c, "InstallerMetadata", "runtime/"+metadata.Family)
	}
	return c
}

func matchesRuntimeLabels(required, observed map[string]string) bool {
	for key, value := range required {
		if current, present := observed[key]; !present || current != value {
			return false
		}
	}
	return true
}

func runtimeEvidenceStatus(c *v1.RuntimeObservation) {
	if c.Status != "Observed" {
		return
	}
	if c.EligibleNodes == 0 {
		c.Status, c.Reason = "Unknown", "NoEligibleNodes"
	} else if evidence := c.HandlerEvidence; evidence != nil && evidence.ReportedNodes == 0 && evidence.NoInventoryNodes == 0 {
		c.Status, c.Reason = "Unknown", "RuntimeHandlerNotReported"
	}
}
