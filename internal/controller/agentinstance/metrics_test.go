package agentinstance

import (
	"context"
	"testing"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/metrics"
	"arally/operator/internal/testutil"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMetricsFollowPersistedTransitionsAndCachedState(t *testing.T) {
	group, ns, definition, w, _ := testutil.Fixture()
	group.Status.Summary = &v1.AgentGroupSummary{Pools: v1.AgentPoolSummary{Total: 1, DesiredSpareReplicas: 2, ReadySpareReplicas: 1}}
	c, scheme := clientWithPublishedDefinitions(t, definition, group, ns, w)
	readFailure := false
	reader := interceptor.NewClient(c, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if readFailure {
			return apierrors.NewServiceUnavailable("cache unavailable")
		}
		return c.List(ctx, list, opts...)
	}})
	observer := metrics.New(reader)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(observer)
	base := getAgentInstance(t, c, w)
	start := metav1.NewTime(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	end := metav1.NewTime(start.Add(5 * time.Second))
	w = base.DeepCopy()
	w.Status.PodUID = "pod-1"
	w.Status.Provisioning = &v1.AgentInstanceProvisioningStatus{StartedAt: &start, CompletedAt: &end, LaunchType: "warm"}
	w.Status.Timings = &v1.AgentInstanceTimings{PodCreatedAt: &start, PodReadyAt: &end, SuspensionStartedAt: &start, SuspendedAt: &end, CleanupStartedAt: &start, CleanupCompletedAt: &end}
	failWrite := true
	writer := interceptor.NewClient(c, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if failWrite {
			return apierrors.NewServiceUnavailable("status unavailable")
		}
		return c.SubResource(subresource).Patch(ctx, object, patch, opts...)
	}})
	r := &Reconciler{Client: writer, Scheme: scheme, Metrics: observer}
	if err := r.patchStatus(ctx, w, base); err == nil {
		t.Fatal("expected status write failure")
	}
	counts := func() map[string]uint64 {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]uint64{}
		for _, family := range families {
			for _, metric := range family.Metric {
				if metric.Histogram != nil {
					result[family.GetName()] += metric.Histogram.GetSampleCount()
					if metric.Histogram.GetSampleCount() > 0 && metric.Histogram.GetSampleSum() != 5*float64(metric.Histogram.GetSampleCount()) {
						t.Fatal("incorrect duration", metric)
					}
				}
				for _, label := range metric.Label {
					if label.GetName() != "state" && label.GetName() != "launch_type" {
						t.Fatal("instance identity leaked into metric labels", label)
					}
				}
			}
		}
		return result
	}
	for _, count := range counts() {
		if count != 0 {
			t.Fatal("failed persistence emitted a duration sample")
		}
	}
	failWrite = false
	if err := r.patchStatus(ctx, w, base); err != nil {
		t.Fatal(err)
	}
	if err := r.patchStatus(ctx, w, w.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"arally_agent_allocation_duration_seconds", "arally_agent_startup_duration_seconds", "arally_agent_suspension_duration_seconds", "arally_agent_cleanup_duration_seconds"} {
		if counts()[name] != 1 {
			t.Fatal("missing or duplicated persisted observation", name)
		}
	}
	base = w.DeepCopy()
	w.Status.PodUID = ""
	if err := r.patchStatus(ctx, w, base); err != nil {
		t.Fatal(err)
	}
	if counts()["arally_agent_startup_duration_seconds"] != 1 {
		t.Fatal("Pod removal recounted old startup")
	}
	base = w.DeepCopy()
	w.Status.PodUID = "pod-2"
	if err := r.patchStatus(ctx, w, base); err != nil {
		t.Fatal(err)
	}
	if counts()["arally_agent_startup_duration_seconds"] != 2 {
		t.Fatal("new Pod with same timestamp lost startup observation")
	}
	readFailure = true
	families, err := registry.Gather()
	if err != nil {
		t.Fatal("cache read failure broke all metric exposition", err)
	}
	for _, family := range families {
		if family.GetName() == "arally_observation_success" && !ptr.Equal(family.Metric[0].Gauge.Value, ptr.To(float64(0))) {
			t.Fatal("failed cache read reported success")
		}
	}
}
