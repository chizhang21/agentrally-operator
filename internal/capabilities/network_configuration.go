package capabilities

import (
	"context"
	"path"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func networkConfiguration(ctx context.Context, reader client.Reader, ds *appsv1.DaemonSet, rule networkDefinition, containers []corev1.Container) (string, string, error) {
	if rule.Status != "" {
		return rule.Status, "PolicyConfigurationUnverified", nil
	}
	if rule.Provider == "Cilium" {
		return ciliumConfiguration(ctx, reader, ds, containers)
	}
	if rule.BooleanOption != "" {
		value, found := componentOption(containers, rule.BooleanOption, true)
		if !found && rule.DefaultBooleanValue != nil {
			value, found = strconv.FormatBool(*rule.DefaultBooleanValue), true
		}
		if !found {
			return "Unknown", "CNIConfigurationUnavailable", nil
		}
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return "Unknown", "CNIConfigurationUnrecognized", nil
		}
		if !enabled {
			return "Unsupported", "NetworkPolicyDisabled", nil
		}
	}
	return "Supported", "PolicyControllerConfigurationObserved", nil
}

// Boolean flags use pflag/flag's bare-flag=true semantics; strings consume the
// next token. Later flags override earlier flags in the same component.
// Conflicting values across separate components remain uncertain.
func componentOption(containers []corev1.Container, name string, boolean bool) (string, bool) {
	value, found := "", false
	for _, container := range containers {
		arguments := append(append([]string{}, container.Command...), container.Args...)
		if len(container.Command) > 0 && oneOf(path.Base(container.Command[0]), "sh", "bash", "ash", "dash", "ksh") {
			return "", true
		}
		current, present := "", false
		for i := 0; i < len(arguments); i++ {
			argument := arguments[i]
			if strings.Contains(argument, "$(") || strings.Contains(argument, "${") {
				return "", true
			}
			if argument == "--" {
				break
			}
			if strings.HasPrefix(argument, "--"+name+"=") {
				current, present = strings.TrimPrefix(argument, "--"+name+"="), true
			} else if argument == "--"+name {
				current, present = "", true
				if boolean {
					current = "true"
				} else if i+1 < len(arguments) && !strings.HasPrefix(arguments[i+1], "--") {
					i++
					current = arguments[i]
				}
			}
		}
		if present {
			if found && value != current {
				return "", true
			}
			value, found = current, true
		}
	}
	return value, found
}

func ciliumConfiguration(ctx context.Context, reader client.Reader, ds *appsv1.DaemonSet, containers []corev1.Container) (string, string, error) {
	if len(containers) != 1 {
		return "Unknown", "CNIConfigurationUnrecognized", nil
	}
	container := containers[0]
	file, fileKnown := ciliumOption(container, nil, "config", "", false, true)
	if !fileKnown || file != "" {
		return "Unknown", "CNIConfigurationUnrecognized", nil
	}
	data, configured, err := ciliumConfigData(ctx, reader, ds, container)
	if err != nil {
		return "Unknown", "CNIConfigurationUnavailable", err
	}
	// Apply defaults only after resolving the configuration source. Explicit
	// flags and environment entries override individual generated settings.
	mode, modeKnown := ciliumOption(container, data, "enable-policy", "default", false, configured)
	kubernetes, kubernetesKnown := ciliumOption(container, data, "enable-k8s-networkpolicy", "true", true, configured)
	watcher, watcherKnown := ciliumOption(container, data, "enable-k8s", "true", true, configured)
	audit, auditKnown := ciliumOption(container, data, "policy-audit-mode", "false", true, configured)
	enabled, parseErr := strconv.ParseBool(kubernetes)
	watching, watcherErr := strconv.ParseBool(watcher)
	auditEnabled, auditErr := strconv.ParseBool(audit)
	if modeKnown && mode == "never" || kubernetesKnown && parseErr == nil && !enabled || watcherKnown && watcherErr == nil && !watching {
		return "Unsupported", "NetworkPolicyDisabled", nil
	}
	if auditKnown && auditErr == nil && auditEnabled {
		return "Unsupported", "PolicyAuditModeEnabled", nil
	}
	if !modeKnown || !kubernetesKnown || !watcherKnown || !auditKnown || !oneOf(mode, "default", "always") || parseErr != nil || watcherErr != nil || auditErr != nil {
		return "Unknown", "CNIConfigurationUnrecognized", nil
	}
	return "Supported", "PolicyControllerConfigurationObserved", nil
}

func ciliumConfigData(ctx context.Context, reader client.Reader, ds *appsv1.DaemonSet, container corev1.Container) (map[string]string, bool, error) {
	config := mountedCiliumConfig(ds, container)
	if config != "" {
		return ciliumConfigMap(ctx, reader, client.ObjectKey{Namespace: ds.Namespace, Name: config})
	}
	return generatedCiliumConfig(ctx, reader, ds, container)
}

func ciliumConfigMap(ctx context.Context, reader client.Reader, key client.ObjectKey) (map[string]string, bool, error) {
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if cm.DeletionTimestamp != nil || len(cm.BinaryData) != 0 {
		return nil, false, nil
	}
	return cm.Data, true, nil
}

func ciliumOption(container corev1.Container, data map[string]string, key, fallback string, boolean, configured bool) (string, bool) {
	if value, found := componentOption([]corev1.Container{container}, key, boolean); found {
		return value, true
	}
	environment := "CILIUM_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
	for i := len(container.Env) - 1; i >= 0; i-- {
		env := container.Env[i]
		if env.Name == environment {
			return env.Value, env.ValueFrom == nil
		}
	}
	if len(container.EnvFrom) != 0 || !configured {
		return "", false
	}
	if value, found := data[key]; found {
		return value, true
	}
	return fallback, true
}

func mountedCiliumConfig(ds *appsv1.DaemonSet, container corev1.Container) string {
	directory, found := ciliumOption(container, nil, "config-dir", "", false, true)
	if !found || directory == "" {
		return ""
	}
	mount, relative := ciliumDirectoryMount(container, directory)
	if mount.Name == "" || relative != "" {
		return ""
	}
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == mount.Name && volume.ConfigMap != nil && len(volume.ConfigMap.Items) == 0 {
			return volume.ConfigMap.Name
		}
	}
	return ""
}
