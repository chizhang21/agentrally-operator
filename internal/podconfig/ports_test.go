package podconfig

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestHTTPPortSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports []corev1.ContainerPort
		want  int32
	}{
		{"single", []corev1.ContainerPort{{ContainerPort: 8080}}, 8080},
		{"aliases", []corev1.ContainerPort{{Name: "web", ContainerPort: 8080}, {Name: "api", ContainerPort: 8080}}, 8080},
		{"named-preferred", []corev1.ContainerPort{{ContainerPort: 9090}, {Name: "http", ContainerPort: 8080}}, 8080},
		{"udp-ignored", []corev1.ContainerPort{{ContainerPort: 53, Protocol: corev1.ProtocolUDP}, {ContainerPort: 8080}}, 8080},
		{"missing", nil, 0},
		{"ambiguous", []corev1.ContainerPort{{ContainerPort: 8080}, {ContainerPort: 9090}}, 0},
		{"ambiguous-named", []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}, {Name: "http", ContainerPort: 9090}}, 0},
		{"invalid", []corev1.ContainerPort{{Name: "http", ContainerPort: 0}}, 0},
		{"invalid-extra", []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}, {ContainerPort: 65536}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, err := HTTPPort(corev1.Container{Ports: tc.ports})
			if port != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatalf("HTTP port=%d error=%v, want %d", port, err, tc.want)
			}
		})
	}
}
