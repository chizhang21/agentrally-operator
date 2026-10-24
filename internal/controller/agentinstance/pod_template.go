package agentinstance

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/podconfig"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func desiredExecution(ctx context.Context, c client.Reader, workload *v1.AgentInstance) (*resolvedTemplate, error) {
	revision, err := loadDefinitionRevision(ctx, c, workload)
	if err != nil {
		return nil, err
	}
	effective := workload.DeepCopy()
	if effective.Spec.Workspace == nil {
		effective.Spec.Workspace = revision.Spec.Workspace.DeepCopy()
	}
	resolved, err := resolvePublishedTemplate(effective, revision)
	if err != nil {
		return nil, err
	}
	template := resolved.PodTemplate
	if err := validateConfigurationMarker(template); err != nil {
		return nil, err
	}
	// Agents have no Kubernetes API credentials unless explicitly requested.
	if template.Spec.AutomountServiceAccountToken == nil {
		template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	if len(template.Spec.Containers) == 0 {
		return nil, diagnostics.New("InvalidConfiguration", "SandboxTemplate requires at least one container", nil)
	}
	if err := applyAgentSecurity(ctx, c, workload.Namespace, revision.Spec.Security, template); err != nil {
		return nil, err
	}
	index := podconfig.AgentIndex(template.Spec.Containers, revision.Spec.AgentContainer)
	if index < 0 {
		return nil, diagnostics.New("InvalidConfiguration", fmt.Sprintf("agentContainer %q is absent from SandboxTemplate", revision.Spec.AgentContainer), nil)
	}
	container := &template.Spec.Containers[index]
	if err := applyModelProvider(ctx, c, workload.Namespace, revision.Spec.ModelProvider, container); err != nil {
		return nil, err
	}
	if err := applyWorkspace(ctx, c, effective, resolved, container); err != nil {
		return nil, err
	}
	applyBuiltinWorkspacePermissions(effective, revision.Spec.Template != "", template)
	if err := validateVolumeClaimTemplates(resolved.VolumeClaimTemplates, &template.Spec); err != nil {
		return nil, err
	}
	if err := validateNativeReferences(ctx, c, workload.Namespace, template); err != nil {
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
		setEnv(container, corev1.EnvVar{Name: "OPENAI_BASE_URL", Value: provider.BaseURL})
		setEnv(container, corev1.EnvVar{Name: "ARALLY_MODEL_NAME", Value: provider.Model})
		setEnv(container, corev1.EnvVar{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &ref}})
	}
	return nil
}

func setEnv(container *corev1.Container, desired corev1.EnvVar) {
	filtered := container.Env[:0]
	for _, env := range container.Env {
		if env.Name != desired.Name {
			filtered = append(filtered, env)
		}
	}
	container.Env = append(filtered, desired)
}
