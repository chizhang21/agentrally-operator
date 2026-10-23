package allocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/podconfig"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Manager reconciles only Arally-generated allocation resources. Core and Extensions
// retain ownership of execution Pods, Sandbox allocation and pool refill.
type Manager struct {
	Client client.Client
	Reader client.Reader
	Scheme *runtime.Scheme
	Now    func() time.Time
}

// Selection identifies an immutable allocation configuration. Only Select produces
// selections; AgentInstance persists the names before any capacity is created.
type Selection struct {
	PoolName, TemplateName      string
	group                       *v1.AgentGroup
	pool                        *unstructured.Unstructured
	exists                      bool
	source, namespace, revision string
	allocationToken             string
}

func (m *Manager) reader() client.Reader {
	if m.Reader != nil {
		return m.Reader
	}
	return m.Client
}

// ManagedLabel identifies Arally-generated group-owned allocation resources.
const ManagedLabel = "arally.io/managed-allocation"
const lastAllocationAnnotation = "arally.io/last-allocation"

// Select reads the requested or generated pool without writing Claim status or resources.
// The caller must persist its returned names before calling Ensure.
func (m *Manager) Select(ctx context.Context, w *v1.AgentInstance, config Configuration, revision string) (*Selection, error) {
	service := config.Service
	if state := w.Status.Provisioning; state != nil && state.ConfigurationRevision != "" && state.ConfigurationRevision != revision {
		return nil, poolDiagnostic("PoolTemplateMismatch", "Configuration changed during allocation; change restartToken to use the new configuration")
	}
	group := &v1.AgentGroup{}
	if err := m.reader().Get(ctx, client.ObjectKey{Name: w.Spec.AgentGroupRef.Name}, group); err != nil {
		return nil, diagnostics.ReferenceError("AgentGroup", "", group.Name, err)
	}
	if group.DeletionTimestamp != nil || group.NamespaceName() != w.Namespace {
		return nil, poolDiagnostic("GroupNotReady", "The pool requires an active AgentGroup in the same namespace")
	}
	key, err := json.Marshal(struct {
		Group, Source, Revision string
		Service                 bool
	}{string(group.UID), w.Spec.TemplateSource(), revision, service})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	suffix := hex.EncodeToString(sum[:16])
	poolName, templateName := "arally-pool-"+suffix, "arally-template-"+suffix
	if selected := PoolName(w); selected != "" {
		poolName = selected
	}
	if state := w.Status.Provisioning; state != nil && state.TemplateName != "" {
		templateName = state.TemplateName
	}
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName(poolName)
	pool.SetNamespace(w.Namespace)
	getErr := m.Client.Get(ctx, client.ObjectKeyFromObject(pool), pool)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return nil, diagnostics.New("PoolUnavailable", "Cannot read the pool; check Extensions APIs and permissions", getErr)
	}
	explicit := w.Spec.Provisioning != nil && w.Spec.Provisioning.WarmPoolRef != nil
	if apierrors.IsNotFound(getErr) && explicit {
		return nil, poolDiagnostic("PoolUnavailable", "The named external pool does not exist; create it before requesting allocation")
	}
	if getErr == nil {
		managed := pool.GetLabels()[ManagedLabel] == "true"
		if (managed && !managedByGroup(pool, group)) || (!explicit && !managed) {
			return nil, poolDiagnostic("PoolOwnershipConflict", "The generated pool name is controlled by another owner")
		}
		if managed && pool.GetAnnotations()["arally.io/source-template"] != w.Spec.TemplateSource() {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The managed pool was generated for another source template")
		}
		name, err := sandbox.PoolTemplateName(pool)
		if err != nil || name == "" {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The selected pool has no valid template reference")
		}
		if w.Status.Provisioning != nil && w.Status.Provisioning.TemplateName != "" && w.Status.Provisioning.TemplateName != name {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The selected pool changed its template; restart the allocation")
		}
		templateName = name
		if !managed {
			if config.Workspace != nil {
				replicas, _, err := unstructured.NestedInt64(pool.Object, "spec", "replicas")
				if err != nil || replicas != 0 {
					return nil, poolDiagnostic("WorkspacePrewarmingUnsupported", "External workspace pools must declare zero spare replicas")
				}
			}
			template := sandbox.ExtensionObject("SandboxTemplate")
			if err := m.reader().Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, template); err != nil {
				return nil, diagnostics.ReferenceError("SandboxTemplate", w.Namespace, name, err)
			}
			if err := ValidateSnapshot(template, group, w.Spec.TemplateSource(), revision, config); err != nil {
				return nil, err
			}
		}
	}

	allocationID := sha256.Sum256([]byte(string(w.UID) + "/" + w.Spec.RestartToken))
	return &Selection{PoolName: poolName, TemplateName: templateName,
		group: group, pool: pool, exists: getErr == nil,
		source: w.Spec.TemplateSource(), namespace: w.Namespace,
		revision: revision, allocationToken: hex.EncodeToString(allocationID[:])}, nil
}

// Ensure creates group-owned immutable templates and capacity after Claim selection
// is persisted. External pools stay untouched; declared policy controls managed replicas.
func (m *Manager) Ensure(ctx context.Context, selection *Selection, config Configuration) error {
	pool := selection.pool.DeepCopy()
	group := selection.group
	templateName := selection.TemplateName
	if selection.exists && pool.GetLabels()[ManagedLabel] != "true" {
		template := sandbox.ExtensionObject("SandboxTemplate")
		if err := m.reader().Get(ctx, client.ObjectKey{Namespace: selection.namespace, Name: templateName}, template); err != nil {
			return err
		}
		if err := ValidateSnapshot(template, group, selection.source, selection.revision, config); err != nil {
			return err
		}
		return m.protectAllocation(ctx, template, selection.allocationToken)
	}

	template, err := configurationSnapshot(config, selection.revision)
	if err != nil {
		return err
	}
	template.SetName(templateName)
	template.SetNamespace(selection.namespace)
	template.SetLabels(allocationLabels(config))
	annotations := template.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["arally.io/source-template"], annotations["arally.io/configuration-revision"] = selection.source, selection.revision
	template.SetAnnotations(annotations)
	if err := m.createAllocationResource(ctx, template, group, selection.allocationToken); err != nil {
		return err
	}
	// Prewarming starts real application processes. It is opt-in, regardless of
	// restart policy or storage; administrators can scale the native pool explicitly.
	pool.SetLabels(allocationLabels(config))
	pool.SetAnnotations(map[string]string{"arally.io/source-template": selection.source})
	if config.Workspace != nil {
		annotations := pool.GetAnnotations()
		annotations[existingWorkspaceAnnotation] = "true"
		pool.SetAnnotations(annotations)
	}
	pool.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": templateName}, "replicas": int64(0), "updateStrategy": map[string]interface{}{"type": "Recreate"}}
	if selection.exists {
		if err := m.protectAllocation(ctx, selection.pool, selection.allocationToken); err != nil {
			return err
		}
		return m.reconcilePoolPolicy(ctx, selection.pool, group, true)
	}
	if group.Spec.Prewarming != nil && config.Workspace == nil {
		pool.Object["spec"].(map[string]interface{})["replicas"] = int64(group.Spec.Prewarming.Replicas)
	}
	return m.createAllocationResource(ctx, pool, group, selection.allocationToken)
}

// ValidateSnapshot proves the complete creation input for an external or managed
// pool. A matching unmarked user template cannot prove credential versions.
func ValidateSnapshot(template *unstructured.Unstructured, group *v1.AgentGroup, source, revision string, config Configuration) error {
	if template.GetDeletionTimestamp() != nil {
		return poolDiagnostic("DependencyTerminating", "The selected configuration snapshot is terminating")
	}
	if group.UID == "" || !managedByGroup(template, group) || template.GetNamespace() != group.NamespaceName() || revision == "" {
		return poolDiagnostic("PoolConfigurationUnverified", "The pool must reference a current Arally-generated configuration snapshot owned by this Group; use a managed pool or its matching snapshot")
	}
	if template.GetAnnotations()["arally.io/source-template"] != source || template.GetAnnotations()["arally.io/configuration-revision"] != revision {
		return poolDiagnostic("PoolTemplateMismatch", "The pool snapshot was created for another source template or configuration revision")
	}
	sources, err := SnapshotSources(template)
	if err != nil {
		return err
	}
	if !maps.Equal(sources, config.SnapshotSources) {
		return poolDiagnostic("PoolTemplateMismatch", "The pool snapshot was created from different VolumeSnapshot identities")
	}
	wanted, err := configurationSnapshot(config, revision)
	if err != nil {
		return err
	}
	if !allocationSpecsMatch(template, wanted) {
		return poolDiagnostic("PoolTemplateMismatch", "The pool snapshot differs from the requested Pod, revision, service, storage or template policies")
	}
	return nil
}

func configurationSnapshot(config Configuration, revision string) (*unstructured.Unstructured, error) {
	pod := config.PodTemplate.DeepCopy()
	if err := podconfig.Stamp(pod, config.AgentContainer, revision); err != nil {
		return nil, err
	}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		return nil, err
	}
	template := sandbox.ExtensionObject("SandboxTemplate")
	if len(config.SnapshotSources) != 0 {
		data, err := json.Marshal(config.SnapshotSources)
		if err != nil {
			return nil, err
		}
		template.SetAnnotations(map[string]string{snapshotSourcesAnnotation: string(data)})
	}
	template.Object["spec"] = map[string]interface{}{
		"podTemplate": fields, "service": config.Service,
		"networkPolicyManagement": "Unmanaged",
		"envVarsInjectionPolicy":  "Disallowed", "volumeClaimTemplatesPolicy": "Disallowed",
	}
	if err := sandbox.SetTemplateVolumeClaims(template, config.VolumeClaimTemplates); err != nil {
		return nil, err
	}
	return template, nil
}

// Snapshot specs are immutable; metadata coordinates allocation and pruning.
// Never adopt a name collision or rewrite external/HPA-controlled pool settings.
func (m *Manager) createAllocationResource(ctx context.Context, desired *unstructured.Unstructured, group *v1.AgentGroup, token string) error {
	if err := ctrl.SetControllerReference(group, desired, m.Scheme); err != nil {
		return err
	}
	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[lastAllocationAnnotation] = token
	desired.SetAnnotations(annotations)
	if err := m.Client.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return diagnostics.New("PoolCreationFailed", "Cannot create the native allocation template or pool; check permissions and quota", err)
		}
		current := desired.DeepCopy()
		if err := m.Client.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
			return err
		}
		if !managedByGroup(current, group) {
			return poolDiagnostic("PoolOwnershipConflict", "The generated allocation resource name is already controlled by another owner")
		}
		if current.GetDeletionTimestamp() != nil {
			return poolDiagnostic("DependencyTerminating", "The managed allocation resource is terminating; wait for cleanup")
		}
		if !allocationSpecsMatch(current, desired) {
			return poolDiagnostic("PoolTemplateMismatch", "The managed allocation snapshot differs from its immutable configuration")
		}
		return m.protectAllocation(ctx, current, token)
	}
	return nil
}

// Selection is persisted before this version change; native Claim creation
// follows it. A pruner that read older references cannot delete this version.
// Retries of the same allocation do not write again.
func (m *Manager) protectAllocation(ctx context.Context, object *unstructured.Unstructured, token string) error {
	annotations := object.GetAnnotations()
	if annotations[lastAllocationAnnotation] == token {
		return nil
	}
	base := object.DeepCopy()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[lastAllocationAnnotation] = token
	object.SetAnnotations(annotations)
	return m.Client.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func managedByGroup(object *unstructured.Unstructured, group *v1.AgentGroup) bool {
	ref := metav1.GetControllerOf(object)
	return object.GetLabels()[ManagedLabel] == "true" && ref != nil && ref.UID == group.UID && ref.Name == group.Name && ref.Kind == "AgentGroup" && ref.APIVersion == v1.GroupVersion.String()
}

// CRD defaulting can materialize native Pod fields (for example port protocol).
// Compare typed configuration, retaining creation markers, without rewriting it.
func allocationSpecsMatch(current, desired *unstructured.Unstructured) bool {
	actual, _, err := unstructured.NestedMap(current.Object, "spec")
	if err != nil {
		return false
	}
	wanted, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return false
	}
	if desired.GetKind() == "SandboxTemplate" {
		actualClaims, actualErr := sandbox.TemplateVolumeClaims(current)
		wantedClaims, wantedErr := sandbox.TemplateVolumeClaims(desired)
		if actualErr != nil || wantedErr != nil || !equality.Semantic.DeepEqual(actualClaims, wantedClaims) {
			return false
		}
		delete(actual, "volumeClaimTemplates")
		delete(wanted, "volumeClaimTemplates")
		pods := []*corev1.PodTemplateSpec{{}, {}}
		for i, spec := range []map[string]interface{}{actual, wanted} {
			fields, ok := spec["podTemplate"].(map[string]interface{})
			if !ok || runtime.DefaultUnstructuredConverter.FromUnstructured(fields, pods[i]) != nil {
				return false
			}
			delete(spec, "podTemplate")
		}
		if !podconfig.MatchesSnapshot(pods[1], pods[0]) {
			return false
		}
	}
	return equality.Semantic.DeepEqual(actual, wanted)
}

// PoolName prefers persisted selection over an explicitly requested pool.
func PoolName(w *v1.AgentInstance) string {
	if w.Status.Provisioning != nil && w.Status.Provisioning.PoolName != "" {
		return w.Status.Provisioning.PoolName
	}
	if w.Spec.Provisioning != nil && w.Spec.Provisioning.WarmPoolRef != nil {
		return w.Spec.Provisioning.WarmPoolRef.Name
	}
	return ""
}

// TemplateName prefers the selected snapshot over the source template.
func TemplateName(w *v1.AgentInstance) string {
	if w.Status.Provisioning != nil && w.Status.Provisioning.TemplateName != "" {
		return w.Status.Provisioning.TemplateName
	}
	return ""
}

func poolDiagnostic(reason, message string) error {
	return diagnostics.New(reason, message, nil)
}

const definitionUIDLabel = "arally.io/definition-uid"

func allocationLabels(config Configuration) map[string]string {
	labels := map[string]string{ManagedLabel: "true"}
	if config.Definition != nil {
		labels[definitionUIDLabel] = string(config.Definition.UID)
	}
	return labels
}
