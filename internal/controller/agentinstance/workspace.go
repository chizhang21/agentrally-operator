package agentinstance

import (
	"context"
	"fmt"
	"path"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func applyWorkspace(ctx context.Context, reader client.Reader, workload *v1.AgentInstance, resolved *resolvedTemplate, container *corev1.Container) error {
	workspace := workload.Spec.Workspace
	if workspace == nil {
		return nil
	}
	if !path.IsAbs(workspace.MountPath) || workspace.MountPath == "/" || path.Clean(workspace.MountPath) != workspace.MountPath {
		return diagnostics.New("InvalidConfiguration", "workspace requires a clean absolute mountPath below /", nil)
	}
	if (workspace.VolumeClaimTemplate != nil) == (workspace.ExistingClaimRef != nil) {
		return diagnostics.New("InvalidConfiguration", "workspace requires exactly one of volumeClaimTemplate or existingClaimRef", nil)
	}
	for _, volume := range resolved.PodTemplate.Spec.Volumes {
		if volume.Name == "arally-workspace" {
			return diagnostics.New("InvalidConfiguration", "workspace volume name arally-workspace is already used in SandboxTemplate", nil)
		}
	}
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == workspace.MountPath {
			return diagnostics.New("InvalidConfiguration", "workspace mountPath is already used in SandboxTemplate", nil)
		}
	}
	if ref := workspace.ExistingClaimRef; ref != nil {
		if len(validation.IsDNS1123Subdomain(ref.Name)) != 0 {
			return diagnostics.New("InvalidConfiguration", "workspace.existingClaimRef requires a PVC name in the instance namespace", nil)
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: ref.Name}, pvc); err != nil {
			return diagnostics.ReferenceError("PersistentVolumeClaim", workload.Namespace, ref.Name, err)
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
		resolved.PodTemplate.Spec.Volumes = append(resolved.PodTemplate.Spec.Volumes, corev1.Volume{Name: "arally-workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name, ReadOnly: workspace.ReadOnly}}})
	} else {
		if mode := workspace.VolumeClaimTemplate.VolumeMode; mode != nil && *mode != corev1.PersistentVolumeFilesystem {
			return diagnostics.New("InvalidConfiguration", "workspace requires a Filesystem volume", nil)
		}
		resolved.VolumeClaimTemplates = append(resolved.VolumeClaimTemplates, corev1.PersistentVolumeClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "arally-workspace"},
			Spec:       *workspace.VolumeClaimTemplate.DeepCopy(),
		})
	}
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "arally-workspace", MountPath: workspace.MountPath, ReadOnly: workspace.ReadOnly})
	setEnv(container, corev1.EnvVar{Name: "ARALLY_WORKSPACE_PATH", Value: workspace.MountPath})
	return nil
}

func (r *Reconciler) bindWorkspace(ctx context.Context, instance *v1.AgentInstance, binding *v1.WorkspaceBinding) error {
	if current := instance.Status.Workspace; current != nil {
		if binding == nil || *current != *binding {
			return diagnostics.New("WorkspaceBindingChanged", "The external workspace identity changed; create a new instance to bind different storage", nil)
		}
		return nil
	}
	if binding == nil {
		return nil
	}
	base := instance.DeepCopy()
	instance.Status.Workspace = binding.DeepCopy()
	return r.patchStatus(ctx, instance, base)
}

// Validate the shared storage blueprint before starting any warm instances.
// Core still owns provisioning, binding and all PVC writes.
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
