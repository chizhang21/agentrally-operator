//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/connector"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The driver installs the published old chart before this test starts. Each
// Connector mode runs in its own disposable cluster, including the old CRDs.
func TestChartUpgrade(t *testing.T) {
	previous := os.Getenv("E2E_PREVIOUS_CHART")
	if previous == "" {
		t.Skip("requires E2E_UPGRADE_FROM")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	if err := appsv1.AddToScheme(kube.Scheme()); err != nil {
		t.Fatal(err)
	}
	const namespace, release = "agentrally-system", "agentrally-operator"
	helmUpgrade := func(chart string, overrides ...string) {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		args := []string{"--kubeconfig", kubeconfig, "upgrade", release, chart, "-n", namespace,
			"--reset-then-reuse-values", "--wait", "--timeout=240s"}
		out, err := exec.CommandContext(commandCtx, "helm", append(args, overrides...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm upgrade: %v\n%s", err, out)
		}
	}
	fixture := prepareComponentUpgrade(t, kube, kubeconfig)
	group := fixture.group

	verifyConnector := func(string) {}
	connectorEnabled := os.Getenv("E2E_UPGRADE_CONNECTOR") == "true"
	if connectorEnabled {
		commands := make(chan connector.Request, 1)
		responses := make(chan connector.Response, 1)
		backend := connectorBackend(t, kube, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer upgrade-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/requests"):
				select {
				case request := <-commands:
					_ = json.NewEncoder(w).Encode(request)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses"):
				var response connector.Response
				if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				select {
				case responses <- response:
				default:
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "upgrade-connector", Namespace: namespace}, Data: map[string][]byte{
			"token": []byte("upgrade-test"), "ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw}),
		}}
		create(t, ctx, kube, secret)
		helmUpgrade(previous, "--set", "connector.enabled=true", "--set", "connector.id=upgrade-test",
			"--set", "connector.credentialsSecret="+secret.Name, "--set-string", "connector.endpoint="+backend.URL,
			"--set", "connector.caFile=/var/run/agentrally-connector/ca.crt")
		verifyConnector = func(id string) {
			t.Helper()
			commands <- connector.Request{ID: id, Method: http.MethodGet, Resource: "agentgroups", Name: group.Name,
				Scope: &connector.RequestScope{GroupName: group.Name, GroupUID: string(group.UID)}}
			select {
			case response := <-responses:
				var observed v1.AgentGroup
				if response.ID != id || response.Status != http.StatusOK || json.Unmarshal(response.Body, &observed) != nil || observed.UID != group.UID {
					t.Fatalf("%s: unexpected Connector response: %+v", id, response)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: no response from deployed Connector", id)
			}
		}
		verifyConnector("before-upgrade")
	}

	chart := os.Getenv("E2E_CHART")
	if chart == "" {
		chart = "chart"
	}
	if !filepath.IsAbs(chart) {
		chart = filepath.Join("../..", chart)
	}
	image, relayImage := os.Getenv("E2E_IMAGE"), os.Getenv("E2E_CONNECTOR_IMAGE")
	imageValues := func(prefix, image string) []string {
		t.Helper()
		i := strings.LastIndex(image, ":")
		if i <= 0 || i == len(image)-1 {
			t.Fatalf("missing tagged candidate image: %q", image)
		}
		return []string{"--set-string", prefix + ".repository=" + image[:i], "--set-string", prefix + ".tag=" + image[i+1:],
			"--set", prefix + ".pullPolicy=IfNotPresent"}
	}
	t.Log("Upgrade the published release with its existing explicit values")
	helmUpgrade(chart, append(imageValues("image", image), imageValues("connector.image", relayImage)...)...)
	// Wait for the previous Operator Pods to terminate before verifying the candidate.
	await(t, "only candidate Operator Pods remain", func() (bool, error) {
		var pods corev1.PodList
		if err := kube.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
			"app.kubernetes.io/name": "agentrally-operator", "app.kubernetes.io/instance": release,
		}); err != nil {
			return false, err
		}
		for _, current := range pods.Items {
			if current.Spec.Containers[0].Image != image {
				return false, nil
			}
		}
		return len(pods.Items) > 0, nil
	})
	relay := &appsv1.Deployment{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: release + "-connector"}, relay)
	if connectorEnabled {
		if err != nil || relay.Status.ReadyReplicas != 1 || relay.Spec.Template.Spec.Containers[0].Image != relayImage {
			t.Fatal("separate Connector is not running the candidate image", err)
		}
		await(t, "only candidate Connector Pods remain", func() (bool, error) {
			var pods corev1.PodList
			if err := kube.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
				"app.kubernetes.io/name": "agentrally-connector", "app.kubernetes.io/instance": release,
			}); err != nil {
				return false, err
			}
			for _, pod := range pods.Items {
				if pod.DeletionTimestamp != nil || pod.Spec.Containers[0].Image != relayImage {
					return false, nil
				}
			}
			return len(pods.Items) > 0, nil
		})
		verifyConnector("after-upgrade")
	} else if !apierrors.IsNotFound(err) {
		t.Fatal("disabled Connector has a Deployment", err)
	}
	fixture.verify(t, kube, kubeconfig)
	fixture.verifyResume(t, kube, kubeconfig)
}
