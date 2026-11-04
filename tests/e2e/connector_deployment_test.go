//go:build e2e

package e2e_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/connector"
	"agentrally/operator/internal/testutil"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestConnectorDeployment(t *testing.T) {
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	for _, register := range []func(*runtime.Scheme) error{appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := register(kube.Scheme()); err != nil {
			t.Fatal(err)
		}
	}
	const namespace, release = "agentrally-system", "agentrally-operator"
	const relayName = release + "-connector"
	helm := func(args ...string) []byte {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(commandCtx, "helm", append([]string{"--kubeconfig", kubeconfig, "-n", namespace}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm %s: %v\n%s", args[0], err, out)
		}
		return out
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, helm("get", "values", release, "-o", "yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	chart := os.Getenv("E2E_CHART")
	if chart == "" {
		chart = "chart"
	}
	if !filepath.IsAbs(chart) {
		chart = filepath.Join("../..", chart)
	}
	upgrade := func(overrides ...string) {
		t.Helper()
		helm(append([]string{"upgrade", release, chart, "-f", values, "--wait", "--timeout=150s"}, overrides...)...)
	}
	t.Cleanup(func() { upgrade() })

	var online atomic.Bool
	online.Store(true)
	commands := make(chan connector.Request, 1)
	responses := make(chan connector.Response, 1)
	backend := connectorBackend(t, kube, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-deployment-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !online.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
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
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "connector-deployment-credentials", Namespace: namespace},
		Data: map[string][]byte{"token": []byte("local-deployment-test"), "ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})}}
	create(t, ctx, kube, secret)
	t.Cleanup(func() { _ = kube.Delete(context.Background(), secret) })
	operator := &appsv1.Deployment{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: release}, operator); err != nil {
		t.Fatal(err)
	}
	generation := operator.Generation
	enabledValues := []string{"--set", "connector.enabled=true", "--set", "connector.id=deployment-test", "--set", "connector.credentialsSecret=" + secret.Name,
		"--set-string", "connector.endpoint=" + backend.URL, "--set", "connector.caFile=/var/run/agentrally-connector/ca.crt"}
	upgrade(enabledValues...)
	lease := &coordinationv1.Lease{}
	await(t, "Connector acquires its own Lease", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, lease)
		return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "", err
	})
	operatorHealthy := func() {
		t.Helper()
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: release}, operator); err != nil {
			t.Fatal(err)
		}
		if operator.Generation != generation || operator.Status.ReadyReplicas != *operator.Spec.Replicas {
			t.Fatal("Connector change restarted or impaired the Operator")
		}
	}
	operatorHealthy()
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "connector-deployment"}}
	t.Cleanup(func() { _ = kube.Delete(context.Background(), group) })
	request := func(command connector.Request, status int) connector.Response {
		t.Helper()
		commands <- command
		select {
		case response := <-responses:
			if response.ID != command.ID || response.Status != status {
				t.Fatalf("%s: unexpected response: %s %d %s", command.ID, response.ID, response.Status, response.Body)
			}
			return response
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: deployed Connector did not deliver a response", command.ID)
			return connector.Response{}
		}
	}
	request(connector.Request{ID: "create-group", Method: http.MethodPost, Resource: "agentgroups", Scope: &connector.RequestScope{GroupName: group.Name},
		Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"name":"connector-deployment"},"spec":{"allocationPaused":true}}`)}, http.StatusCreated)
	await(t, "Operator reconciles Connector-created Group", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	relayPod := func() corev1.Pod {
		t.Helper()
		var pods corev1.PodList
		if err := kube.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app.kubernetes.io/name": "agentrally-connector", "app.kubernetes.io/instance": release}); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) != 1 {
			t.Fatalf("expected one Connector Pod, got %d", len(pods.Items))
		}
		return pods.Items[0]
	}
	before := relayPod()
	online.Store(false)
	relay := &appsv1.Deployment{}
	await(t, "backend outage changes Connector readiness", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, relay)
		return relay.Status.ReadyReplicas == 0, err
	})
	operatorHealthy()
	// Exercise reconciliation while the Connector is disconnected.
	group.Spec.AllocationPaused = false
	update(t, ctx, kube, group)
	await(t, "Operator reconciles during Connector outage", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "Ready", group.Generation), err
	})
	online.Store(true)
	await(t, "Connector reconnects", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, relay)
		return relay.Status.ReadyReplicas == 1, err
	})
	after := relayPod()
	if after.UID != before.UID || after.Status.ContainerStatuses[0].RestartCount != before.Status.ContainerStatuses[0].RestartCount {
		t.Fatal("backend outage restarted Connector")
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, relay); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(relay.DeepCopy())
	relay.Spec.Template.Annotations = map[string]string{"e2e.agentrally.io/restart": "rolling"}
	if err := kube.Patch(ctx, relay, patch); err != nil {
		t.Fatal(err)
	}
	await(t, "Connector rolling restart completes", func() (bool, error) {
		var pods corev1.PodList
		if err := kube.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app.kubernetes.io/name": "agentrally-connector"}); err != nil {
			return false, err
		}
		if len(pods.Items) != 1 {
			return false, nil
		}
		for _, pod := range pods.Items {
			if pod.UID != before.UID {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						return true, nil
					}
				}
			}
		}
		return false, nil
	})
	verifyManagement := func(id string) {
		t.Helper()
		response := request(connector.Request{ID: id, Method: http.MethodGet, Resource: "agentgroups", Name: group.Name,
			Scope: &connector.RequestScope{GroupName: group.Name, GroupUID: string(group.UID)}}, http.StatusOK)
		var observed v1.AgentGroup
		if err := json.Unmarshal(response.Body, &observed); err != nil || observed.UID != group.UID {
			t.Fatalf("%s: management did not return the existing Group: %v", id, err)
		}
	}
	// PodReady also covers standbys; a round trip proves the replacement took over.
	verifyManagement("after-rollout")
	operatorHealthy()

	// The Lease belongs to Helm. Losing it must not leave unelected Pods Ready.
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, lease); err != nil {
		t.Fatal(err)
	}
	oldLeaseUID := lease.UID
	beforeLeaseLoss := relayPod()
	if err := kube.Delete(ctx, lease); err != nil {
		t.Fatal(err)
	}
	config, _ := testutil.E2EConfig(t, "", "")
	api, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	probe := func(pod, path string) int {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		status := 0
		err := api.CoreV1().RESTClient().Get().Namespace(namespace).Resource("pods").Name(pod + ":8081").SubResource("proxy").Suffix(path).Do(probeCtx).StatusCode(&status).Error()
		if apiStatus, ok := err.(apierrors.APIStatus); ok {
			return int(apiStatus.Status().Code)
		}
		return status
	}
	await(t, "restarted Connector is live but not ready without its Lease", func() (bool, error) {
		pod := relayPod()
		if len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].RestartCount <= beforeLeaseLoss.Status.ContainerStatuses[0].RestartCount {
			return false, nil
		}
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, &coordinationv1.Lease{})
		return apierrors.IsNotFound(err) && probe(pod.Name, "healthz") == http.StatusOK && probe(pod.Name, "readyz/connector") == http.StatusInternalServerError, nil
	})
	operatorHealthy()
	upgrade(enabledValues...)
	await(t, "Helm repairs Lease and Connector elects again", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, lease)
		return lease.UID != oldLeaseUID && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "", err
	})
	verifyManagement("after-lease-repair")
	operatorHealthy()
	upgrade()
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.ServiceAccount{}, &coordinationv1.Lease{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}} {
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: relayName}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("disabled Connector retains %T: %v", object, err)
		}
	}
	for _, object := range []client.Object{&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}} {
		if err := kube.Get(ctx, client.ObjectKey{Name: release + "-" + namespace + "-connector"}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("disabled Connector retains %T: %v", object, err)
		}
	}
	await(t, "disabled Connector Pods removed", func() (bool, error) {
		var pods corev1.PodList
		err := kube.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app.kubernetes.io/name": "agentrally-connector", "app.kubernetes.io/instance": release})
		return len(pods.Items) == 0, err
	})
	if err := kube.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatalf("disabling Connector removed user credentials: %v", err)
	}
	operatorHealthy()
}

// Serve HTTPS on the host address reachable from the disposable Kind network.
func connectorBackend(t *testing.T, kube client.Client, handler http.Handler) *httptest.Server {
	t.Helper()
	var nodes corev1.NodeList
	if err := kube.List(t.Context(), &nodes); err != nil {
		t.Fatal(err)
	}
	var nodeIP string
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				nodeIP = address.Address
			}
		}
	}
	if nodeIP == "" {
		t.Fatal("Kind node has no internal IP")
	}
	route, err := net.Dial("udp", net.JoinHostPort(nodeIP, "443"))
	if err != nil {
		t.Fatal(err)
	}
	address := route.LocalAddr().(*net.UDPAddr).IP
	_ = route.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort(address.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{address}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}
