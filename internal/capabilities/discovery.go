// Package capabilities observes native cluster integrations without configuring them.
package capabilities

import (
	"context"
	"errors"
	"slices"
	"time"

	v1 "agentrally/operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RefreshInterval = 90 * time.Second
const retryInterval = 10 * time.Second
const sectionTimeout = 5 * time.Second

type Section uint8

const (
	Runtime Section = 1 << iota
	Storage
	Network
	All = Runtime | Storage | Network
)

func (s Section) Condition() string {
	switch s {
	case Runtime:
		return "RuntimeDiscoveryReady"
	case Storage:
		return "StorageDiscoveryReady"
	case Network:
		return "NetworkPolicyDiscoveryReady"
	}
	panic("unknown capability section")
}

// Observer computes cache candidates to suppress unchanged event-triggered reads.
// Only direct API audits publish evidence and renew its freshness. A lagging cache
// may cause another audit, but can never overwrite newer audited observations.
// Refresh mutates only the supplied status; its caller persists the result.
type Observer struct {
	Reader client.Reader
	Cache  client.Reader
	Now    func() time.Time
}

// Native candidates use existing informers. Optional APIs stay direct so a
// candidate cannot create a mandatory informer for an uninstalled component.
type candidateReader struct {
	client.Reader
	api client.Reader
}

func (r candidateReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, optional := list.(*unstructured.UnstructuredList); optional {
		return r.api.List(ctx, list, options...)
	}
	return r.Reader.List(ctx, list, options...)
}

func (o Observer) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

// Discover collects a complete default report. Refresh is the same observation
// path with persisted intent, section selection and a controlled clock.
func Discover(ctx context.Context, reader client.Reader) v1.ClusterCapabilitiesStatus {
	report := &v1.ClusterCapabilities{}
	(Observer{Reader: reader}).Refresh(ctx, report, All, true)
	return report.Status
}

func (o Observer) Refresh(ctx context.Context, report *v1.ClusterCapabilities, dirty Section, force bool) time.Duration {
	force = force || report.Status.ObservedGeneration != report.Generation || report.Status.CatalogRevision != builtins.revision
	for _, retired := range []string{"DiscoveryReady", "DiscoverySucceeded", "SnapshotDiscoveryReady"} {
		meta.RemoveStatusCondition(&report.Status.Conditions, retired)
	}
	report.Status.Sections = slices.DeleteFunc(report.Status.Sections, func(section v1.DiscoverySectionObservation) bool {
		return section.Name == "SnapshotDiscoveryReady"
	})
	nextAudit := o.now().Add(RefreshInterval)
	attempted := false
	for section := Runtime; section <= Network; section <<= 1 {
		now := o.now()
		previous := sectionObservation(report.Status, section)
		interval := RefreshInterval
		condition := meta.FindStatusCondition(report.Status.Conditions, section.Condition())
		if condition == nil || condition.Status != metav1.ConditionTrue {
			interval = retryInterval
		}
		due := force || previous.LastAttemptAt == nil || now.Before(previous.LastAttemptAt.Time) || !now.Before(previous.LastAttemptAt.Add(interval))
		if !due && dirty&section != 0 {
			if o.Cache == nil {
				due = true
			} else {
				candidateCtx, cancel := context.WithTimeout(ctx, time.Second)
				candidate, err := collect(candidateCtx, candidateReader{Reader: o.Cache, api: o.Reader}, report.Spec, section)
				cancel()
				due = err != nil || !sameSection(report.Status, candidate, section)
			}
		}
		if due {
			sectionCtx, cancel := context.WithTimeout(ctx, sectionTimeout)
			current, err := collect(sectionCtx, o.Reader, report.Spec, section)
			cancel()
			observed := metav1.NewTime(o.now())
			attempted = true
			previous.LastAttemptAt = &observed
			condition := metav1.Condition{Type: section.Condition(), Status: metav1.ConditionTrue, ObservedGeneration: report.Generation,
				Reason: "ResourcesObserved", Message: "Native configuration observed; execution and enforcement are not probed"}
			if err != nil {
				current = v1.ClusterCapabilitiesStatus{}
				if section == Network {
					current.NetworkPolicy = &v1.NetworkPolicyObservation{Status: "Unknown", Reason: "DiscoveryFailed"}
				}
				condition.Status = metav1.ConditionFalse
				condition.Reason, condition.Message = readFailure(err)
				interval = retryInterval
			} else {
				previous.LastSuccessAt = observed.DeepCopy()
				interval = RefreshInterval
			}
			applySection(&report.Status, current, section)
			putSectionObservation(&report.Status, previous)
			meta.SetStatusCondition(&report.Status.Conditions, condition)
			report.Status.ObservedAt = &observed
		}
		previous = sectionObservation(report.Status, section)
		if previous.LastAttemptAt != nil {
			dueAt := previous.LastAttemptAt.Add(interval)
			if dueAt.Before(nextAudit) {
				nextAudit = dueAt
			}
		}
	}
	if attempted {
		report.Status.ObservedGeneration = report.Generation
		report.Status.CatalogRevision = builtins.revision
		report.Status.Capabilities = summarize(report.Status)
	}
	// Account for time spent auditing other sections before scheduling a retry.
	return max(time.Millisecond, nextAudit.Sub(o.now()))
}

func collect(ctx context.Context, reader client.Reader, spec v1.ClusterCapabilitiesSpec, section Section) (v1.ClusterCapabilitiesStatus, error) {
	status := v1.ClusterCapabilitiesStatus{}
	var err error
	switch section {
	case Runtime:
		err = discoverRuntimes(ctx, reader, &status)
	case Storage:
		err = discoverStorage(ctx, reader, &status)
	case Network:
		err = discoverNetwork(ctx, reader, &status)
	}
	if err == nil {
		applyMappings(&status, spec, section)
	}
	return status, err
}

func sameSection(left, right v1.ClusterCapabilitiesStatus, section Section) bool {
	a, b := v1.ClusterCapabilitiesStatus{}, v1.ClusterCapabilitiesStatus{}
	applySection(&a, left, section)
	applySection(&b, right, section)
	return equality.Semantic.DeepEqual(a, b)
}

func applySection(target *v1.ClusterCapabilitiesStatus, source v1.ClusterCapabilitiesStatus, section Section) {
	switch section {
	case Runtime:
		target.DefaultRuntime, target.Runtimes, target.NodeRuntimes = source.DefaultRuntime, source.Runtimes, source.NodeRuntimes
		target.RuntimeHandlers = source.RuntimeHandlers
	case Storage:
		target.StorageClasses, target.CSIDrivers, target.CSIDriverDetails = source.StorageClasses, source.CSIDrivers, source.CSIDriverDetails
	case Network:
		target.NetworkPolicy = source.NetworkPolicy
	}
}

func sectionObservation(status v1.ClusterCapabilitiesStatus, section Section) v1.DiscoverySectionObservation {
	for _, observed := range status.Sections {
		if observed.Name == section.Condition() {
			return observed
		}
	}
	return v1.DiscoverySectionObservation{Name: section.Condition()}
}

func putSectionObservation(status *v1.ClusterCapabilitiesStatus, observed v1.DiscoverySectionObservation) {
	for i := range status.Sections {
		if status.Sections[i].Name == observed.Name {
			status.Sections[i] = observed
			return
		}
	}
	status.Sections = append(status.Sections, observed)
}

func readFailure(err error) (string, string) {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return "PermissionDenied", "Cannot read discovery inputs; check Operator read permissions"
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "DiscoveryTimeout", "Discovery timed out; check API connectivity and retry"
	case meta.IsNoMatchError(err):
		return "APIUnavailable", "A required discovery API is unavailable"
	default:
		return "DiscoveryFailed", "Cannot read discovery inputs; check API connectivity and permissions"
	}
}
