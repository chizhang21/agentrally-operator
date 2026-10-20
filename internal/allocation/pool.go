package allocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

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
}

// Selection identifies an immutable allocation configuration. Only Select produces
// selections; AgentClaim persists the names before any capacity is created.
type Selection struct {
	PoolName, TemplateName      string
	group                       *v1.AgentGroup
	pool                        *unstructured.Unstructured
	exists                      bool
	source, namespace, revision string
	service                     bool
}

func (m *Manager) reader() client.Reader {
	if m.Reader != nil {
		return m.Reader
	}
	return m.Client
}

// ManagedLabel identifies Arally-generated group-owned allocation resources.
const ManagedLabel = "arally.io/managed-allocation"

// Select reads the requested or generated pool without writing Claim status or resources.
// The caller must persist its returned names before calling Ensure.
func (m *Manager) Select(ctx context.Context, w *v1.AgentClaim, service bool, revision string) (*Selection, error) {
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
	if getErr == nil {
		managed := pool.GetLabels()[ManagedLabel] == "true"
		explicit := w.Spec.Provisioning != nil && w.Spec.Provisioning.WarmPoolRef != nil
		if (managed && !managedByGroup(pool, group)) || (!explicit && !managed) {
			return nil, poolDiagnostic("PoolOwnershipConflict", "The generated pool name is controlled by another owner")
		}
		if managed && pool.GetAnnotations()["arally.io/source-template"] != w.Spec.TemplateSource() {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The managed pool was generated for another source template")
		}
		name, err := sandbox.PoolTemplateName(pool)
		if !managed && name != w.Spec.TemplateSource() {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The named external pool must reference AgentClaim.templateRef")
		}
		if err != nil || name == "" {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The selected pool has no valid template reference")
		}
		if w.Status.Provisioning != nil && w.Status.Provisioning.TemplateName != "" && w.Status.Provisioning.TemplateName != name {
			return nil, poolDiagnostic("PoolTemplateMismatch", "The selected pool changed its template; restart the allocation")
		}
		templateName = name
	}

	return &Selection{PoolName: poolName, TemplateName: templateName,
		group: group, pool: pool, exists: getErr == nil,
		source: w.Spec.TemplateSource(), namespace: w.Namespace,
		revision: revision, service: service}, nil
}

// Ensure creates group-owned immutable templates and capacity after Claim selection
// is persisted. Existing external pools and externally scaled replicas are preserved.
func (m *Manager) Ensure(ctx context.Context, selection *Selection, pod *corev1.PodTemplateSpec, claims []corev1.PersistentVolumeClaimTemplate, agentContainer string) error {
	pool := selection.pool.DeepCopy()
	group := selection.group
	templateName := selection.TemplateName
	if selection.exists && pool.GetLabels()[ManagedLabel] != "true" {
		return nil
	}

	// Never mutate a caller's template; instantiate the effective configuration
	// before warming. The marker records creation inputs without secret values.
	effective := pod.DeepCopy()
	if err := podconfig.Stamp(effective, agentContainer, selection.revision); err != nil {
		return err
	}
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(effective)
	if err != nil {
		return err
	}
	template := sandbox.ExtensionObject("SandboxTemplate")
	template.SetName(templateName)
	template.SetNamespace(selection.namespace)
	template.SetLabels(map[string]string{ManagedLabel: "true"})
	template.SetAnnotations(map[string]string{"arally.io/source-template": selection.source, "arally.io/configuration-revision": selection.revision})
	template.Object["spec"] = map[string]interface{}{
		"podTemplate": fields, "service": selection.service,
		"networkPolicyManagement": "Unmanaged",
		"envVarsInjectionPolicy":  "Disallowed", "volumeClaimTemplatesPolicy": "Disallowed",
	}
	if err := sandbox.SetTemplateVolumeClaims(template, claims); err != nil {
		return err
	}
	if err := m.createAllocationResource(ctx, template, group); err != nil {
		return err
	}
	// Prewarming starts real application processes. It is opt-in, regardless of
	// restart policy or storage; administrators can scale the native pool explicitly.
	pool.SetLabels(map[string]string{ManagedLabel: "true"})
	pool.SetAnnotations(map[string]string{"arally.io/source-template": selection.source})
	pool.Object["spec"] = map[string]interface{}{"sandboxTemplateRef": map[string]interface{}{"name": templateName}, "replicas": int64(0), "updateStrategy": map[string]interface{}{"type": "Recreate"}}
	if selection.exists {
		// Preserve externally scaled replicas on managed pools.
		return nil
	}
	return m.createAllocationResource(ctx, pool, group)
}

// Create-only snapshots: races may observe an identical resource, but never
// adopt a name collision or rewrite external/HPA-controlled pool settings.
func (m *Manager) createAllocationResource(ctx context.Context, desired *unstructured.Unstructured, group *v1.AgentGroup) error {
	if err := ctrl.SetControllerReference(group, desired, m.Scheme); err != nil {
		return err
	}
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
	}
	return nil
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
func PoolName(w *v1.AgentClaim) string {
	if w.Status.Provisioning != nil && w.Status.Provisioning.PoolName != "" {
		return w.Status.Provisioning.PoolName
	}
	if w.Spec.Provisioning != nil && w.Spec.Provisioning.WarmPoolRef != nil {
		return w.Spec.Provisioning.WarmPoolRef.Name
	}
	return ""
}

// TemplateName prefers the selected snapshot over the source template.
func TemplateName(w *v1.AgentClaim) string {
	if w.Status.Provisioning != nil && w.Status.Provisioning.TemplateName != "" {
		return w.Status.Provisioning.TemplateName
	}
	if w.Spec.TemplateRef != nil {
		return w.Spec.TemplateRef.Name
	}
	return ""
}

func poolDiagnostic(reason, message string) error {
	return diagnostics.New(reason, message, nil)
}
