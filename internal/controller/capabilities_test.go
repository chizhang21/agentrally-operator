package controller

import (
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestCapabilityEventsIgnoreAuditTimestamps(t *testing.T) {
	before := &v1.ClusterCapabilities{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: 1}}
	before.Status.ObservedAt = &metav1.Time{Time: time.Now().UTC()}
	before.Status.Sections = []v1.DiscoverySectionObservation{{Name: "RuntimeDiscoveryReady", LastAttemptAt: before.Status.ObservedAt.DeepCopy(), LastSuccessAt: before.Status.ObservedAt.DeepCopy()}}
	after := before.DeepCopy()
	after.Status.ObservedAt.Time = after.Status.ObservedAt.Add(30 * time.Second)
	after.Status.Sections[0].LastAttemptAt = after.Status.ObservedAt.DeepCopy()
	after.Status.Sections[0].LastSuccessAt = after.Status.ObservedAt.DeepCopy()
	filter := CapabilityChanges()
	if filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("timestamp-only refresh woke consumers")
	}
	after.Status.Conditions = []metav1.Condition{{Type: "StorageDiscoveryReady", Status: metav1.ConditionFalse, Reason: "DiscoveryFailed"}}
	if !filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("discovery failure did not wake consumers")
	}
	after = before.DeepCopy()
	after.Status.Runtimes = []v1.RuntimeObservation{{Name: "kata", Status: "Observed"}}
	if !filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("runtime change did not wake consumers")
	}
	if !filter.Delete(event.DeleteEvent{Object: before}) || !filter.Create(event.CreateEvent{Object: before}) {
		t.Fatal("report creation/deletion was suppressed")
	}
}
