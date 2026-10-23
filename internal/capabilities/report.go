package capabilities

import (
	"context"
	"time"

	v1 "arally/operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const capabilityMaxAge = 2 * time.Minute

// ReadReport returns the canonical report, or nil when evidence cannot be read.
func ReadReport(ctx context.Context, reader client.Reader) *v1.ClusterCapabilities {
	report := &v1.ClusterCapabilities{}
	if err := reader.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
		return nil
	}
	return report
}

// SectionCurrent checks generation, age and readiness of a discovery section.
func SectionCurrent(report *v1.ClusterCapabilities, section string, now time.Time) bool {
	if report == nil || report.DeletionTimestamp != nil || report.Status.ObservedGeneration != report.Generation || report.Status.ObservedAt == nil {
		return false
	}
	var success *metav1.Time
	for _, observed := range report.Status.Sections {
		if observed.Name == section {
			success = observed.LastSuccessAt
			break
		}
	}
	if success == nil {
		return false
	}
	age := now.Sub(success.Time)
	if age < 0 || age > capabilityMaxAge {
		return false
	}
	condition := meta.FindStatusCondition(report.Status.Conditions, section)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == report.Generation
}

// NetworkCondition translates discovery evidence into a Group preflight condition.
func NetworkCondition(report *v1.ClusterCapabilities, generation int64) metav1.Condition {
	condition := metav1.Condition{
		Type: "NetworkPolicySupported", Status: metav1.ConditionUnknown, ObservedGeneration: generation,
		Reason:  "CapabilityEvidenceUnavailable",
		Message: "NetworkPolicy support has not been confirmed; traffic enforcement is not verified",
	}
	if !SectionCurrent(report, "NetworkPolicyDiscoveryReady", time.Now()) || report.Status.NetworkPolicy == nil {
		return condition
	}
	switch report.Status.NetworkPolicy.Status {
	case "Supported":
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "ProviderConfigurationObserved", "Ready NetworkPolicy provider configuration observed; traffic enforcement is not verified"
	case "Unsupported":
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "NetworkPolicyUnsupported", "Current cluster observations show no supported NetworkPolicy provider"
	}
	return condition
}
