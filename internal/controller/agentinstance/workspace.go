package agentinstance

import (
	"context"
	"fmt"
	"path"
	"strings"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/executionconfig"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"
	managedworkspace "agentrally/operator/internal/workspace"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// preparePersistentWorkspace persists the binding before any configuration can
// consume it. Suspension must not depend on provisioning new storage.
func (r *Reconciler) preparePersistentWorkspace(ctx context.Context, instance *v1.AgentInstance, group *v1.AgentGroup, mode string) error {
	if mode == sandbox.Suspended || instance.Spec.Workspace == nil || instance.Spec.Workspace.Persistence == nil {
		return nil
	}
	revision, err := loadDefinitionRevision(ctx, r.referenceReader(), instance)
	if err != nil {
		return err
	}
	resolved, err := executionconfig.ResolveTemplate(instance.Spec.DefinitionRef.Name, instance.Spec.Workspace, revision)
	if err != nil {
		return err
	}
	if err := validateWorkspaceMount(instance.Spec.Workspace, resolved); err != nil {
		return err
	}
	if err := r.bindDefinition(ctx, instance, resolved.Definition); err != nil {
		return err
	}
	return managedworkspace.Ensure(ctx, r.directClient(), r.referenceReader(), r.Scheme, instance, group, revision)
}

func validateWorkspaceMount(workspace *v1.WorkspaceMount, resolved *allocation.Configuration) error {
	if !path.IsAbs(workspace.MountPath) || workspace.MountPath == "/" || path.Clean(workspace.MountPath) != workspace.MountPath {
		return diagnostics.New("InvalidConfiguration", "workspace requires a clean absolute mountPath below /", nil)
	}
	if (workspace.VolumeClaimTemplate != nil) == (workspace.ExistingClaimRef != nil) {
		return diagnostics.New("InvalidConfiguration", "workspace requires exactly one of volumeClaimTemplate or existingClaimRef", nil)
	}
	for _, volume := range resolved.PodTemplate.Spec.Volumes {
		if volume.Name == "agentrally-workspace" {
			return diagnostics.New("InvalidConfiguration", "workspace volume name agentrally-workspace is already used in SandboxTemplate", nil)
		}
	}
	for _, claim := range resolved.VolumeClaimTemplates {
		if claim.Name == "agentrally-workspace" {
			return diagnostics.New("InvalidConfiguration", "workspace volume name agentrally-workspace is already used in volumeClaimTemplates", nil)
		}
	}
	index := podconfig.AgentIndex(resolved.PodTemplate.Spec.Containers, resolved.AgentContainer)
	if index < 0 {
		return diagnostics.New("InvalidConfiguration", "Workspace Agent container is missing", nil)
	}
	for _, mount := range resolved.PodTemplate.Spec.Containers[index].VolumeMounts {
		if mount.MountPath == workspace.MountPath {
			return diagnostics.New("InvalidConfiguration", "workspace mountPath is already used in SandboxTemplate", nil)
		}
	}
	return nil
}

func applyWorkspace(ctx context.Context, reader client.Reader, workload *v1.AgentInstance, resolved *allocation.Configuration, container *corev1.Container) error {
	workspace := workload.Spec.Workspace
	if workspace == nil {
		return nil
	}
	if err := validateWorkspaceMount(workspace, resolved); err != nil {
		return err
	}
	if workspace.Persistence != nil {
		binding, err := managedworkspace.Binding(ctx, reader, workload)
		if err != nil {
			return err
		}
		resolved.Workspace = binding
		resolved.PodTemplate.Spec.Volumes = append(resolved.PodTemplate.Spec.Volumes, corev1.Volume{Name: "agentrally-workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: binding.ClaimName, ReadOnly: workspace.ReadOnly}}})
	} else if ref := workspace.ExistingClaimRef; ref != nil {
		if len(validation.IsDNS1123Subdomain(ref.Name)) != 0 {
			return diagnostics.New("InvalidConfiguration", "workspace.existingClaimRef requires a PVC name in the instance namespace", nil)
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: ref.Name}, pvc); err != nil {
			return diagnostics.ReferenceError("PersistentVolumeClaim", workload.Namespace, ref.Name, err)
		}
		if pvc.Annotations[managedworkspace.UIDAnnotation] != "" {
			return diagnostics.New("WorkspaceOwnershipConflict", "Platform workspaces cannot be attached as external PVCs", nil)
		}
		if pvc.DeletionTimestamp != nil || pvc.Status.Phase == corev1.ClaimLost {
			return diagnostics.New("WorkspaceUnavailable", "The external workspace PVC is terminating or lost", nil)
		}
		for _, owner := range pvc.OwnerReferences {
			if owner.Kind == "Sandbox" && strings.HasPrefix(owner.APIVersion, sandbox.GroupVersion.Group+"/") {
				return diagnostics.New("WorkspaceOwnershipConflict", "Execution-owned PVCs cannot be used as retained external workspaces", nil)
			}
		}
		if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
			return diagnostics.New("InvalidConfiguration", "workspace requires a Filesystem PVC", nil)
		}
		resolved.Workspace = &v1.WorkspaceBinding{ClaimName: pvc.Name, UID: pvc.UID}
		resolved.PodTemplate.Spec.Volumes = append(resolved.PodTemplate.Spec.Volumes, corev1.Volume{Name: "agentrally-workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name, ReadOnly: workspace.ReadOnly}}})
	} else {
		if mode := workspace.VolumeClaimTemplate.VolumeMode; mode != nil && *mode != corev1.PersistentVolumeFilesystem {
			return diagnostics.New("InvalidConfiguration", "workspace requires a Filesystem volume", nil)
		}
		resolved.VolumeClaimTemplates = append(resolved.VolumeClaimTemplates, corev1.PersistentVolumeClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "agentrally-workspace"},
			Spec:       *workspace.VolumeClaimTemplate.DeepCopy(),
		})
	}
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "agentrally-workspace", MountPath: workspace.MountPath, ReadOnly: workspace.ReadOnly})
	podconfig.SetEnv(container, corev1.EnvVar{Name: "AGENTRALLY_WORKSPACE_PATH", Value: workspace.MountPath})
	return nil
}

func (r *Reconciler) bindWorkspace(ctx context.Context, instance *v1.AgentInstance, binding *v1.WorkspaceBinding) error {
	if current := instance.Status.Workspace; current != nil {
		if binding == nil || *current != *binding && !(current.UID == "" && current.WorkspaceUID != "" && current.WorkspaceUID == binding.WorkspaceUID && current.ClaimName == binding.ClaimName) {
			return diagnostics.New("WorkspaceBindingChanged", "The external workspace identity changed; create a new instance to bind different storage", nil)
		}
	}
	if binding == nil {
		return nil
	}
	if instance.Spec.Workspace != nil && instance.Spec.Workspace.Exclusive {
		if err := r.reserveWorkspace(ctx, instance, binding); err != nil {
			return err
		}
	}
	if instance.Status.Workspace == nil || instance.Status.Workspace.UID == "" {
		base := instance.DeepCopy()
		instance.Status.Workspace = binding.DeepCopy()
		if err := r.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	reservation, err := r.readWorkspaceReservation(ctx, instance, binding)
	if err == nil && instance.Spec.Workspace != nil && instance.Spec.Workspace.Exclusive && (reservation == nil || reservation.Annotations[workspaceReservationReady] != "true") {
		return diagnostics.New("WorkspaceReservationChanged", "Exclusive workspace reservation is no longer present", nil)
	}
	return err
}

// Validate the shared storage blueprint before starting any warm instances.
// Core owns provisioning and binding for these execution-scoped templates.
func validateVolumeClaimTemplates(claims []corev1.PersistentVolumeClaimTemplate, pod *corev1.PodSpec) error {
	names := map[string]bool{}
	for _, volume := range pod.Volumes {
		names[volume.Name] = true
	}
	for _, claim := range claims {
		invalid := func(message string) error {
			return diagnostics.New("InvalidConfiguration", fmt.Sprintf("volumeClaimTemplate %q: %s", claim.Name, message), nil)
		}
		if len(validation.IsDNS1123Label(claim.Name)) != 0 || names[claim.Name] {
			return invalid("name must be a unique DNS label and must not conflict with a Pod volume")
		}
		names[claim.Name] = true
		if claim.Spec.VolumeName != "" {
			return invalid("volumeName cannot bind multiple instances to the same preselected PV")
		}
		size := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if size.Sign() <= 0 {
			return invalid("resources.requests.storage must be positive")
		}
		if len(claim.Spec.AccessModes) == 0 {
			return invalid("accessModes must select at least one native PVC access mode")
		}
		modes := map[corev1.PersistentVolumeAccessMode]bool{}
		for _, mode := range claim.Spec.AccessModes {
			switch mode {
			case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadOnlyMany, corev1.ReadWriteOncePod:
			default:
				return invalid("accessModes contains an unsupported mode")
			}
			if modes[mode] {
				return invalid("accessModes contains duplicate modes")
			}
			modes[mode] = true
		}
		if mode := claim.Spec.VolumeMode; mode != nil && *mode != corev1.PersistentVolumeFilesystem && *mode != corev1.PersistentVolumeBlock {
			return invalid("volumeMode must be Filesystem or Block")
		}
		block := claim.Spec.VolumeMode != nil && *claim.Spec.VolumeMode == corev1.PersistentVolumeBlock
		for _, containers := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
			for _, container := range containers {
				for _, mount := range container.VolumeMounts {
					if mount.Name == claim.Name && block {
						return invalid("Block volumes require volumeDevices instead of volumeMounts")
					}
				}
				for _, device := range container.VolumeDevices {
					if device.Name == claim.Name && !block {
						return invalid("Filesystem volumes require volumeMounts instead of volumeDevices")
					}
				}
			}
		}
	}
	return nil
}
