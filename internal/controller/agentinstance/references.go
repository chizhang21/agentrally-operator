package agentinstance

import (
	"context"
	"fmt"

	"arally/operator/internal/diagnostics"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeReferenceValidator struct {
	ctx       context.Context
	reader    client.Reader
	namespace string
	secrets   map[string]*corev1.Secret
	configMap map[string]*corev1.ConfigMap
	errors    map[string]error
}

// validateNativeReferences preflights native Pod references without copying
// credentials into diagnostics or mutating the user-supplied template.
func validateNativeReferences(ctx context.Context, reader client.Reader, namespace string, template *corev1.PodTemplateSpec) error {
	if ctx == nil || reader == nil || template == nil || namespace == "" {
		return diagnostics.New("InvalidConfiguration", "native reference validation requires a namespace and Pod template; correct the deployment configuration", nil)
	}
	v := &nativeReferenceValidator{ctx: ctx, reader: reader, namespace: namespace, secrets: map[string]*corev1.Secret{}, configMap: map[string]*corev1.ConfigMap{}, errors: map[string]error{}}
	for _, ref := range podReferences(template) {
		if err := v.check(ref); err != nil {
			return err
		}
	}
	return nil
}

// podReference preserves the requirements of each use of a Secret or ConfigMap.
// Optionality and keys belong to the use, not just to the referenced object.
type podReference struct {
	Kind, Name           string
	Keys                 []string
	Optional, BinaryData bool
}

func podReferences(template *corev1.PodTemplateSpec) []podReference {
	var refs []podReference
	add := func(kind, name string, keys []string, optional, binaryData bool) {
		refs = append(refs, podReference{Kind: kind, Name: name, Keys: keys, Optional: optional, BinaryData: binaryData})
	}
	for _, containers := range [][]corev1.Container{template.Spec.InitContainers, template.Spec.Containers} {
		for _, container := range containers {
			for _, env := range container.Env {
				if env.ValueFrom == nil {
					continue
				}
				if ref := env.ValueFrom.SecretKeyRef; ref != nil {
					add("Secret", ref.Name, []string{ref.Key}, optionalReference(ref.Optional), false)
				}
				if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
					add("ConfigMap", ref.Name, []string{ref.Key}, optionalReference(ref.Optional), false)
				}
			}
			for _, source := range container.EnvFrom {
				if ref := source.SecretRef; ref != nil {
					add("Secret", ref.Name, nil, optionalReference(ref.Optional), false)
				}
				if ref := source.ConfigMapRef; ref != nil {
					add("ConfigMap", ref.Name, nil, optionalReference(ref.Optional), false)
				}
			}
		}
	}
	for _, volume := range template.Spec.Volumes {
		if ref := volume.Secret; ref != nil {
			add("Secret", ref.SecretName, referenceKeys(ref.Items), optionalReference(ref.Optional), true)
		}
		if ref := volume.ConfigMap; ref != nil {
			add("ConfigMap", ref.Name, referenceKeys(ref.Items), optionalReference(ref.Optional), true)
		}
		if ref := volume.Projected; ref != nil {
			for _, source := range ref.Sources {
				if projection := source.Secret; projection != nil {
					add("Secret", projection.Name, referenceKeys(projection.Items), optionalReference(projection.Optional), true)
				}
				if projection := source.ConfigMap; projection != nil {
					add("ConfigMap", projection.Name, referenceKeys(projection.Items), optionalReference(projection.Optional), true)
				}
			}
		}
	}
	for _, ref := range template.Spec.ImagePullSecrets {
		add("Secret", ref.Name, nil, false, false)
	}
	return refs
}

func optionalReference(optional *bool) bool { return optional != nil && *optional }

func referenceKeys(items []corev1.KeyToPath) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	return keys
}

func (v *nativeReferenceValidator) check(ref podReference) error {
	kind, name := ref.Kind, ref.Name
	if name == "" {
		return diagnostics.New("InvalidConfiguration", fmt.Sprintf("%s reference has no name in namespace %s; set its resource name", kind, v.namespace), nil)
	}
	for _, key := range ref.Keys {
		if key == "" {
			return diagnostics.New("InvalidConfiguration", fmt.Sprintf("%s %s/%s reference has no key; set the referenced key", kind, v.namespace, name), nil)
		}
	}
	cacheKey := kind + "/" + name
	readErr, seen := v.errors[cacheKey]
	if !seen {
		key := client.ObjectKey{Namespace: v.namespace, Name: name}
		if kind == "Secret" {
			object := &corev1.Secret{}
			readErr = v.reader.Get(v.ctx, key, object)
			v.secrets[name] = object
		} else {
			object := &corev1.ConfigMap{}
			readErr = v.reader.Get(v.ctx, key, object)
			v.configMap[name] = object
		}
		v.errors[cacheKey] = readErr
	}
	if readErr != nil {
		if apierrors.IsNotFound(readErr) {
			if ref.Optional {
				return nil
			}
			return diagnostics.New("DependencyNotFound",
				fmt.Sprintf("%s %s/%s does not exist; create it in this namespace or correct the reference", kind, v.namespace, name),
				fmt.Errorf("read %s %s/%s: %w", kind, v.namespace, name, readErr))
		}
		reason := "DependencyReadFailed"
		action := "check API server connectivity and retry"
		if apierrors.IsForbidden(readErr) || apierrors.IsUnauthorized(readErr) {
			reason, action = "DependencyAccessDenied", "grant the operator permission to read this resource"
		}
		return diagnostics.New(reason, fmt.Sprintf("cannot read %s %s/%s; %s", kind, v.namespace, name, action), fmt.Errorf("read %s %s/%s: %w", kind, v.namespace, name, readErr))
	}
	var object client.Object
	if kind == "Secret" {
		object = v.secrets[name]
	} else {
		object = v.configMap[name]
	}
	if object.GetDeletionTimestamp() != nil {
		return diagnostics.New("DependencyTerminating", fmt.Sprintf("%s %s/%s is terminating; wait for deletion and recreate it or select another resource", kind, v.namespace, name), nil)
	}
	for _, key := range ref.Keys {
		present := false
		if kind == "Secret" {
			_, present = v.secrets[name].Data[key]
		} else {
			_, present = v.configMap[name].Data[key]
			if !present && ref.BinaryData {
				_, present = v.configMap[name].BinaryData[key]
			}
		}
		if !present && !ref.Optional {
			return diagnostics.New("DependencyKeyMissing", fmt.Sprintf("%s %s/%s has no key %s; add the key or correct the reference", kind, v.namespace, name, key), nil)
		}
	}
	return nil
}
