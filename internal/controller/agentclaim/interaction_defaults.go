package agentclaim

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/diagnostics"
	"arally/operator/internal/podconfig"
	"arally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolve on a copy: defaults and discovered infrastructure never become user spec.
func (r *Reconciler) resolveInteraction(ctx context.Context, w *v1.AgentClaim, core *sandbox.Sandbox) (*v1.InteractionEndpoint, error) {
	e := w.Spec.Interaction.DeepCopy()
	if e.Port == 0 {
		port, err := interactionPort(w, core)
		if err != nil {
			return nil, err
		}
		e.Port = port
	}
	if e.Type == "OpenShiftRoute" {
		if e.Scheme == "" {
			e.Scheme = "HTTPS"
		}
		return resolvedInteraction(e)
	}
	if e.Type != "HTTPRoute" {
		return nil, fmt.Errorf("interaction.type must be HTTPRoute or OpenShiftRoute")
	}
	// Fully specified endpoints keep their existing explicit-parent behavior.
	if e.GatewayRef != nil && e.Hostname != "" && e.Scheme != "" {
		return resolvedInteraction(e)
	}
	return r.resolveHTTPInteraction(ctx, w, e)
}

func (r *Reconciler) resolveHTTPInteraction(ctx context.Context, w *v1.AgentClaim, e *v1.InteractionEndpoint) (*v1.InteractionEndpoint, error) {
	defaults, err := r.groupHTTPRouteDefaults(ctx, w)
	if err != nil {
		return nil, err
	}
	if e.GatewayRef == nil && defaults.GatewayRef != nil {
		e.GatewayRef = defaults.GatewayRef.DeepCopy()
	}
	if e.Hostname == "" && defaults.Domain != "" {
		e.Hostname = interactionHostname(w, defaults.Domain)
	}
	selected, err := r.selectInteractionGateway(ctx, w, e)
	if err != nil {
		return nil, err
	}
	e.GatewayRef = selected.ref.DeepCopy()
	if e.Scheme == "" {
		e.Scheme = selected.scheme
	}
	if e.Hostname == "" {
		if !strings.HasPrefix(selected.hostname, "*.") {
			return nil, interactionDiagnostic("InteractionDomainRequired", "Set Group interactionDefaults.httpRoute.domain or Claim interaction.hostname; the Gateway listener has no wildcard domain")
		}
		e.Hostname = interactionHostname(w, strings.TrimPrefix(selected.hostname, "*."))
	}
	return resolvedInteraction(e)
}

func (r *Reconciler) groupHTTPRouteDefaults(ctx context.Context, w *v1.AgentClaim) (*v1.HTTPRouteDefaults, error) {
	group := &v1.AgentGroup{}
	if err := r.Get(ctx, client.ObjectKey{Name: w.Spec.AgentGroupRef.Name}, group); err != nil {
		return nil, err
	}
	if group.Spec.InteractionDefaults != nil && group.Spec.InteractionDefaults.HTTPRoute != nil {
		return group.Spec.InteractionDefaults.HTTPRoute.DeepCopy(), nil
	}
	return &v1.HTTPRouteDefaults{}, nil
}

func interactionPort(w *v1.AgentClaim, core *sandbox.Sandbox) (int32, error) {
	var containers []corev1.Container
	if core != nil {
		containers = core.Spec.PodTemplate.Spec.Containers
	}
	if len(containers) == 0 && w.Spec.Runtime != nil {
		containers = []corev1.Container{{Name: "agent", Ports: w.Spec.Runtime.Ports}}
	}
	index := podconfig.AgentIndex(containers, w.Spec.AgentContainer)
	if index < 0 {
		return 0, interactionDiagnostic("InteractionPortRequired", "Declare the Agent's TCP port or set interaction.port")
	}
	distinct, named := map[int32]bool{}, map[int32]bool{}
	for _, p := range containers[index].Ports {
		if (p.Protocol != "" && p.Protocol != corev1.ProtocolTCP) || p.ContainerPort < 1 || p.ContainerPort > 65535 {
			continue
		}
		distinct[p.ContainerPort] = true
		if p.Name == "http" {
			named[p.ContainerPort] = true
		}
	}
	if len(named) == 1 {
		for port := range named {
			return port, nil
		}
	}
	if len(named) == 0 && len(distinct) == 1 {
		for port := range distinct {
			return port, nil
		}
	}
	return 0, interactionDiagnostic("InteractionPortRequired", "Declare one named http TCP port, one TCP port, or set interaction.port explicitly")
}

// Include namespace and a hash of the original identity to avoid normalization,
// concatenation and truncation collisions between Groups and Claims.
func interactionHostname(w *v1.AgentClaim, domain string) string {
	sum := sha256.Sum256([]byte(w.Namespace + "/" + w.Name))
	label := strings.ReplaceAll(w.Name+"-"+w.Namespace, ".", "-")
	if len(label) > 46 {
		label = label[:46]
	}
	label = strings.Trim(label, "-") + fmt.Sprintf("-%x", sum[:8])
	return label + "." + domain
}

func interactionDiagnostic(reason, message string) error {
	return diagnostics.New(reason, message, nil)
}

func resolvedInteraction(endpoint *v1.InteractionEndpoint) (*v1.InteractionEndpoint, error) {
	if err := validateInteraction(endpoint); err != nil {
		return nil, interactionDiagnostic("InvalidConfiguration", err.Error())
	}
	return endpoint, nil
}
