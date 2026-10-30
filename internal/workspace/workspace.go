// Package workspace owns initial persistent bindings. It never authorizes recovery or GC.
package workspace

import (
	"context"
	"crypto/sha256"
	"fmt"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const Finalizer = "arally.io/workspace-protection"
const UIDAnnotation = "arally.io/workspace-uid"
const GroupUIDLabel = "arally.io/group-uid"
const NamespaceUIDLabel = "arally.io/namespace-uid"

func Name(group types.UID, instance string) string {
	return fmt.Sprintf("agentrally-workspace-%x", sha256.Sum256([]byte(string(group)+"/"+instance)))[:52]
}
func unavailable() error {
	return diagnostics.New("WorkspaceIdentityChanged", "Persistent workspace identity is unavailable; recovery is not supported", nil)
}

// Ensure pins each successful creation before permitting a consumer. A provisional
// PVC owner is removed only after its UID is durable; no data promise precedes that point.
func Ensure(ctx context.Context, c client.Client, reader client.Reader, scheme *runtime.Scheme, instance *v1.AgentInstance, group *v1.AgentGroup, revision *definition.Revision) error {
	mount := instance.Spec.Workspace
	if mount == nil || mount.Persistence == nil {
		return nil
	}
	if !group.Spec.PersistentWorkspaces || group.DeletionTimestamp != nil || mount.VolumeClaimTemplate == nil || mount.ExistingClaimRef != nil {
		return diagnostics.New("PersistentWorkspaceUnavailable", "Persistent workspaces require an enabled, active Group and a private template", nil)
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: instance.Namespace}, ns); err != nil {
		return err
	}
	if err := controller.CheckGroupNamespace(ns, group); err != nil {
		return err
	}
	if ns.DeletionTimestamp != nil {
		return unavailable()
	}
	template := mount.VolumeClaimTemplate
	if template.VolumeName != "" || template.VolumeMode != nil && *template.VolumeMode != corev1.PersistentVolumeFilesystem || template.DataSource != nil || template.DataSourceRef != nil || len(template.AccessModes) == 0 || template.Resources.Requests.Storage().Sign() <= 0 {
		return diagnostics.New("InvalidConfiguration", "Persistent workspace requires a new Filesystem PVC with size and access modes", nil)
	}
	record := &v1.AgentWorkspace{ObjectMeta: metav1.ObjectMeta{Name: Name(group.UID, instance.Name)}}
	desired := v1.AgentWorkspaceSpec{GroupName: group.Name, GroupUID: group.UID, Namespace: ns.Name, NamespaceUID: ns.UID, InstanceName: instance.Name, InstanceUID: instance.UID,
		Definition: v1.DefinitionBinding{Name: instance.Spec.DefinitionRef.Name, UID: revision.DefinitionUID, Revision: revision.ID, TemplateName: revision.Template.GetName()}, RevisionUID: revision.Template.GetUID(), RetentionSeconds: mount.Persistence.RetentionSeconds, VolumeClaimTemplate: *template.DeepCopy()}
	err := reader.Get(ctx, client.ObjectKeyFromObject(record), record)
	if apierrors.IsNotFound(err) {
		if instance.Status.Workspace != nil {
			return unavailable()
		}
		record.Spec = desired
		record.Finalizers = []string{Finalizer}
		record.Labels = map[string]string{GroupUIDLabel: string(group.UID), NamespaceUIDLabel: string(ns.UID)}
		if err := c.Create(ctx, record); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if record.Spec.Definition.Revision != desired.Definition.Revision {
		return diagnostics.New("PersistentRevisionPinned", "The first workspace revision is pinned; persistent workspace upgrades are not yet supported", nil)
	}
	if record.DeletionTimestamp != nil || len(record.OwnerReferences) != 0 || !equality.Semantic.DeepEqual(record.Spec, desired) {
		return unavailable()
	}
	if instance.Status.Workspace == nil {
		base := instance.DeepCopy()
		instance.Status.Workspace = &v1.WorkspaceBinding{ClaimName: record.Name, WorkspaceName: record.Name, WorkspaceUID: record.UID}
		if err := c.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	} else if instance.Status.Workspace.WorkspaceUID != record.UID || instance.Status.Workspace.WorkspaceName != record.Name {
		return unavailable()
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: record.Name, Namespace: ns.Name}}
	err = reader.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)
	if apierrors.IsNotFound(err) {
		if record.Status.PVCUID != "" {
			return unavailable()
		}
		pvc.Spec = *template.DeepCopy()
		pvc.Annotations = map[string]string{UIDAnnotation: string(record.UID)}
		if err := ctrl.SetControllerReference(record, pvc, scheme); err != nil {
			return err
		}
		if err := c.Create(ctx, pvc); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if pvc.DeletionTimestamp != nil || pvc.Annotations[UIDAnnotation] != string(record.UID) {
		return unavailable()
	}
	if record.Status.PVCUID == "" {
		if err := controller.CheckOwner(pvc, record); err != nil {
			return err
		}
		base := record.DeepCopy()
		record.Status.PVCUID = pvc.UID
		record.Status.Phase = "Bound"
		if err := c.Status().Patch(ctx, record, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	if record.Status.PVCUID != pvc.UID {
		return unavailable()
	}
	if len(pvc.OwnerReferences) != 0 {
		if len(pvc.OwnerReferences) != 1 {
			return unavailable()
		}
		if err := controller.CheckOwner(pvc, record); err != nil {
			return err
		}
		base := pvc.DeepCopy()
		pvc.OwnerReferences = nil
		if err := c.Patch(ctx, pvc, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func Binding(ctx context.Context, reader client.Reader, instance *v1.AgentInstance) (*v1.WorkspaceBinding, error) {
	bound := instance.Status.Workspace
	if bound == nil || bound.WorkspaceUID == "" {
		return nil, unavailable()
	}
	record := &v1.AgentWorkspace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: bound.WorkspaceName}, record); err != nil {
		return nil, err
	}
	if record.UID != bound.WorkspaceUID || record.Spec.InstanceUID != instance.UID || record.Spec.Namespace != instance.Namespace || record.DeletionTimestamp != nil || record.Status.PVCUID == "" {
		return nil, unavailable()
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := reader.Get(ctx, client.ObjectKey{Name: bound.ClaimName, Namespace: instance.Namespace}, pvc); err != nil {
		return nil, err
	}
	if pvc.UID != record.Status.PVCUID || len(pvc.OwnerReferences) != 0 || pvc.DeletionTimestamp != nil || pvc.Status.Phase == corev1.ClaimLost || pvc.Annotations[UIDAnnotation] != string(record.UID) {
		return nil, unavailable()
	}
	result := bound.DeepCopy()
	result.UID = pvc.UID
	return result, nil
}

// ValidateVolumes also covers custom templates; naming a platform PVC directly
// must not bypass the Instance binding path or enable unapproved recovery.
func ValidateVolumes(ctx context.Context, reader client.Reader, instance *v1.AgentInstance, pod *corev1.PodTemplateSpec) error {
	for _, volume := range pod.Spec.Volumes {
		ref := volume.PersistentVolumeClaim
		if ref == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: ref.ClaimName}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			} // Existing storage validation reports missing references before allocation.
			return err
		}
		if pvc.Annotations[UIDAnnotation] == "" {
			continue
		}
		binding, err := Binding(ctx, reader, instance)
		if err != nil {
			return err
		}
		if binding.ClaimName != ref.ClaimName || binding.UID != pvc.UID {
			return unavailable()
		}
	}
	return nil
}
