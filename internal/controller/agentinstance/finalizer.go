package agentinstance

import (
	"context"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *Reconciler) finalize(ctx context.Context, workload *v1.AgentInstance) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workload, AgentInstanceFinalizer) {
		return ctrl.Result{}, nil
	}
	done, err := r.cleanupExecution(ctx, workload)
	if err != nil {
		return r.failed(ctx, workload, err)
	}
	if !done {
		return r.pending(ctx, workload, "Deleting", "Waiting for owned execution and interaction foreground cleanup")
	}
	if err := r.releaseWorkspace(ctx, workload); err != nil {
		return r.failed(ctx, workload, err)
	}
	return ctrl.Result{}, controller.RemoveFinalizer(ctx, r.Client, workload, AgentInstanceFinalizer)
}
