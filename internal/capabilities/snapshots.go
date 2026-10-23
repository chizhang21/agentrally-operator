package capabilities

import (
	"context"
	"sort"

	v1 "arally/operator/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The optional API is read directly during audits, with no mandatory informer.
func discoverSnapshots(ctx context.Context, reader client.Reader, status *v1.ClusterCapabilitiesStatus) error {
	classes := &unstructured.UnstructuredList{}
	classes.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClassList"})
	if err := reader.List(ctx, classes); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) || apierrors.IsNotFound(err) {
			status.Snapshots = &v1.SnapshotObservation{ClassAPIStatus: "Unavailable", Reason: "SnapshotAPINotInstalled"}
			return nil
		}
		return err
	}
	status.Snapshots = &v1.SnapshotObservation{ClassAPIStatus: "Available"}
	for _, class := range classes.Items {
		if class.GetDeletionTimestamp() != nil {
			continue
		}
		driver, _, err := unstructured.NestedString(class.Object, "driver")
		if err != nil {
			return err
		}
		policy, _, err := unstructured.NestedString(class.Object, "deletionPolicy")
		if err != nil {
			return err
		}
		status.Snapshots.Classes = append(status.Snapshots.Classes, v1.SnapshotClassObservation{
			Name: class.GetName(), Driver: driver, DeletionPolicy: policy, Default: class.GetAnnotations()["snapshot.storage.kubernetes.io/is-default-class"] == "true",
		})
	}
	sort.Slice(status.Snapshots.Classes, func(i, j int) bool { return status.Snapshots.Classes[i].Name < status.Snapshots.Classes[j].Name })
	return nil
}
