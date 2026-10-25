package allocation

import (
	"encoding/json"

	v1 "agentrally/operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const snapshotSourcesAnnotation = "arally.io/volume-snapshot-sources"

// Configuration is the resolved creation input shared by revision calculation,
// immutable pool snapshots and assignment validation. Callers copy it before
// stamping or projecting native Pod fields.
type Configuration struct {
	PodTemplate          *corev1.PodTemplateSpec
	VolumeClaimTemplates []corev1.PersistentVolumeClaimTemplate
	Service              bool
	AgentContainer       string
	// SnapshotSources records initialization identity within the Claim namespace.
	// Completed allocations reuse it without requiring the source to still exist.
	SnapshotSources map[string]types.UID
	Workspace       *v1.WorkspaceBinding
	Definition      *v1.DefinitionBinding
}

// SnapshotSources reads initialization identities from a verified allocation template.
func SnapshotSources(template *unstructured.Unstructured) (map[string]types.UID, error) {
	data := template.GetAnnotations()[snapshotSourcesAnnotation]
	if data == "" {
		return nil, nil
	}
	var sources map[string]types.UID
	if err := json.Unmarshal([]byte(data), &sources); err != nil {
		return nil, poolDiagnostic("PoolTemplateMismatch", "The allocation template has invalid VolumeSnapshot identities")
	}
	for name, uid := range sources {
		if name == "" || uid == "" {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The allocation template has incomplete VolumeSnapshot identities")
		}
	}
	return sources, nil
}
