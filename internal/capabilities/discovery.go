// Package capabilities observes native cluster integrations without configuring them.
package capabilities

import (
	"context"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RefreshInterval = 30 * time.Second

// Discover always constructs a new observation, preventing stale success on errors.
// Section conditions indicate successful reads, independently of backend availability.
func Discover(ctx context.Context, reader client.Reader) v1.ClusterCapabilitiesStatus {
	status := v1.ClusterCapabilitiesStatus{}
	scans := []struct {
		name string
		run  func(context.Context, client.Reader, *v1.ClusterCapabilitiesStatus) error
	}{
		{"RuntimeDiscoveryReady", discoverRuntimes},
		{"StorageDiscoveryReady", discoverStorage},
		{"NetworkPolicyDiscoveryReady", discoverNetwork},
	}
	complete := true
	for _, scan := range scans {
		condition := metav1.Condition{Type: scan.name, Status: metav1.ConditionTrue, Reason: "ResourcesObserved", Message: "Native resource configuration observed; execution and enforcement are not probed"}
		if err := scan.run(ctx, reader, &status); err != nil {
			complete = false
			condition.Status = metav1.ConditionFalse
			condition.Reason = "DiscoveryFailed"
			condition.Message = "Cannot read Kubernetes resources; check operator permissions and API server connectivity"
		}
		meta.SetStatusCondition(&status.Conditions, condition)
	}
	condition := metav1.Condition{Type: "DiscoveryReady", Status: metav1.ConditionTrue, Reason: "ResourcesObserved", Message: "All discovery sections completed; configuration evidence is not runtime verification"}
	if !complete {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "PartialDiscovery"
		condition.Message = "One or more discovery sections failed; inspect section conditions"
	}
	meta.SetStatusCondition(&status.Conditions, condition)
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	status.ObservedAt = &now
	return status
}
