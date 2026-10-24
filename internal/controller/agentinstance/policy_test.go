package agentinstance

import (
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGroupExecutionPolicyGatesResolvedTemplates(t *testing.T) {
	for _, builtin := range []bool{false, true} {
		for _, scenario := range []string{"class-missing", "class-denied", "image-tag", "init-tag", "digest-accepted"} {
			if builtin && scenario == "init-tag" {
				continue
			}
			name := "custom/" + scenario
			if builtin {
				name = "builtin/" + scenario
			}
			t.Run(name, func(t *testing.T) {
				group, ns, definition, w, source := testutil.Fixture()
				group.Spec.ExecutionPolicy = &v1.ExecutionPolicy{AllowedRuntimeClasses: []string{"isolated"}, RequireImageDigest: true}
				image := "example/agent@sha256:" + strings.Repeat("a", 64)
				source.Spec.RuntimeClassName = ptr.To("isolated")
				source.Spec.Containers[0].Image = image
				reason := "RuntimeClassDenied"
				switch scenario {
				case "class-missing":
					source.Spec.RuntimeClassName = nil
				case "class-denied":
					source.Spec.RuntimeClassName = ptr.To("other")
				case "image-tag":
					source.Spec.Containers[0].Image, reason = "example/agent:latest", "ImageDigestRequired"
				case "init-tag":
					source.Spec.InitContainers, reason = []corev1.Container{{Name: "init", Image: "example/init:latest"}}, "ImageDigestRequired"
				}
				if builtin {
					definition.Spec.TemplateRef, definition.Spec.Template = nil, "generic"
					definition.Spec.Container = &v1.AgentContainerSpec{Image: source.Spec.Containers[0].Image, RuntimeClassName: source.Spec.RuntimeClassName}
				}
				class := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "isolated"}, Handler: "kata"}
				objects := []client.Object{group, ns, definition, w, class}
				if !builtin {
					objects = append(objects, nativeTemplate(definition, source))
				}
				c, scheme := testutil.Client(t, objects...)
				r := &Reconciler{Client: c, Reader: c, Scheme: scheme}
				reconcileAllocation(t, r, w)
				state := getAgentInstance(t, c, w)
				if scenario == "digest-accepted" {
					if state.Status.Provisioning == nil || state.Status.Provisioning.ClaimName == "" {
						t.Fatal("allowed digest-pinned workload was not allocated")
					}
				} else {
					assertReason(t, state, "CapabilityValidated", reason)
					if state.Status.Provisioning != nil {
						t.Fatal("denied execution started allocation")
					}
				}
			})
		}
	}
}
