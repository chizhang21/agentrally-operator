package capabilities

import (
	"context"
	"path"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolve the official build-config init workflow without executing the image
// or reimplementing Cilium's node-specific configuration merger.
func generatedCiliumConfig(ctx context.Context, reader client.Reader, ds *appsv1.DaemonSet, agent corev1.Container) (map[string]string, bool, error) {
	directory, known := ciliumOption(agent, nil, "config-dir", "", false, true)
	if !known || directory == "" {
		return nil, false, nil
	}
	for _, init := range ds.Spec.Template.Spec.InitContainers {
		arguments := append(append([]string{}, init.Command...), init.Args...)
		if len(arguments) < 2 || path.Base(arguments[0]) != "cilium-dbg" || arguments[1] != "build-config" {
			continue
		}
		dest, destKnown := ciliumBuildOption(init, "dest", "/tmp/cilium/config-map")
		if !destKnown || !sharedCiliumDirectory(ds, agent, directory, init, dest) {
			return nil, false, nil
		}
		// Restrict recognition to an unfiltered ConfigMap source, optionally
		// followed by the default CiliumNodeConfig source. Other source chains
		// and override filters need their own evidence model.
		for _, option := range []string{"allow-config-keys", "deny-config-keys"} {
			value, known := ciliumBuildOption(init, option, "")
			if !known || value != "" {
				return nil, false, nil
			}
		}
		// An alternate API source cannot be associated with this cluster's
		// ConfigMaps merely from the init manifest.
		for _, option := range []string{"k8s-kubeconfig-path", "k8s-api-server-urls"} {
			value, known := ciliumBuildOption(init, option, "")
			if !known || value != "" {
				return nil, false, nil
			}
		}
		namespace := ciliumInitNamespace(init, ds.Namespace)
		defaultSource := ""
		if namespace != "" {
			defaultSource = "config-map:cilium-config,cilium-node-config:" + namespace
		}
		source, sourceKnown := ciliumBuildOption(init, "source", defaultSource)
		if !sourceKnown || source == "" || strings.ContainsAny(source, "\"' \t\n$") {
			return nil, false, nil
		}
		nodeOverrides := source == defaultSource
		if nodeOverrides {
			source = "config-map:cilium-config"
		}
		name, recognized := strings.CutPrefix(source, "config-map:")
		if !recognized || strings.Contains(name, ",") {
			return nil, false, nil
		}
		if ns, config, namespaced := strings.Cut(name, "/"); namespaced {
			namespace, name = ns, config
		}
		if namespace == "" || name == "" || strings.Contains(name, "/") {
			return nil, false, nil
		}
		if nodeOverrides {
			known, err := ciliumNodePolicyUnchanged(ctx, reader, namespace)
			if err != nil || !known {
				return nil, false, err
			}
		}
		return ciliumConfigMap(ctx, reader, client.ObjectKey{Namespace: namespace, Name: name})
	}
	return nil, false, nil
}

func ciliumBuildOption(init corev1.Container, name, fallback string) (string, bool) {
	// build-config uses StringSlice for source and override filters. Repeated
	// flags append rather than replacing, so reject them instead of applying
	// the agent's ordinary last-value semantics.
	arguments := append(append([]string{}, init.Command...), init.Args...)
	count := 0
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--"+name || strings.HasPrefix(argument, "--"+name+"=") {
			count++
		}
	}
	if count > 1 {
		return "", false
	}
	return ciliumOption(init, nil, name, fallback, false, true)
}

func ciliumInitNamespace(init corev1.Container, namespace string) string {
	for i := len(init.Env) - 1; i >= 0; i-- {
		env := init.Env[i]
		if env.Name != "CILIUM_K8S_NAMESPACE" {
			continue
		}
		if env.ValueFrom == nil {
			return env.Value
		}
		if field := env.ValueFrom.FieldRef; field != nil && field.FieldPath == "metadata.namespace" && oneOf(field.APIVersion, "", "v1") {
			return namespace
		}
		return ""
	}
	return ""
}

func sharedCiliumDirectory(ds *appsv1.DaemonSet, agent corev1.Container, directory string, init corev1.Container, dest string) bool {
	agentMount, agentPath := ciliumDirectoryMount(agent, directory)
	initMount, initPath := ciliumDirectoryMount(init, dest)
	if agentMount.Name == "" || agentMount.Name != initMount.Name || agentPath != initPath || initMount.ReadOnly {
		return false
	}
	// build-config does not remove arbitrary files left by other writers.
	for _, container := range append(append([]corev1.Container{}, ds.Spec.Template.Spec.InitContainers...), ds.Spec.Template.Spec.Containers...) {
		if container.Name == init.Name || container.Name == agent.Name {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == agentMount.Name && !mount.ReadOnly {
				return false
			}
		}
	}
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == agentMount.Name {
			return volume.EmptyDir != nil
		}
	}
	return false
}

func ciliumDirectoryMount(container corev1.Container, directory string) (corev1.VolumeMount, string) {
	var selected corev1.VolumeMount
	var relative string
	if !path.IsAbs(directory) || strings.Contains(directory, "$") {
		return selected, relative
	}
	for _, mount := range container.VolumeMounts {
		root := path.Clean(mount.MountPath)
		if strings.HasPrefix(root, strings.TrimSuffix(path.Clean(directory), "/")+"/") {
			// A file or directory mounted inside config-dir can hide generated
			// policy values even when the parent volume is shared correctly.
			return corev1.VolumeMount{}, ""
		}
		suffix, below := strings.CutPrefix(path.Clean(directory), strings.TrimSuffix(root, "/")+"/")
		if path.Clean(directory) != root && !below || len(mount.MountPath) <= len(selected.MountPath) {
			continue
		}
		if !below {
			suffix = ""
		}
		selected, relative = mount, suffix
	}
	if selected.SubPath != "" || selected.SubPathExpr != "" {
		return corev1.VolumeMount{}, ""
	}
	return selected, relative
}

func ciliumNodePolicyUnchanged(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	// These are versions of the same optional resource, not independent object
	// sets. A missing API is harmless; unreadable overrides are not absence.
	for _, version := range []string{"v2", "v2alpha1"} {
		configs := &unstructured.UnstructuredList{}
		configs.SetGroupVersionKind(schema.GroupVersionKind{Group: "cilium.io", Version: version, Kind: "CiliumNodeConfigList"})
		if err := reader.List(ctx, configs, client.InNamespace(namespace)); err != nil {
			if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		for _, config := range configs.Items {
			selector, found, err := unstructured.NestedFieldNoCopy(config.Object, "spec", "nodeSelector")
			if err != nil {
				return false, err
			}
			// A nil selector is not applied by the default list source. Any
			// non-nil selector can target nodes now or after a label change.
			if !found || selector == nil {
				continue
			}
			defaults, _, err := unstructured.NestedStringMap(config.Object, "spec", "defaults")
			if err != nil {
				return false, err
			}
			for _, key := range []string{"enable-policy", "enable-k8s-networkpolicy", "enable-k8s", "policy-audit-mode"} {
				if _, overridden := defaults[key]; overridden {
					return false, nil
				}
			}
		}
		return true, nil
	}
	return true, nil
}
