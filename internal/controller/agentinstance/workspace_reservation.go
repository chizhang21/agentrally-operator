package agentinstance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/diagnostics"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const workspaceReservationLabel = "arally.io/workspace-reservation"
const workspaceReservationReady = "arally.io/workspace-reservation-ready"

func workspaceReservation(instance *v1.AgentInstance, binding *v1.WorkspaceBinding) *coordinationv1.Lease {
	sum := sha256.Sum256([]byte(binding.UID))
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: fmt.Sprintf("agentrally-workspace-%x", sum[:16]), Namespace: instance.Namespace,
		Labels: map[string]string{workspaceReservationLabel: "true"},
	}, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(string(instance.UID))}}
}

func (r *Reconciler) readWorkspaceReservation(ctx context.Context, instance *v1.AgentInstance, binding *v1.WorkspaceBinding) (*coordinationv1.Lease, error) {
	if binding.UID == "" {
		return nil, nil
	}
	lease := workspaceReservation(instance, binding)
	err := r.referenceReader().Get(ctx, client.ObjectKeyFromObject(lease), lease)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lease.DeletionTimestamp != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(instance.UID) || controller.CheckOwner(lease, instance) != nil {
		return nil, diagnostics.New("WorkspaceInUse", "The external workspace is reserved by another AgentInstance; wait for its cleanup and deletion", nil)
	}
	return lease, nil
}

// Create is the arbitration point between exclusive contenders. Shared callers
// persist their binding before checking the reservation; after winning Create,
// read those bindings directly before accepting private access.
func (r *Reconciler) reserveWorkspace(ctx context.Context, instance *v1.AgentInstance, binding *v1.WorkspaceBinding) error {
	if binding.UID == "" || instance.UID == "" {
		return diagnostics.New("InvalidConfiguration", "Exclusive workspace binding requires PVC and Instance UIDs", nil)
	}
	lease, err := r.readWorkspaceReservation(ctx, instance, binding)
	if err != nil {
		return err
	}
	if lease == nil {
		lease = workspaceReservation(instance, binding)
		if err := ctrl.SetControllerReference(instance, lease, r.Scheme); err != nil {
			return err
		}
		if err := r.directClient().Create(ctx, lease); apierrors.IsAlreadyExists(err) {
			return diagnostics.New("WorkspaceReservationChanged", "The reservation changed during acquisition; retry before using the workspace", nil)
		} else if err != nil {
			return err
		}
	}
	if lease.Annotations[workspaceReservationReady] == "true" {
		return nil
	}
	instances := &v1.AgentInstanceList{}
	err = r.referenceReader().List(ctx, instances, client.InNamespace(instance.Namespace))
	if err == nil {
		for _, other := range instances.Items {
			if other.UID != instance.UID && other.Status.Workspace != nil && other.Status.Workspace.UID == binding.UID {
				err = diagnostics.New("WorkspaceInUse", "Another AgentInstance is already bound to the external workspace", nil)
				break
			}
		}
	}
	if err != nil {
		if _, deleteErr := controller.DeleteOwned(ctx, r.directClient(), lease, instance); deleteErr != nil {
			return errors.Join(err, deleteErr)
		}
		return err
	}
	// Persist validation separately from acquisition. A failed inventory read or
	// rollback must not turn a shared binding into private access on the retry.
	base := lease.DeepCopy()
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[workspaceReservationReady] = "true"
	return r.directClient().Patch(ctx, lease, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) releaseWorkspace(ctx context.Context, instance *v1.AgentInstance) error {
	leases := &coordinationv1.LeaseList{}
	if err := r.referenceReader().List(ctx, leases, client.InNamespace(instance.Namespace), client.MatchingLabels{workspaceReservationLabel: "true"}); err != nil {
		return err
	}
	for i := range leases.Items {
		lease := &leases.Items[i]
		if controller.CheckOwner(lease, instance) != nil {
			continue
		}
		if _, err := controller.DeleteOwned(ctx, r.directClient(), lease, instance); err != nil {
			return err
		}
	}
	return nil
}
