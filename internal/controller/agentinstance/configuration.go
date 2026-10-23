package agentinstance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"arally/operator/internal/allocation"
	"arally/operator/internal/controller"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/podconfig"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type configurationObservation struct {
	Condition       metav1.Condition
	AppliedRevision string
}

type configurationReference struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"`
	Missing         bool   `json:"missing,omitempty"`
	optional        bool
}

// configRevision records creation inputs without hashing credential contents.
// Resource versions deliberately make rotation visible even for unchanged references.
func configRevision(ctx context.Context, reader client.Reader, namespace string, config allocation.Configuration) (string, error) {
	template, agentContainer, claims := config.PodTemplate, config.AgentContainer, config.VolumeClaimTemplates
	if ctx == nil || reader == nil || namespace == "" || template == nil {
		return "", diagnostics.New("InvalidConfiguration", "configuration revision requires a namespace and Pod template", nil)
	}
	desired := template.DeepCopy()
	podconfig.StripMarkers(&desired.Spec)
	index := podconfig.AgentIndex(desired.Spec.Containers, agentContainer)
	if index < 0 {
		return "", diagnostics.New("InvalidConfiguration", "the selected Agent container does not exist", nil)
	}
	// The default and an explicit first-container selection share identity. Keep
	// their existing fingerprints; other selections stamp a different container.
	selected := ""
	if index > 0 {
		selected = desired.Spec.Containers[index].Name
	}
	refs := configurationReferences(desired)
	for i := range refs {
		ref := &refs[i]
		var object client.Object
		if ref.Kind == "Secret" {
			object = &corev1.Secret{}
		} else {
			object = &corev1.ConfigMap{}
		}
		err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, object)
		if err != nil {
			if apierrors.IsNotFound(err) && ref.optional {
				ref.Missing = true
				continue
			}
			reason, message := diagnostics.Classify(err)
			return "", diagnostics.New(reason, message, err)
		}
		if object.GetDeletionTimestamp() != nil {
			return "", diagnostics.New("DependencyTerminating", fmt.Sprintf("%s %s/%s is terminating; wait for deletion or choose another resource", ref.Kind, namespace, ref.Name), nil)
		}
		ref.ResourceVersion = object.GetResourceVersion()
	}
	snapshots, err := snapshotReferences(ctx, reader, namespace, claims, config.SnapshotSources)
	if err != nil {
		return "", err
	}
	for name, uid := range snapshots {
		refs = append(refs, configurationReference{Kind: "VolumeSnapshot", Name: name, UID: string(uid)})
	}
	if config.Workspace != nil {
		refs = append(refs, configurationReference{Kind: "PersistentVolumeClaim", Name: config.Workspace.ClaimName, UID: string(config.Workspace.UID)})
	}
	if config.Definition != nil {
		refs = append(refs, configurationReference{Kind: "AgentDefinition", Name: config.Definition.Name, UID: string(config.Definition.UID), ResourceVersion: config.Definition.Revision})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Kind+"/"+refs[i].Name < refs[j].Kind+"/"+refs[j].Name })
	encoded, err := json.Marshal(struct {
		Template             *corev1.PodTemplateSpec                `json:"template"`
		References           []configurationReference               `json:"references"`
		AgentContainer       string                                 `json:"agentContainer,omitempty"`
		VolumeClaimTemplates []corev1.PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
	}{desired, refs, selected, claims})
	if err != nil {
		return "", diagnostics.New("InvalidConfiguration", "cannot encode the Pod configuration", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func configurationReferences(template *corev1.PodTemplateSpec) []configurationReference {
	refs := map[string]configurationReference{}
	for _, ref := range podReferences(template) {
		key := ref.Kind + "/" + ref.Name
		optional := ref.Optional
		if old, found := refs[key]; found {
			optional = old.optional && optional
		}
		refs[key] = configurationReference{Kind: ref.Kind, Name: ref.Name, optional: optional}
	}
	result := make([]configurationReference, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind+"/"+result[i].Name < result[j].Kind+"/"+result[j].Name })
	return result
}

func validateConfigurationMarker(template *corev1.PodTemplateSpec) error {
	if template == nil {
		return diagnostics.New("InvalidConfiguration", "a Pod template is required", nil)
	}
	for _, containers := range [][]corev1.Container{template.Spec.Containers, template.Spec.InitContainers} {
		for _, container := range containers {
			for _, env := range container.Env {
				if env.Name == podconfig.RevisionEnv {
					return diagnostics.New("InvalidConfiguration", "ARALLY_CONFIGURATION_REVISION is reserved for operator configuration observation; remove it from the Pod template", nil)
				}
			}
		}
	}
	return nil
}

// observeConfiguration reads immutable Pod spec, never mutable Pod metadata.
// A marker proves creation inputs; it does not attest application hot reloads.
func observeConfiguration(template *corev1.PodTemplateSpec, pod *corev1.Pod, agentName, desiredRevision string) configurationObservation {
	result := configurationObservation{Condition: metav1.Condition{
		Type: controller.ConditionConfigurationApplied, Status: metav1.ConditionUnknown,
		Reason: "PodNotObserved", Message: "the owned execution Pod has not been observed",
	}}
	if pod == nil {
		return result
	}
	if template == nil || podconfig.AgentIndex(template.Spec.Containers, agentName) < 0 {
		result.Condition.Reason = "ConfigurationUnverified"
		result.Condition.Message = "the desired Agent container is unavailable for configuration observation"
		return result
	}
	index := podconfig.AgentIndex(template.Spec.Containers, agentName)
	actualIndex := podconfig.AgentIndex(pod.Spec.Containers, template.Spec.Containers[index].Name)
	markerCount := 0
	if actualIndex >= 0 {
		for _, env := range pod.Spec.Containers[actualIndex].Env {
			if env.Name == podconfig.RevisionEnv {
				markerCount++
				if env.ValueFrom == nil {
					result.AppliedRevision = env.Value
				}
			}
		}
	}
	if !podconfig.MatchesAdmitted(&template.Spec, &pod.Spec) || markerCount > 1 || (markerCount == 1 && (result.AppliedRevision != desiredRevision || desiredRevision == "")) {
		result.Condition.Status = metav1.ConditionFalse
		result.Condition.Reason = "RestartRequired"
		result.Condition.Message = "the running Pod differs from the requested configuration; change spec.restartToken to replace the execution"
		return result
	}
	if markerCount == 0 || result.AppliedRevision == "" {
		result.Condition.Reason = "ConfigurationUnverified"
		result.Condition.Message = "native Pod configuration matches, but this Pod has no creation revision; referenced credential versions cannot be verified"
		return result
	}
	result.Condition.Status = metav1.ConditionTrue
	result.Condition.Reason = "ConfigurationCurrent"
	result.Condition.Message = "the owned Pod matches the requested configuration and its creation revision"
	return result
}
