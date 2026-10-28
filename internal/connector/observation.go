package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/podconfig"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

type ExecutionObservation struct {
	ID            string    `json:"id"`
	IdentityScope string    `json:"identityScope"`
	SandboxUID    types.UID `json:"sandboxUID"`
	PodUID        types.UID `json:"podUID"`
	ContainerName string    `json:"containerName"`
	ContainerID   string    `json:"containerID"`
	RestartCount  int32     `json:"restartCount"`
	StartedAt     string    `json:"startedAt"`
}

type InstanceObservation struct {
	InstanceID              string                `json:"instanceId"`
	InstanceUID             types.UID             `json:"instanceUID"`
	GroupUID                string                `json:"groupUID"`
	Definition              *v1.DefinitionBinding `json:"definition,omitempty"`
	Execution               *ExecutionObservation `json:"execution,omitempty"`
	ProcessIdentityVerified bool                  `json:"processIdentityVerified"`
	State                   string                `json:"state,omitempty"`
	StateEvidence           string                `json:"stateEvidence"`
	ExecutionRelease        string                `json:"executionRelease"`
	Reason                  string                `json:"reason"`
	ObservedAt              string                `json:"observedAt"`
	Invocation              invocationObservation `json:"invocation"`
}

type invocationObservation struct {
	DeclaredProtocol string `json:"declaredProtocol,omitempty"`
	Supported        bool   `json:"supported"`
	Reason           string `json:"reason"`
}

// observationReads detects changes to the objects used in an observation. This
// is not a transaction or a fencing mechanism, and includes observed absences.
type observationReads struct {
	client dynamic.Interface
	reads  []observedObject
}

type observedObject struct {
	resource        schema.GroupVersionResource
	namespace, name string
	uid             types.UID
	version         string
}

func (r *observationReads) get(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	object, err := r.client.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	observed := observedObject{resource: resource, namespace: namespace, name: name}
	if err == nil {
		observed.uid, observed.version = object.GetUID(), object.GetResourceVersion()
	}
	r.reads = append(r.reads, observed)
	return object, err
}

func (r *observationReads) verify(ctx context.Context) error {
	for _, observed := range r.reads {
		current, err := r.client.Resource(observed.resource).Namespace(observed.namespace).Get(ctx, observed.name, metav1.GetOptions{})
		if observed.uid == "" && apierrors.IsNotFound(err) {
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err != nil || current.GetUID() != observed.uid || current.GetResourceVersion() != observed.version {
			return protocolError(http.StatusConflict, "ObservationChanged", "Execution resources changed while observing the instance; read the current observation again")
		}
	}
	return nil
}

type observedExecution struct {
	Observation InstanceObservation
	Instance    *v1.AgentInstance
	Pod         *corev1.Pod
	Sandbox     *unstructured.Unstructured
}

func (e Executor) instanceObservation(ctx context.Context, req Request) (json.RawMessage, int, error) {
	observed, err := e.observeExecution(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	return mustJSON(observed.Observation), http.StatusOK, nil
}

func (e Executor) observeExecution(ctx context.Context, req Request) (*observedExecution, error) {
	if req.Method != http.MethodGet || req.Name == "" || len(req.Body) != 0 || req.Limit != 0 || req.Continue != "" {
		return nil, protocolError(http.StatusBadRequest, "BadRequest", "instance observations require a named GET without body or pagination")
	}
	native := req
	native.Resource = "agentinstances"
	if _, err := validateRequest(native); err != nil {
		return nil, err
	}
	if _, err := e.scopedNamespace(ctx, req, false); err != nil {
		return nil, err
	}
	reads := &observationReads{client: e.Client}
	object, err := reads.get(ctx, nativeResource("agentinstances"), req.Namespace, req.Name)
	if err != nil {
		return nil, err
	}
	if err := checkGroupReference(object, req.Scope.GroupName); err != nil {
		return nil, err
	}
	var instance v1.AgentInstance
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &instance); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	if instance.UID == "" {
		return nil, scopeForbidden()
	}
	observation := InstanceObservation{
		InstanceID: instance.Name, InstanceUID: instance.UID, GroupUID: req.Scope.GroupUID, Definition: instance.Status.Definition,
		StateEvidence: "KubernetesObservation", ExecutionRelease: "Unknown", Reason: "ExecutionUnavailable",
		Invocation: invocationObservation{Reason: "ProductionInvocationUnavailable"},
	}
	if binding := instance.Status.Definition; binding != nil {
		observation.Invocation.DeclaredProtocol = binding.InvocationProtocol
	}
	pod, current, err := observePod(ctx, reads, object, &instance)
	if err != nil {
		return nil, err
	}
	var sandboxUID types.UID
	if current != nil {
		sandboxUID = current.GetUID()
	}
	observeInstanceState(&observation, &instance, pod, sandboxUID)
	if err := reads.verify(ctx); err != nil {
		return nil, err
	}
	if _, err := e.scopedNamespace(ctx, req, false); err != nil {
		return nil, protocolError(http.StatusConflict, "ScopeChanged", "Instance scope could not be verified after observation")
	}
	observation.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return &observedExecution{Observation: observation, Instance: &instance, Pod: pod, Sandbox: current}, nil
}

func observePod(ctx context.Context, reads *observationReads, object *unstructured.Unstructured, instance *v1.AgentInstance) (*corev1.Pod, *unstructured.Unstructured, error) {
	provisioning := instance.Status.Provisioning
	if provisioning == nil || provisioning.ClaimName == "" {
		return nil, nil, nil
	}
	claim, err := reads.get(ctx, sandbox.ExtensionsGroupVersion.WithResource("sandboxclaims"), instance.Namespace, provisioning.ClaimName)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := requireOwner(claim, object, v1.GroupVersion.String(), "AgentInstance"); err != nil {
		return nil, nil, err
	}
	name, err := sandbox.ClaimSandboxName(claim)
	if err != nil || name == "" {
		return nil, nil, err
	}
	current, err := reads.get(ctx, sandbox.GroupVersion.WithResource("sandboxes"), instance.Namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := requireOwner(current, claim, sandbox.ExtensionsGroupVersion.String(), "SandboxClaim"); err != nil {
		return nil, nil, err
	}
	podName := current.GetAnnotations()["agents.x-k8s.io/pod-name"]
	if podName == "" {
		podName = current.GetName()
	}
	object, err = reads.get(ctx, schema.GroupVersionResource{Version: "v1", Resource: "pods"}, instance.Namespace, podName)
	if apierrors.IsNotFound(err) {
		return nil, current, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := requireOwner(object, current, sandbox.GroupVersion.String(), "Sandbox"); err != nil {
		return nil, nil, err
	}
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &pod); err != nil {
		return nil, nil, apierrors.NewInternalError(err)
	}
	return &pod, current, nil
}

func currentCondition(instance *v1.AgentInstance, kind string) bool {
	condition := meta.FindStatusCondition(instance.Status.Conditions, kind)
	return condition != nil && condition.ObservedGeneration == instance.Generation && condition.Status == metav1.ConditionTrue
}

func observeInstanceState(out *InstanceObservation, instance *v1.AgentInstance, pod *corev1.Pod, sandboxUID types.UID) {
	// Restarting is a persisted operation. Other spec edits must not make its
	// in-flight cleanup disappear merely by advancing metadata.generation.
	restarting := meta.IsStatusConditionTrue(instance.Status.Conditions, "Restarting")
	if instance.DeletionTimestamp != nil || restarting || instance.Spec.RestartToken != instance.Status.LastRestartToken {
		out.Reason = "Stopping"
		return
	}
	if instance.Status.Lifecycle != nil && instance.Status.Lifecycle.ExpiredAt != nil {
		out.Reason = "Stopping"
		if currentCondition(instance, "CleanupReady") && pod == nil {
			out.Reason = "ExecutionResourcesRemoved"
		}
		return
	}
	if instance.Spec.OperatingMode == sandbox.Suspended {
		out.Reason = "Stopping"
		if sandboxUID != "" && pod == nil && currentCondition(instance, "Suspended") {
			out.State, out.Reason = "Idle", "SuspendedResourcesObserved"
		}
		return
	}
	if pod == nil {
		return
	}
	if pod.DeletionTimestamp != nil {
		out.Reason = "Stopping"
		return
	}
	binding := instance.Status.Definition
	if binding == nil || binding.Revision == "" || binding.AgentContainer == "" {
		out.Reason = "BindingObservationUnavailable"
		return
	}
	if podconfig.AgentIndex(pod.Spec.Containers, binding.AgentContainer) < 0 {
		out.Reason = "AgentContainerUnavailable"
		return
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name != binding.AgentContainer {
			continue
		}
		if (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) && container.State.Terminated != nil {
			out.State, out.Reason = "Terminated", "ContainerTerminationObserved"
			return
		}
		if pod.UID == "" || container.ContainerID == "" || container.State.Running == nil || container.State.Running.StartedAt.IsZero() {
			break
		}
		execution := &ExecutionObservation{
			IdentityScope: "Container", SandboxUID: sandboxUID, PodUID: pod.UID, ContainerName: container.Name,
			ContainerID: container.ContainerID, RestartCount: container.RestartCount,
			StartedAt: container.State.Running.StartedAt.UTC().Format(time.RFC3339Nano),
		}
		identity := struct {
			InstanceUID types.UID
			Execution   *ExecutionObservation
		}{instance.UID, execution}
		digest := sha256.Sum256(mustJSON(identity))
		execution.ID = hex.EncodeToString(digest[:])
		out.Execution, out.Reason = execution, "ContainerObserved"
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && pod.Status.Phase == corev1.PodRunning {
				out.State = "Active"
			}
		}
		return
	}
	out.Reason = "ContainerObservationUnavailable"
}
