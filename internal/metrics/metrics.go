// Package metrics exposes bounded Operator observations through the manager's
// existing Prometheus registry. Collection reads the cache, never credentials.
package metrics

import (
	"context"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Observer struct {
	reader     client.Reader
	allocation *prometheus.HistogramVec
	startup    *prometheus.HistogramVec
	suspension prometheus.Histogram
	cleanup    prometheus.Histogram
	instances  *prometheus.Desc
	pools      *prometheus.Desc
	spares     *prometheus.Desc
	pvcs       *prometheus.Desc
	storage    *prometheus.Desc
	success    *prometheus.Desc
}

func New(reader client.Reader) *Observer {
	buckets := []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800}
	opts := func(name, help string) prometheus.HistogramOpts {
		return prometheus.HistogramOpts{Namespace: "agentrally", Name: name, Help: help, Buckets: buckets}
	}
	return &Observer{
		reader:     reader,
		allocation: prometheus.NewHistogramVec(opts("agent_allocation_duration_seconds", "Observed time from persisted selection to validated Sandbox assignment."), []string{"launch_type"}),
		startup:    prometheus.NewHistogramVec(opts("agent_startup_duration_seconds", "Observed time from Pod creation to first execution availability."), []string{"launch_type"}),
		suspension: prometheus.NewHistogram(opts("agent_suspension_duration_seconds", "Observed time from suspension submission to Core confirmation and Pod removal.")),
		cleanup:    prometheus.NewHistogram(opts("agent_cleanup_duration_seconds", "Observed time from cleanup submission to owned allocation and access resource removal.")),
		instances:  prometheus.NewDesc("agentrally_agentinstances", "Cached instance counts; states overlap.", []string{"state"}, nil),
		pools:      prometheus.NewDesc("agentrally_pools", "Native pools in observed Group namespaces.", nil, nil),
		spares:     prometheus.NewDesc("agentrally_pool_spare_replicas", "Desired and current-generation ready spare capacity; excludes terminating resources.", []string{"state"}, nil),
		pvcs:       prometheus.NewDesc("agentrally_namespace_pvcs", "All PVCs in observed Group namespaces, including external and deleting PVCs.", nil, nil),
		storage:    prometheus.NewDesc("agentrally_namespace_storage_bytes", "Requested and provisioned PVC capacity; not disk usage.", []string{"state"}, nil),
		success:    prometheus.NewDesc("agentrally_observation_success", "Whether cached instance and Group collection succeeded.", nil, nil),
	}
}

func (o *Observer) Describe(ch chan<- *prometheus.Desc) {
	for _, collector := range []prometheus.Collector{o.allocation, o.startup, o.suspension, o.cleanup} {
		collector.Describe(ch)
	}
	for _, desc := range []*prometheus.Desc{o.instances, o.pools, o.spares, o.pvcs, o.storage, o.success} {
		ch <- desc
	}
}

func (o *Observer) Collect(ch chan<- prometheus.Metric) {
	for _, collector := range []prometheus.Collector{o.allocation, o.startup, o.suspension, o.cleanup} {
		collector.Collect(ch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instances, groups := &v1.AgentInstanceList{}, &v1.AgentGroupList{}
	if err := o.reader.List(ctx, instances); err != nil {
		ch <- prometheus.MustNewConstMetric(o.success, prometheus.GaugeValue, 0)
		return
	}
	if err := o.reader.List(ctx, groups); err != nil {
		ch <- prometheus.MustNewConstMetric(o.success, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(o.success, prometheus.GaugeValue, 1)
	counts := controller.SummarizeInstances(instances.Items)
	for state, count := range map[string]int32{"total": counts.Total, "ready": counts.Ready, "available": counts.Available, "pending": counts.Pending, "suspended": counts.Suspended, "expired": counts.Expired} {
		ch <- prometheus.MustNewConstMetric(o.instances, prometheus.GaugeValue, float64(count), state)
	}
	var pools, desired, ready, pvcs, requested, capacity float64
	for _, group := range groups.Items {
		if summary := group.Status.Summary; summary != nil {
			pools += float64(summary.Pools.Total)
			desired += float64(summary.Pools.DesiredSpareReplicas)
			ready += float64(summary.Pools.ReadySpareReplicas)
			pvcs += float64(summary.Storage.PVCs)
			requested += summary.Storage.Requested.AsApproximateFloat64()
			capacity += summary.Storage.Capacity.AsApproximateFloat64()
		}
	}
	ch <- prometheus.MustNewConstMetric(o.pools, prometheus.GaugeValue, pools)
	ch <- prometheus.MustNewConstMetric(o.spares, prometheus.GaugeValue, desired, "desired")
	ch <- prometheus.MustNewConstMetric(o.spares, prometheus.GaugeValue, ready, "ready")
	ch <- prometheus.MustNewConstMetric(o.pvcs, prometheus.GaugeValue, pvcs)
	ch <- prometheus.MustNewConstMetric(o.storage, prometheus.GaugeValue, requested, "requested")
	ch <- prometheus.MustNewConstMetric(o.storage, prometheus.GaugeValue, capacity, "capacity")
}

// RecordTransition is called only after a successful status patch. Re-observing
// persisted timestamps emits no duplicate sample. Like other process metrics,
// histograms reset on restart; status remains the durable timing record.
func (o *Observer) RecordTransition(before, after v1.AgentInstanceStatus) {
	if o == nil {
		return
	}
	launch := "unknown"
	if state := after.Provisioning; state != nil && (state.LaunchType == "warm" || state.LaunchType == "cold") {
		launch = state.LaunchType
	}
	observe := func(previous, start, end *metav1.Time, metric prometheus.Observer) {
		if start != nil && end != nil && !end.Equal(previous) && !end.Before(start) {
			metric.Observe(end.Sub(start.Time).Seconds())
		}
	}
	if state := after.Provisioning; state != nil {
		var previous *metav1.Time
		if before.Provisioning != nil {
			previous = before.Provisioning.CompletedAt
		}
		observe(previous, state.StartedAt, state.CompletedAt, o.allocation.WithLabelValues(launch))
	}
	if after.Timings == nil {
		return
	}
	previous := before.Timings
	if previous == nil {
		previous = &v1.AgentInstanceTimings{}
	}
	next := after.Timings
	previousReady := previous.PodReadyAt
	if after.PodUID != "" && before.PodUID != after.PodUID {
		previousReady = nil
	}
	observe(previousReady, next.PodCreatedAt, next.PodReadyAt, o.startup.WithLabelValues(launch))
	observe(previous.SuspendedAt, next.SuspensionStartedAt, next.SuspendedAt, o.suspension)
	observe(previous.CleanupCompletedAt, next.CleanupStartedAt, next.CleanupCompletedAt, o.cleanup)
}
