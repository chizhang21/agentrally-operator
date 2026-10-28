// Package executionconfig resolves the shared creation inputs used by instance
// allocation and Definition preparation. It does not create execution resources.
package executionconfig

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/allocation"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/podconfig"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolve applies only shared Definition inputs. Callers add and validate any
// instance workspace after this read-only step.
func Resolve(ctx context.Context, reader client.Reader, namespace, name string, workspace *v1.WorkspaceMount, revision *definition.Revision) (*allocation.Configuration, error) {
	resolved, err := ResolveTemplate(name, workspace, revision)
	if err != nil {
		return nil, err
	}
	template := resolved.PodTemplate
	if err := ValidateMarker(template); err != nil {
		return nil, err
	}
	if template.Spec.AutomountServiceAccountToken == nil {
		template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	if err := applyAgentSecurity(ctx, reader, namespace, revision.Spec.Security, template); err != nil {
		return nil, err
	}
	index := podconfig.AgentIndex(template.Spec.Containers, revision.Spec.AgentContainer)
	if err := applyModelProvider(ctx, reader, namespace, revision.Spec.ModelProvider, &template.Spec.Containers[index]); err != nil {
		return nil, err
	}
	if err := ValidateNativeReferences(ctx, reader, namespace, template); err != nil {
		return nil, err
	}
	return resolved, nil
}

func applyModelProvider(ctx context.Context, c client.Reader, namespace string, provider *v1.ModelProvider, container *corev1.Container) error {
	if provider != nil {
		u, err := url.Parse(provider.BaseURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(provider.BaseURL, " \t\r\n") || strings.TrimSpace(provider.Model) == "" {
			return diagnostics.New("InvalidConfiguration", "modelProvider requires an HTTP(S) baseURL and model name", nil)
		}
		ref := provider.APIKeySecretRef
		if ref.Name == "" || ref.Key == "" {
			return diagnostics.New("InvalidConfiguration", "modelProvider.apiKeySecretRef requires name and key", nil)
		}
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
			return diagnostics.ReferenceError("Secret", namespace, ref.Name, err)
		}
		if secret.DeletionTimestamp != nil {
			return diagnostics.New("DependencyTerminating", fmt.Sprintf("model Secret %s/%s is terminating", namespace, ref.Name), nil)
		}
		if _, exists := secret.Data[ref.Key]; !exists {
			return diagnostics.New("DependencyKeyMissing", fmt.Sprintf("model Secret %s/%s has no key %s", namespace, ref.Name, ref.Key), nil)
		}
		if len(secret.Data[ref.Key]) == 0 {
			return diagnostics.New("InvalidConfiguration", fmt.Sprintf("model Secret %s/%s key %s must be nonempty", namespace, ref.Name, ref.Key), nil)
		}
		ref.Optional = ptr.To(false)
		podconfig.SetEnv(container, corev1.EnvVar{Name: "OPENAI_BASE_URL", Value: provider.BaseURL})
		podconfig.SetEnv(container, corev1.EnvVar{Name: "AGENTRALLY_MODEL_NAME", Value: provider.Model})
		podconfig.SetEnv(container, corev1.EnvVar{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &ref}})
	}
	return nil
}
