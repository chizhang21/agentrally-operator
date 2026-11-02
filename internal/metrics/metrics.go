// Package metrics exposes cached AgentRally state and resource attribution through
// the manager's existing Prometheus registry. Collection never reads credentials.
package metrics

import (
	"context"
	"strconv"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Observer struct {
	reader     client.Reader
	instances  *prometheus.Desc
	pools      *prometheus.Desc
	spares     *prometheus.Desc
	pvcs       *prometheus.Desc
	storage    *prometheus.Desc
	success    *prometheus.Desc
	podInfo    *prometheus.Desc
	volumeInfo *prometheus.Desc
}

func New(reader client.Reader) *Observer {
	identity := []string{"agent_namespace", "agentinstance", "agentinstance_uid", "agentdefinition", "agentdefinition_uid"}
	return &Observer{
		reader:     reader,
		instances:  prometheus.NewDesc("agentrally_agentinstances", "Cached instance counts; states overlap.", []string{"state"}, nil),
		pools:      prometheus.NewDesc("agentrally_pools", "Native pools in observed Group namespaces.", nil, nil),
		spares:     prometheus.NewDesc("agentrally_pool_spare_replicas", "Desired and current-generation ready spare capacity; excludes terminating resources.", []string{"state"}, nil),
		pvcs:       prometheus.NewDesc("agentrally_namespace_pvcs", "All PVCs in observed Group namespaces, including external and deleting PVCs.", nil, nil),
		storage:    prometheus.NewDesc("agentrally_namespace_storage_bytes", "Requested and provisioned PVC capacity; not disk usage.", []string{"state"}, nil),
		success:    prometheus.NewDesc("agentrally_observation_success", "Whether cached resource collection succeeded.", nil, nil),
		podInfo:    prometheus.NewDesc("agentrally_instance_pod_info", "Observed Instance and accepted Definition attribution for an execution Pod.", append(identity, "agent_pod", "agent_pod_uid"), nil),
		volumeInfo: prometheus.NewDesc("agentrally_instance_volume_info", "Whole-PVC attribution; unresolved or replaced claims cannot supply usage.", append(identity, "persistentvolumeclaim", "persistentvolumeclaim_uid", "resolved"), nil),
	}
}

func (o *Observer) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{o.instances, o.pools, o.spares, o.pvcs, o.storage, o.success, o.podInfo, o.volumeInfo} {
		ch <- desc
	}
}

func (o *Observer) Collect(ch chan<- prometheus.Metric) {
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
	claims := &corev1.PersistentVolumeClaimList{}
	if err := o.reader.List(ctx, claims); err != nil {
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
	byName := make(map[client.ObjectKey]*corev1.PersistentVolumeClaim, len(claims.Items))
	for i := range claims.Items {
		pvc := &claims.Items[i]
		byName[client.ObjectKeyFromObject(pvc)] = pvc
	}
	for _, instance := range instances.Items {
		o.collectIdentity(ch, &instance, byName)
	}
}

func (o *Observer) collectIdentity(ch chan<- prometheus.Metric, instance *v1.AgentInstance, claims map[client.ObjectKey]*corev1.PersistentVolumeClaim) {
	definition, definitionUID := instance.Spec.DefinitionRef.Name, ""
	if binding := instance.Status.Definition; binding != nil {
		definition, definitionUID = binding.Name, string(binding.UID)
	}
	identity := []string{instance.Namespace, instance.Name, string(instance.UID), definition, definitionUID}
	if instance.Status.PodName != "" && instance.Status.PodUID != "" {
		ch <- prometheus.MustNewConstMetric(o.podInfo, prometheus.GaugeValue, 1, append(identity, instance.Status.PodName, instance.Status.PodUID)...)
	}
	names := map[string]bool{}
	for _, name := range instance.Status.VolumeClaims {
		if name != "" {
			names[name] = true
		}
	}
	if workspace := instance.Status.Workspace; workspace != nil && workspace.ClaimName != "" {
		names[workspace.ClaimName] = true
	}
	for name := range names {
		pvc := claims[client.ObjectKey{Namespace: instance.Namespace, Name: name}]
		uid := ""
		resolved := pvc != nil && pvc.DeletionTimestamp == nil
		if pvc != nil {
			uid = string(pvc.UID)
		}
		if workspace := instance.Status.Workspace; workspace != nil && workspace.ClaimName == name {
			resolved = resolved && pvc.UID == workspace.UID
			if !resolved {
				uid = string(workspace.UID)
			}
		}
		ch <- prometheus.MustNewConstMetric(o.volumeInfo, prometheus.GaugeValue, 1, append(identity, name, uid, strconv.FormatBool(resolved))...)
	}
}
