package agentinstance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type executionEventsReader struct {
	client.Reader
	events client.Reader
	err    error
	lists  int
}

func (r *executionEventsReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.EventList); !ok {
		return r.Reader.List(ctx, list, opts...)
	}
	r.lists++
	if r.err != nil {
		return r.err
	}
	return r.events.List(ctx, list, opts...)
}

func podEvent(pod *corev1.Pod, name, reason string, at time.Time) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pod.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: "do-not-expose", LastTimestamp: metav1.NewTime(at)}
}

func TestExecutionEventsReachAgentInstance(t *testing.T) {
	for _, tc := range []struct{ event, reason string }{
		{"FailedMount", "VolumeMountFailed"},
		{"FailedAttachVolume", "VolumeAttachFailed"},
		{"FailedCreatePodSandBox", "PodSandboxCreationFailed"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			g, ns, definition, w, source := testutil.Fixture()
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), g, ns, w)
			recorder := events.NewFakeRecorder(100)
			r := &Reconciler{Client: c, Scheme: scheme, Recorder: recorder}
			reconcileWithUpstreamSimulation(t, r, w)
			pod := ownedExecutionPod(t, c, w)
			pod.Status.Phase = corev1.PodPending
			if err := c.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			event := podEvent(pod, "failure", tc.event, time.Now())
			events := fake.NewClientBuilder().WithScheme(scheme).WithObjects(event).
				WithIndex(&corev1.Event{}, "involvedObject.uid", func(object client.Object) []string {
					return []string{string(object.(*corev1.Event).InvolvedObject.UID)}
				}).Build()
			reader := &executionEventsReader{Reader: c, events: events}
			r.Reader = reader
			reconcileWithUpstreamSimulation(t, r, w)
			state := getAgentInstance(t, c, w)
			assertReason(t, state, "Ready", tc.reason)
			if reader.lists != 1 {
				t.Fatalf("unexpected Event requests: %d", reader.lists)
			}
			for len(recorder.Events) > 0 {
				if event := <-recorder.Events; strings.Contains(event, "do-not-expose") {
					t.Fatal("raw kubelet Event message leaked")
				}
			}
			// Event reads are optional; failures must still show Pod progress.
			reader.err = fmt.Errorf("event access denied: do-not-expose")
			reconcileWithUpstreamSimulation(t, r, w)
			assertReason(t, getAgentInstance(t, c, w), "Ready", "PodStarting")
			// Same-name Pods belonging to a different Sandbox must not contribute
			// status or trigger an Event lookup, even when Core reports Ready.
			reader.lists = 0
			pod.OwnerReferences[0].UID = "unrelated-sandbox"
			if err := c.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}
			reconcileWithUpstreamSimulation(t, r, w)
			state = getAgentInstance(t, c, w)
			assertReason(t, state, "Ready", "PodObservationBlocked")
			if reader.lists != 0 || state.Status.PodName != "" {
				t.Fatal("unowned Pod was observed as the execution")
			}
		})
	}
}

func TestExecutionEventEvidenceDoesNotOutliveProgress(t *testing.T) {
	now := time.Now().UTC()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "arally-demo", UID: types.UID("current-pod"), CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))}}
	failure := podEvent(pod, "failure", "FailedMount", now)
	wrongUID := failure.DeepCopy()
	wrongUID.InvolvedObject.UID = "previous-pod"
	older := failure.DeepCopy()
	older.LastTimestamp = metav1.NewTime(now.Add(-2 * time.Minute))
	progress := podEvent(pod, "progress", "Created", now.Add(time.Second))
	progress.Type = corev1.EventTypeNormal
	series := failure.DeepCopy()
	series.Series = &corev1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(now.Add(2 * time.Second))}
	for _, tc := range []struct {
		name   string
		events []corev1.Event
		want   bool
	}{
		{"current", []corev1.Event{*failure}, true},
		{"other-pod", []corev1.Event{*wrongUID}, false},
		{"before-creation", []corev1.Event{*older}, false},
		{"normal-progress", []corev1.Event{*failure, *progress}, false},
		{"recurring-failure", []corev1.Event{*series, *progress}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := latestPodEventDiagnostic(pod, tc.events)
			if (issue != nil) != tc.want {
				t.Fatalf("unexpected diagnosis: %+v", issue)
			}
			if issue != nil && strings.Contains(issue.Message, "do-not-expose") {
				t.Fatal("raw Event message leaked")
			}
		})
	}
}
