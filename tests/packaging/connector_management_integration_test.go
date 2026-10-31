//go:build integration

package packaging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/connector"
	"agentrally/operator/internal/controller/agentgroup"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/sandbox"
	"agentrally/operator/internal/workspace"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Exercise the outbound protocol with the rendered Operator roles, not envtest's
// administrator identity. No workload or upstream runtime controller is needed.
func verifyConnectorManagement(t *testing.T, adminConfig *rest.Config, scheme *runtime.Scheme, admin client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	objects := renderObjects(t, enabledOverrides...)
	for _, object := range []client.Object{&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}} {
		name := "ClusterRole/agentrally-operator-agentrally-system-connector"
		if _, ok := object.(*rbacv1.ClusterRoleBinding); ok {
			name = "ClusterRoleBinding/agentrally-operator-agentrally-system-connector"
		}
		if err := yaml.Unmarshal(objects[name], object); err != nil {
			t.Fatal(err)
		}
		if err := admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	config := rest.CopyConfig(adminConfig)
	config.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:agentrally-system:agentrally-operator",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:agentrally-system", "system:authenticated"}}
	var secretDeletes, configMapDeletes atomic.Int32
	config.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return managementRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/secrets/") {
				secretDeletes.Add(1)
			}
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/configmaps/") {
				configMapDeletes.Add(1)
			}
			return next.RoundTrip(r)
		})
	})
	kube, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := connector.NewMetadataClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := kube.Resource(v1.GroupVersion.WithResource("agentgroups")).Get(ctx, "connector-team", metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}); err != nil {
		t.Fatal("rendered Operator RBAC did not become available:", err)
	}
	commands := make(chan connector.Request)
	responses := make(chan connector.Response, 1)
	var ackAttempts, deleteACKAttempts, configMapACKAttempts, configMapDeleteACKAttempts atomic.Int32
	var responseMu sync.Mutex
	firstResponses := map[string][]byte{}
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-management-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/connectors/test-cluster/requests":
			select {
			case request := <-commands:
				_ = json.NewEncoder(w).Encode(request)
			case <-r.Context().Done():
			}
		case "POST /internal/connectors/test-cluster/responses":
			data, err := io.ReadAll(io.LimitReader(r.Body, connector.MaxMessageBytes+1))
			var response connector.Response
			if err != nil || json.Unmarshal(data, &response) != nil {
				t.Error("invalid management response")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var attempts *atomic.Int32
			switch response.ID {
			case "create-secret":
				attempts = &ackAttempts
			case "delete-secret":
				attempts = &deleteACKAttempts
			case "create-configmap":
				attempts = &configMapACKAttempts
			case "delete-configmap":
				attempts = &configMapDeleteACKAttempts
			}
			if attempts != nil {
				responseMu.Lock()
				if attempts.Add(1) == 1 {
					firstResponses[response.ID] = append([]byte(nil), data...)
					responseMu.Unlock()
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if !bytes.Equal(firstResponses[response.ID], data) {
					t.Error("response retry changed the executed write result")
				}
				responseMu.Unlock()
			}
			select {
			case responses <- response:
			case <-ctx.Done():
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()
	directory := t.TempDir()
	token, ca := filepath.Join(directory, "token"), filepath.Join(directory, "ca.crt")
	if err := os.WriteFile(token, []byte("local-management-test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	relay, err := connector.NewClient(connector.Config{Endpoint: backend.URL, ID: "test-cluster", TokenFile: token, CAFile: ca}, connector.Executor{Client: kube, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- relay.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("management relay did not stop")
		}
	}()
	send := func(request connector.Request, want int) connector.Response {
		t.Helper()
		select {
		case commands <- request:
		case <-ctx.Done():
			t.Fatal("management delivery timed out")
		}
		select {
		case response := <-responses:
			if response.ID != request.ID || response.Status != want {
				t.Fatalf("%s: status=%d, want=%d: %s", request.ID, response.Status, want, response.Body)
			}
			return response
		case <-ctx.Done():
			t.Fatal("management result timed out")
			return connector.Response{}
		}
	}
	groupRequest := connector.Request{ID: "create-group", Method: http.MethodPost, Resource: "agentgroups", Scope: &connector.RequestScope{GroupName: "connector-team"},
		Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"name":"connector-team"},"spec":{"persistentWorkspaces":true,"quota":{"hard":{"requests.cpu":"1"}}}}`)}
	createdGroup := send(groupRequest, http.StatusCreated)
	group := &v1.AgentGroup{}
	if err := json.Unmarshal(createdGroup.Body, group); err != nil || group.UID == "" {
		t.Fatal("Group creation did not return its native identity")
	}
	controllerClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := agentgroup.Reconciler{Client: controllerClient, Reader: controllerClient, Scheme: scheme}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err != nil {
		t.Fatal("Group namespace reconciliation:", err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(group), group); err != nil {
		t.Fatal(err)
	}
	inventory := send(connector.Request{ID: "group-inventory", Method: http.MethodGet, Resource: "agentgroups", Limit: 500}, http.StatusOK)
	if strings.Contains(string(inventory.Body), `"spec"`) || !strings.Contains(string(inventory.Body), string(group.UID)) {
		t.Fatal("Group inventory exposed configuration instead of identities")
	}
	scope := &connector.RequestScope{GroupName: group.Name, GroupUID: string(group.UID)}
	identity := send(connector.Request{ID: "namespace", Method: http.MethodGet, Resource: "namespaces", Scope: scope}, http.StatusOK)
	var namespace metav1.PartialObjectMetadata
	if json.Unmarshal(identity.Body, &namespace) != nil || namespace.UID == "" {
		t.Fatal("namespace identity discovery failed")
	}
	scope.NamespaceUID = string(namespace.UID)
	secretRequest := connector.Request{ID: "create-secret", Method: http.MethodPost, Resource: "secrets", Namespace: namespace.Name, Scope: scope,
		Body: json.RawMessage(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"` + namespace.Name + `"},"stringData":{"key":"sensitive-management-marker"}}`)}
	secretResponse := send(secretRequest, http.StatusCreated)
	if ackAttempts.Load() != 2 || strings.Contains(string(secretResponse.Body), "sensitive-management-marker") || strings.Contains(string(secretResponse.Body), `"keys"`) || strings.Contains(string(secretResponse.Body), `"stringData"`) || strings.Contains(string(secretResponse.Body), `"type"`) {
		t.Fatal("Secret ACK retry replayed a write or leaked content")
	}
	var secretIdentity metav1.PartialObjectMetadata
	if json.Unmarshal(secretResponse.Body, &secretIdentity) != nil || secretIdentity.Kind != "PartialObjectMetadata" || secretIdentity.APIVersion != "meta.k8s.io/v1" || len(secretIdentity.Annotations) != 1 || secretIdentity.Annotations["arally.io/connector-operation-id"] != secretRequest.ID {
		t.Fatal("Secret result cannot be correlated")
	}
	duplicateSecret := secretRequest
	duplicateSecret.ID = "duplicate-secret"
	send(duplicateSecret, http.StatusConflict)
	for _, name := range []string{"credentials", ""} {
		response := send(connector.Request{ID: "secret-read-" + name, Method: http.MethodGet, Resource: "secrets", Namespace: namespace.Name, Name: name, Scope: scope, Limit: 0}, http.StatusOK)
		if strings.Contains(string(response.Body), "sensitive-management-marker") || strings.Contains(string(response.Body), `"data"`) || strings.Contains(string(response.Body), `"type"`) {
			t.Fatal("Secret discovery returned content fields")
		}
	}
	verifyRegistrySecrets(t, ctx, admin, namespace.Name, scope, send)
	verifyConfigMapCreation(t, ctx, admin, secretRequest, send)
	if configMapACKAttempts.Load() != 2 {
		t.Fatal("ConfigMap creation ACK failure did not retry the same result")
	}
	definitionRequest := connector.Request{ID: "create-definition", Method: http.MethodPost, Resource: "agentdefinitions", Namespace: namespace.Name, Scope: scope,
		Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentDefinition","metadata":{"name":"agent","namespace":"` + namespace.Name + `"},"spec":{"agentGroupRef":{"name":"connector-team"},"template":"generic","container":{"image":"example/agent","env":[{"name":"API_KEY","valueFrom":{"secretKeyRef":{"name":"credentials","key":"key"}}},{"name":"MODE","valueFrom":{"configMapKeyRef":{"name":"settings","key":"mode"}}}]}}}`)}
	definitionResponse := send(definitionRequest, http.StatusCreated)
	definitionObject := &v1.AgentDefinition{}
	if json.Unmarshal(definitionResponse.Body, definitionObject) != nil || definitionObject.Spec.Container == nil || len(definitionObject.Spec.Container.Env) != 2 || definitionObject.Spec.Container.Env[0].ValueFrom == nil || definitionObject.Spec.Container.Env[0].ValueFrom.SecretKeyRef == nil || definitionObject.Spec.Container.Env[0].ValueFrom.SecretKeyRef.Name != "credentials" || definitionObject.Spec.Container.Env[1].ValueFrom == nil || definitionObject.Spec.Container.Env[1].ValueFrom.ConfigMapKeyRef == nil || definitionObject.Spec.Container.Env[1].ValueFrom.ConfigMapKeyRef.Name != "settings" {
		t.Fatal("definition creation changed native credential references")
	}
	unknownField := definitionRequest
	var invalidDefinition unstructured.Unstructured
	if err := json.Unmarshal(definitionRequest.Body, &invalidDefinition); err != nil {
		t.Fatal(err)
	}
	invalidDefinition.SetName("invalid-definition")
	if err := unstructured.SetNestedField(invalidDefinition.Object, true, "spec", "unexpectedField"); err != nil {
		t.Fatal(err)
	}
	unknownField.ID = "unknown-native-field"
	unknownField.Body, err = json.Marshal(&invalidDefinition)
	if err != nil {
		t.Fatal(err)
	}
	send(unknownField, http.StatusBadRequest)
	instanceRequest := connector.Request{ID: "create-instance", Method: http.MethodPost, Resource: "agentinstances", Namespace: namespace.Name, Scope: scope,
		Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstance","metadata":{"name":"agent","namespace":"` + namespace.Name + `"},"spec":{"agentGroupRef":{"name":"connector-team"},"definitionRef":{"name":"agent"},"workspace":{"mountPath":"/workspace","persistence":{"retentionSeconds":3600},"volumeClaimTemplate":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}}},"status":{"sandboxName":"forged"}}`)}
	instanceResponse := send(instanceRequest, http.StatusCreated)
	instance := &v1.AgentInstance{}
	if json.Unmarshal(instanceResponse.Body, instance) != nil || instance.UID == "" || instance.Status.SandboxName != "" {
		t.Fatal("instance lost identity or accepted controller status")
	}
	verifyConnectorDiagnostics(t, ctx, admin, instanceRequest, send)
	invalid := instanceRequest
	wrongScope := *scope
	wrongScope.NamespaceUID = "different-namespace"
	invalid.ID, invalid.Scope = "stale-namespace", &wrongScope
	send(invalid, http.StatusForbidden)
	// Prepare a real persistent record/PVC through the existing workspace owner.
	template := sandbox.ExtensionObject("SandboxTemplate")
	template.SetName("management-fixture")
	template.SetUID(types.UID("00000000-0000-4000-8000-000000000003"))
	revision := &definition.Revision{ID: strings.Repeat("a", 64), DefinitionUID: definitionObject.UID, Template: template}
	if err := workspace.Ensure(ctx, controllerClient, controllerClient, scheme, instance, group, revision); err != nil {
		t.Fatal("workspace creation:", err)
	}
	secondRequest := instanceRequest
	var secondBody unstructured.Unstructured
	if err := json.Unmarshal(instanceRequest.Body, &secondBody); err != nil {
		t.Fatal(err)
	}
	secondBody.SetName("second-agent")
	secondRequest.ID = "create-second-instance"
	secondRequest.Body, err = json.Marshal(&secondBody)
	if err != nil {
		t.Fatal(err)
	}
	secondResponse := send(secondRequest, http.StatusCreated)
	second := &v1.AgentInstance{}
	if err := json.Unmarshal(secondResponse.Body, second); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Ensure(ctx, controllerClient, controllerClient, scheme, second, group, revision); err != nil {
		t.Fatal("second workspace creation:", err)
	}
	// A retained record from another Group must not enter this Group's pages.
	foreign := &v1.AgentWorkspace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: instance.Status.Workspace.WorkspaceName}, foreign); err != nil {
		t.Fatal(err)
	}
	foreign.ObjectMeta = metav1.ObjectMeta{Name: "foreign-workspace", Labels: map[string]string{workspace.GroupUIDLabel: "another-group", workspace.NamespaceUIDLabel: "another-namespace"}}
	foreign.Spec.GroupName, foreign.Spec.GroupUID = "other-team", "another-group"
	foreign.Spec.Namespace, foreign.Spec.NamespaceUID = "agentrally-other-team", "another-namespace"
	foreign.Spec.InstanceName, foreign.Spec.InstanceUID = "other-agent", "another-instance"
	foreign.Status = v1.AgentWorkspaceStatus{}
	if err := admin.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for _, query := range []connector.Request{
		{ID: "pvcs", Method: http.MethodGet, Resource: "persistentvolumeclaims", Namespace: namespace.Name, Scope: scope, Limit: 1},
		{ID: "workspaces", Method: http.MethodGet, Resource: "agentworkspaces", Scope: scope, Limit: 1},
	} {
		seen := map[string]bool{}
		baseID := query.ID
		pages := 0
		for {
			query.ID = fmt.Sprintf("%s-%d", baseID, pages)
			response := send(query, http.StatusOK)
			var list unstructured.UnstructuredList
			if json.Unmarshal(response.Body, &list) != nil || len(list.Items) > 1 || strings.Contains(string(response.Body), "remainingItemCount") || strings.Contains(string(response.Body), foreign.Name) {
				t.Fatalf("storage page escaped scope or limit: %s", response.Body)
			}
			for _, item := range list.Items {
				if seen[item.GetName()] {
					t.Fatal("storage pagination repeated a record")
				}
				seen[item.GetName()] = true
			}
			pages++
			query.Continue = list.GetContinue()
			if query.Continue == "" {
				break
			}
			if pages > 5 {
				t.Fatal("storage pagination did not terminate")
			}
		}
		if len(seen) != 2 || pages < 2 {
			t.Fatalf("storage pagination omitted records or continuation: %s, records=%d pages=%d", query.Resource, len(seen), pages)
		}
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
	read := connector.Request{ID: "instance-read", Method: http.MethodGet, Resource: "agentinstances", Name: instance.Name, Namespace: namespace.Name, Scope: scope}
	current := send(read, http.StatusOK)
	object := &unstructured.Unstructured{}
	if json.Unmarshal(current.Body, object) != nil {
		t.Fatal("cannot read created instance")
	}
	if err := unstructured.SetNestedField(object.Object, "Suspended", "spec", "operatingMode"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(object)
	update := read
	update.ID, update.Method, update.Body = "suspend", http.MethodPut, body
	send(update, http.StatusOK)
	update.ID = "stale-update"
	send(update, http.StatusConflict)
	read.ID = "updated-instance"
	current = send(read, http.StatusOK)
	if json.Unmarshal(current.Body, object) != nil {
		t.Fatal("cannot read updated instance")
	}
	uid, rv := object.GetUID(), object.GetResourceVersion()
	options, _ := json.Marshal(metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
	deletion := read
	deletion.ID, deletion.Method, deletion.Body = "delete-instance", http.MethodDelete, options
	send(deletion, http.StatusAccepted)
	verifyConfigMapDeletion(t, ctx, admin, config, secretRequest, send)
	if configMapDeleteACKAttempts.Load() != 2 || configMapDeletes.Load() != 3 {
		t.Fatalf("ConfigMap deletion replayed or bypassed guards: ACKs=%d, deletes=%d", configMapDeleteACKAttempts.Load(), configMapDeletes.Load())
	}
	verifySecretDeletion(t, ctx, admin, config, secretRequest, send)
	if deleteACKAttempts.Load() != 2 || secretDeletes.Load() != 3 {
		t.Fatalf("Secret deletion replayed or a rejected request reached Kubernetes: ACKs=%d, deletes=%d", deleteACKAttempts.Load(), secretDeletes.Load())
	}
}
