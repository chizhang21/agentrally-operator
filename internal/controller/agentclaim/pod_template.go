package agentclaim

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

func desiredExecution(ctx context.Context, c client.Reader, workload *v1.AgentClaim) (*resolvedTemplate, error) {
	resolved, err := resolveSandboxTemplate(ctx, c, workload)
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
	if err := applyAgentSecurity(ctx, c, workload, template); err != nil {
		return nil, err
	}
	index := podconfig.AgentIndex(template.Spec.Containers, workload.Spec.AgentContainer)
	if index < 0 {
		return nil, diagnostics.New("InvalidConfiguration", fmt.Sprintf("agentContainer %q is absent from SandboxTemplate", workload.Spec.AgentContainer), nil)
	}
	container := &template.Spec.Containers[index]
	if err := applyModelProvider(ctx, c, workload, container); err != nil {
		return nil, err
	}
	if err := applyWorkspace(workload, resolved, container); err != nil {
		return nil, err
	}
	if err := applyBuiltinWorkspacePermissions(ctx, c, workload, template); err != nil {
		return nil, err
	}
	if err := validateVolumeClaimTemplates(resolved.VolumeClaimTemplates, &template.Spec); err != nil {
		return nil, err
	}
	if err := validateNativeReferences(ctx, c, workload.Namespace, template); err != nil {
		return nil, err
	}
	return resolved, nil
}

func applyModelProvider(ctx context.Context, c client.Reader, workload *v1.AgentClaim, container *corev1.Container) error {
	if provider := workload.Spec.ModelProvider; provider != nil {
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
		if err := c.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: ref.Name}, secret); err != nil {
			return diagnostics.ReferenceError("Secret", workload.Namespace, ref.Name, err)
		}
		if secret.DeletionTimestamp != nil {
			return diagnostics.New("DependencyTerminating", fmt.Sprintf("model Secret %s/%s is terminating", workload.Namespace, ref.Name), nil)
		}
		if _, exists := secret.Data[ref.Key]; !exists {
			return diagnostics.New("DependencyKeyMissing", fmt.Sprintf("model Secret %s/%s has no key %s", workload.Namespace, ref.Name, ref.Key), nil)
		}
		if len(secret.Data[ref.Key]) == 0 {
			return diagnostics.New("InvalidConfiguration", fmt.Sprintf("model Secret %s/%s key %s must be nonempty", workload.Namespace, ref.Name, ref.Key), nil)
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
