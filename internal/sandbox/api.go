// Package sandbox adapts the upstream Core Sandbox v1beta1 wire contract.
// The external controller owns its CRD and Pod lifecycle. This package contains
// only the fields AgentRally uses; it does not implement or fork the upstream controller.
package sandbox

import (
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Running   = "Running"
	Suspended = "Suspended"
)

var GroupVersion = schema.GroupVersion{Group: "agents.x-k8s.io", Version: "v1beta1"}

// Spec follows upstream api/v1beta1/sandbox_types.go. Core provisions the PVC
// templates copied by Extensions; AgentRally only changes the requested operating mode.
type Spec struct {
	PodTemplate          corev1.PodTemplateSpec                 `json:"podTemplate"`
	VolumeClaimTemplates []corev1.PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
	OperatingMode        string                                 `json:"operatingMode"`
	Service              bool                                   `json:"service"`
}
type Status struct {
	LabelSelector string             `json:"selector,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	Service       string             `json:"service,omitempty"`
	ServiceFQDN   string             `json:"serviceFQDN,omitempty"`
}
type Sandbox struct {
	Metadata metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec     Spec              `json:"spec"`
	Status   Status            `json:"status,omitempty"`
}

func Object() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(GroupVersion.WithKind("Sandbox"))
	return object
}
func Encode(value *Sandbox) (*unstructured.Unstructured, error) {
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
	if err != nil {
		return nil, fmt.Errorf("encode Sandbox: %w", err)
	}
	object := &unstructured.Unstructured{Object: fields}
	object.SetGroupVersionKind(GroupVersion.WithKind("Sandbox"))
	return object, nil
}
func Decode(object *unstructured.Unstructured) (*Sandbox, error) {
	value := &Sandbox{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, value); err != nil {
		return nil, fmt.Errorf("decode Sandbox: %w", err)
	}
	return value, nil
}

// AddToScheme supports Kubernetes fake clients and tools without depending on
// the upstream root module's newer Kubernetes/controller-runtime versions.
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypeWithName(GroupVersion.WithKind("Sandbox"), Object())
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(GroupVersion.WithKind("SandboxList"))
	scheme.AddKnownTypeWithName(GroupVersion.WithKind("SandboxList"), list)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return AddExtensionsToScheme(scheme)
}
