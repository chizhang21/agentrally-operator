package controller

import (
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
)

func TestConditionEventsEmitOnlyMeaningfulTransitions(t *testing.T) {
	w := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "arally-team"}}
	recorder := events.NewFakeRecorder(10)
	before := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionUnknown, Reason: "SandboxPending"}}
	after := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "PodFailed", Message: "task failed"}, {Type: "Finished", Status: metav1.ConditionTrue, Reason: "PodFailed", Message: "task failed"}}
	RecordConditionChanges(recorder, w, before, after)
	for range 2 {
		select {
		case event := <-recorder.Events:
			if !strings.Contains(event, "Warning PodFailed") {
				t.Fatal(event)
			}
		default:
			t.Fatal("failure event missing")
		}
	}
	unchanged := append([]metav1.Condition(nil), after...)
	unchanged[0].Message = "details changed"
	unchanged[0].ObservedGeneration = 2
	RecordConditionChanges(recorder, w, after, unchanged)
	select {
	case event := <-recorder.Events:
		t.Fatal("duplicate event", event)
	default:
	}
	expected := []metav1.Condition{{Type: "Suspended", Status: metav1.ConditionTrue, Reason: "PodTerminated", Message: "suspended"}, {Type: "Finished", Status: metav1.ConditionTrue, Reason: "PodSucceeded", Message: "completed"}}
	RecordConditionChanges(recorder, w, nil, expected)
	for range 2 {
		select {
		case event := <-recorder.Events:
			if !strings.HasPrefix(event, "Normal ") {
				t.Fatal(event)
			}
		default:
			t.Fatal("completion event missing")
		}
	}
	RecordConditionChanges(nil, w, nil, expected)
}
