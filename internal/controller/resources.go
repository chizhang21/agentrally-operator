// Package controller provides shared resource ownership, conditions and event
// handling. Individual reconcilers live in its subpackages.
package controller

import (
	"context"
	"fmt"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// CheckOwner rejects resources without the expected Arally controller owner UID.
func CheckOwner(object, owner client.Object) error {
	ref := metav1.GetControllerOf(object)
	if ref == nil || ref.UID != owner.GetUID() || ref.APIVersion != v1.GroupVersion.String() {
		return diagnostics.New("OwnershipConflict", fmt.Sprintf("%T %s/%s is not controlled by %s (UID %s)", object, object.GetNamespace(), object.GetName(), owner.GetName(), owner.GetUID()), nil)
	}
	return nil
}

// PrepareOwned checks ownership inside a CreateOrUpdate mutation. Resource-specific
// field updates stay with the typed caller; existing unowned objects are never adopted.
func PrepareOwned(object, owner client.Object, scheme *runtime.Scheme) error {
	if object.GetDeletionTimestamp() != nil {
		return diagnostics.New("DependencyTerminating", fmt.Sprintf("%T %s/%s is terminating", object, object.GetNamespace(), object.GetName()), nil)
	}
	if object.GetResourceVersion() != "" {
		if err := CheckOwner(object, owner); err != nil {
			return err
		}
	}
	return ctrl.SetControllerReference(owner, object, scheme)
}

// DeleteOwned requests foreground deletion with an owner check and UID precondition.
// Core and Kubernetes garbage collection handle child cleanup.
func DeleteOwned(ctx context.Context, c client.Client, object, owner client.Object) (bool, error) {
	if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	if err := CheckOwner(object, owner); err != nil {
		return false, err
	}
	if object.GetDeletionTimestamp() == nil {
		err := c.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationForeground),
			&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(object.GetUID())}})
		if err != nil {
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}
	}
	return false, nil
}

// AddFinalizer persists a missing finalizer with optimistic concurrency.
func AddFinalizer(ctx context.Context, c client.Client, object client.Object, finalizer string) error {
	if controllerutil.ContainsFinalizer(object, finalizer) {
		return nil
	}
	base := object.DeepCopyObject().(client.Object)
	controllerutil.AddFinalizer(object, finalizer)
	return c.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// RemoveFinalizer removes only the specified finalizer with optimistic concurrency.
func RemoveFinalizer(ctx context.Context, c client.Client, object client.Object, finalizer string) error {
	base := object.DeepCopyObject().(client.Object)
	controllerutil.RemoveFinalizer(object, finalizer)
	return c.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// DependencyRetry bounds polling of read-only dependencies and allocation APIs.
const DependencyRetry = 30 * time.Second
