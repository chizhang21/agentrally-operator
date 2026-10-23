package agentinstance

import (
	"context"
	"fmt"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/allocation"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const snapshotAPIGroup = "snapshot.storage.k8s.io"

// snapshotReferences performs optional, direct reads only for native snapshot
// sources. CSI owns restoration; preflight does not create storage resources or
// require the snapshot API when no PVC template uses it.
func snapshotReferences(ctx context.Context, reader client.Reader, namespace string, claims []corev1.PersistentVolumeClaimTemplate, retained map[string]types.UID) (map[string]types.UID, error) {
	refs := map[string]types.UID{}
	for _, claim := range claims {
		name, err := volumeSnapshotSource(claim.Spec, namespace)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		if uid := retained[name]; uid != "" {
			refs[name] = uid
			continue
		}
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(schema.GroupVersionKind{Group: snapshotAPIGroup, Version: "v1", Kind: "VolumeSnapshot"})
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
			if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
				return nil, diagnostics.New("SnapshotAPIUnavailable", "Install the VolumeSnapshot CRDs, snapshot controller and a compatible CSI snapshot driver before using snapshot sources", err)
			}
			if apierrors.IsNotFound(err) {
				return nil, diagnostics.New("SnapshotNotFound", fmt.Sprintf("VolumeSnapshot %s/%s does not exist", namespace, name), err)
			}
			return nil, diagnostics.New("SnapshotReadFailed", "Cannot read the workspace VolumeSnapshot; check API connectivity and snapshot read permissions", err)
		}
		if object.GetDeletionTimestamp() != nil {
			return nil, diagnostics.New("DependencyTerminating", "The workspace VolumeSnapshot is terminating; select an available snapshot", nil)
		}
		ready, _, err := unstructured.NestedBool(object.Object, "status", "readyToUse")
		if err != nil || !ready {
			return nil, diagnostics.New("SnapshotNotReady", "Waiting for the workspace VolumeSnapshot to become readyToUse; inspect its status and CSI snapshot controller", err)
		}
		size, found, err := unstructured.NestedString(object.Object, "status", "restoreSize")
		if err != nil {
			return nil, diagnostics.New("SnapshotInvalid", "The workspace VolumeSnapshot reports an invalid restoreSize", err)
		}
		if found {
			restoreSize, err := resource.ParseQuantity(size)
			if err != nil || restoreSize.Sign() < 0 {
				return nil, diagnostics.New("SnapshotInvalid", "The workspace VolumeSnapshot reports an invalid restoreSize", err)
			}
			requested := claim.Spec.Resources.Requests[corev1.ResourceStorage]
			if requested.Cmp(restoreSize) < 0 {
				return nil, diagnostics.New("SnapshotCapacityInsufficient", "The PVC storage request must be at least the snapshot restoreSize", nil)
			}
		}
		// UID prevents a deleted/recreated source from sharing an old warm pool.
		// Status-only snapshot updates do not invalidate restored workspaces.
		refs[name] = object.GetUID()
	}
	return refs, nil
}

// Only a completed execution may retain its initialization sources. Read them
// from the recorded Group-owned snapshot, not a new source or an external pool.
func (r *Reconciler) resolveSnapshotSources(ctx context.Context, workload *v1.AgentInstance, group *v1.AgentGroup, claims []corev1.PersistentVolumeClaimTemplate) (map[string]types.UID, error) {
	var retained map[string]types.UID
	needsSnapshot := false
	for _, claim := range claims {
		name, err := volumeSnapshotSource(claim.Spec, workload.Namespace)
		if err != nil {
			return nil, err
		}
		needsSnapshot = needsSnapshot || name != ""
	}
	if state := workload.Status.Provisioning; needsSnapshot && state != nil && state.CompletedAt != nil {
		template := sandbox.ExtensionObject("SandboxTemplate")
		if err := r.referenceReader().Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: state.TemplateName}, template); err != nil {
			return nil, diagnostics.ReferenceError("SandboxTemplate", workload.Namespace, state.TemplateName, err)
		}
		if err := controller.CheckOwner(template, group); err != nil {
			return nil, err
		}
		if template.GetAnnotations()["arally.io/source-template"] == workload.Spec.TemplateSource() && template.GetAnnotations()["arally.io/configuration-revision"] == state.ConfigurationRevision {
			var err error
			retained, err = allocation.SnapshotSources(template)
			if err != nil {
				return nil, err
			}
		}
	}
	return snapshotReferences(ctx, r.referenceReader(), workload.Namespace, claims, retained)
}

func volumeSnapshotSource(spec corev1.PersistentVolumeClaimSpec, namespace string) (string, error) {
	group := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	local, ref := spec.DataSource, spec.DataSourceRef
	if local != nil && ref != nil && (local.Name != ref.Name || local.Kind != ref.Kind || group(local.APIGroup) != group(ref.APIGroup)) {
		return "", diagnostics.New("InvalidConfiguration", "PVC dataSource and dataSourceRef must identify the same source", nil)
	}
	name, kind, apiGroup := "", "", ""
	if local != nil {
		name, kind, apiGroup = local.Name, local.Kind, group(local.APIGroup)
	}
	if ref != nil {
		name, kind, apiGroup = ref.Name, ref.Kind, group(ref.APIGroup)
	}
	if kind != "VolumeSnapshot" || apiGroup != snapshotAPIGroup {
		return "", nil // Other native volume populators remain Kubernetes' responsibility.
	}
	if name == "" {
		return "", diagnostics.New("InvalidConfiguration", "VolumeSnapshot sources require a name and apiGroup: snapshot.storage.k8s.io", nil)
	}
	if ref != nil && ref.Namespace != nil && *ref.Namespace != "" && *ref.Namespace != namespace {
		return "", diagnostics.New("SnapshotNamespaceUnsupported", "Workspace VolumeSnapshot sources must be in the AgentInstance namespace", nil)
	}
	return name, nil
}
