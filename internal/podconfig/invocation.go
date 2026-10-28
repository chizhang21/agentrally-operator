package podconfig

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// InvocationPort accepts an explicit http port or a single unambiguous TCP port.
func InvocationPort(container corev1.Container) (int32, error) {
	var named, single int32
	count := 0
	for _, port := range container.Ports {
		if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
			continue
		}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return 0, fmt.Errorf("invalid invocation port")
		}
		count++
		single = port.ContainerPort
		if port.Name == "http" {
			if named != 0 {
				return 0, fmt.Errorf("ambiguous http port")
			}
			named = port.ContainerPort
		}
	}
	if named != 0 {
		return named, nil
	}
	if count == 1 {
		return single, nil
	}
	return 0, fmt.Errorf("an http port or one TCP port is required")
}
