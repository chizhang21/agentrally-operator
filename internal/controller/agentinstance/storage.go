package agentinstance

import (
	"context"
	"reflect"
	"sort"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Definition storage defaults provision new instances; accepted storage is not an
// application upgrade input. Explicit instance overrides must still match it.
func (r *Reconciler) retainAllocatedStorage(ctx context.Context, w *v1.AgentInstance, config *executionConfiguration) error {
	if w.Status.Provisioning == nil || w.Status.Provisioning.CompletedAt == nil {
		return nil
	}
	object, err := r.existingExecution(ctx, w)
	if err != nil {
		return err
	}
	current, err := sandbox.Decode(object)
	if err != nil {
		return err
	}
	incompatible := func() error {
		return diagnostics.New("StorageChangeRejected", "An application update must retain existing volume identities, modes and mount paths; provision a separate instance for storage changes", nil)
	}
	oldClaims := map[string]corev1.PersistentVolumeClaimTemplate{}
	for _, claim := range current.Spec.VolumeClaimTemplates {
		oldClaims[claim.Name] = claim
	}
	if len(oldClaims) != len(config.VolumeClaimTemplates) {
		return incompatible()
	}
	for _, claim := range config.VolumeClaimTemplates {
		old, exists := oldClaims[claim.Name]
		if !exists || volumeMode(old.Spec) != volumeMode(claim.Spec) {
			return incompatible()
		}
		if claim.Name == "agentrally-workspace" && w.Spec.Workspace != nil && w.Spec.Workspace.VolumeClaimTemplate != nil &&
			!equality.Semantic.DeepEqual(old.Spec, *w.Spec.Workspace.VolumeClaimTemplate) {
			return incompatible()
		}
	}
	before, after := persistentReferences(&current.Spec.PodTemplate.Spec), persistentReferences(&config.PodTemplate.Spec)
	if !reflect.DeepEqual(before, after) {
		return incompatible()
	}
	persistent := map[string]bool{}
	for name := range oldClaims {
		persistent[name] = true
	}
	for name := range before {
		persistent[name] = true
	}
	if !reflect.DeepEqual(persistentMounts(&current.Spec.PodTemplate.Spec, persistent), persistentMounts(&config.PodTemplate.Spec, persistent)) {
		return incompatible()
	}
	config.VolumeClaimTemplates = current.Spec.VolumeClaimTemplates
	return nil
}

func volumeMode(spec corev1.PersistentVolumeClaimSpec) corev1.PersistentVolumeMode {
	if spec.VolumeMode == nil {
		return corev1.PersistentVolumeFilesystem
	}
	return *spec.VolumeMode
}

func persistentReferences(pod *corev1.PodSpec) map[string]corev1.PersistentVolumeClaimVolumeSource {
	refs := map[string]corev1.PersistentVolumeClaimVolumeSource{}
	for _, volume := range pod.Volumes {
		if volume.PersistentVolumeClaim != nil {
			refs[volume.Name] = *volume.PersistentVolumeClaim
		}
	}
	return refs
}

type persistentContainerMounts struct {
	Mounts  []corev1.VolumeMount
	Devices []corev1.VolumeDevice
}

func persistentMounts(pod *corev1.PodSpec, names map[string]bool) map[string]persistentContainerMounts {
	mounts := map[string]persistentContainerMounts{}
	for _, group := range []struct {
		prefix     string
		containers []corev1.Container
	}{{"init/", pod.InitContainers}, {"", pod.Containers}} {
		for _, container := range group.containers {
			entry := persistentContainerMounts{}
			for _, mount := range container.VolumeMounts {
				if names[mount.Name] {
					entry.Mounts = append(entry.Mounts, mount)
				}
			}
			for _, device := range container.VolumeDevices {
				if names[device.Name] {
					entry.Devices = append(entry.Devices, device)
				}
			}
			if len(entry.Mounts)+len(entry.Devices) != 0 {
				sort.Slice(entry.Mounts, func(i, j int) bool {
					return entry.Mounts[i].Name+entry.Mounts[i].MountPath < entry.Mounts[j].Name+entry.Mounts[j].MountPath
				})
				sort.Slice(entry.Devices, func(i, j int) bool { return entry.Devices[i].Name < entry.Devices[j].Name })
				mounts[group.prefix+container.Name] = entry
			}
		}
	}
	return mounts
}

// Persist PVC identities as they appear. Once pinned, absence or replacement is
// an error; a same-name empty volume must never be reported as restored data.
func (r *Reconciler) observeVolumeIdentities(ctx context.Context, w *v1.AgentInstance, object *unstructured.Unstructured, current *sandbox.Sandbox, requireAll bool) error {
	names := sandbox.VolumeClaimNames(object.GetName(), current.Spec.VolumeClaimTemplates)
	owned := map[string]bool{}
	for name := range names {
		owned[name] = true
	}
	if names == nil {
		names = map[string]string{}
	}
	for volume, ref := range persistentReferences(&current.Spec.PodTemplate.Spec) {
		names[volume] = ref.ClaimName
	}
	identities := map[string]types.UID{}
	for name, uid := range w.Status.VolumeClaimUIDs {
		identities[name] = uid
	}
	for volume, name := range names {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.referenceReader().Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, pvc); err != nil {
			if apierrors.IsNotFound(err) && identities[volume] == "" && !requireAll {
				continue
			}
			return diagnostics.ReferenceError("PersistentVolumeClaim", w.Namespace, name, err)
		}
		if pvc.DeletionTimestamp != nil || pvc.Status.Phase == corev1.ClaimLost {
			return diagnostics.New("WorkspaceUnavailable", "An allocated PVC is terminating or lost", nil)
		}
		if owned[volume] {
			ref := metav1.GetControllerOf(pvc)
			if ref == nil || ref.UID != object.GetUID() || ref.Name != object.GetName() || ref.Kind != "Sandbox" || ref.APIVersion != sandbox.GroupVersion.String() {
				return diagnostics.New("WorkspaceOwnershipConflict", "The allocated PVC is not controlled by the original Sandbox", nil)
			}
		}
		if uid := identities[volume]; uid != "" && uid != pvc.UID {
			return diagnostics.New("WorkspaceIdentityChanged", "An allocated PVC was replaced; refusing to treat a new volume as the existing workspace", nil)
		}
		identities[volume] = pvc.UID
	}
	if len(identities) == 0 {
		return nil
	}
	base := w.DeepCopy()
	w.Status.VolumeClaimUIDs = identities
	return r.patchStatus(ctx, w, base)
}
