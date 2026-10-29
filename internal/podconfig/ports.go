package podconfig

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// HTTPPort prefers a named http TCP port, otherwise one distinct TCP port.
// Multiple names for the same port do not introduce another destination.
func HTTPPort(container corev1.Container) (int32, error) {
	var named, single int32
	distinct := make(map[int32]bool)
	for _, port := range container.Ports {
		if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
			continue
		}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return 0, fmt.Errorf("invalid HTTP port")
		}
		distinct[port.ContainerPort] = true
		single = port.ContainerPort
		if port.Name == "http" {
			if named != 0 && named != port.ContainerPort {
				return 0, fmt.Errorf("ambiguous http port")
			}
			named = port.ContainerPort
		}
	}
	if named != 0 {
		return named, nil
	}
	if len(distinct) == 1 {
		return single, nil
	}
	return 0, fmt.Errorf("a named http TCP port or one distinct TCP port is required")
}
