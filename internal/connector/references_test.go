package connector

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"agentrally/operator/internal/workspace"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func TestSecretMetadataNegotiationAndProjection(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		code       int
		nameQuery  string
		want       int
	}{
		{"read", `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"credentials","namespace":"agentrally-team","uid":"secret-uid","annotations":{"private":"sensitive-marker","arally.io/connector-operation-id":"create-1"},"labels":{"private":"sensitive-marker"},"managedFields":[{"manager":"sensitive-marker"}]}}`, 200, "credentials", 200},
		{"list", `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"continue":"next-page","remainingItemCount":99},"items":[{"metadata":{"name":"credentials","namespace":"agentrally-team","annotations":{"private":"sensitive-marker"}}}]}`, 200, "", 200},
		{"full response rejected", `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials"},"stringData":{"key":"sensitive-marker"}}`, 200, "credentials", 502},
		{"no full fallback", `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotAcceptable","code":406}`, 406, "credentials", 406},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			kube, req := managementFixture(t)
			req.Resource, req.Name = "secrets", scenario.nameQuery
			calls := 0
			client, err := NewMetadataClient(&rest.Config{Host: "https://kube.example", Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				kind := "PartialObjectMetadata"
				if req.Name == "" {
					kind += "List"
				}
				if r.Header.Get("Accept") != "application/json;as="+kind+";g=meta.k8s.io;v=v1" {
					t.Fatal("metadata negotiation permits a full-object fallback")
				}
				response := remoteResponse(r, scenario.code, scenario.body)
				response.Header.Set("Content-Type", "application/json")
				return response, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			response := (Executor{Client: kube, Metadata: client}).Execute(t.Context(), req)
			if response.Status != scenario.want || calls != 1 || strings.Contains(string(response.Body), "sensitive-marker") || strings.Contains(string(response.Body), "remainingItemCount") {
				t.Fatalf("incorrect metadata response (%d calls): %s", calls, response.Body)
			}
			if scenario.name == "read" && !strings.Contains(string(response.Body), "create-1") {
				t.Fatal("creation identity was lost")
			}
			if scenario.name == "list" && !strings.Contains(string(response.Body), "next-page") {
				t.Fatal("metadata pagination was lost")
			}
		})
	}
}

func TestSecretCreationRejectsPrivilegeFieldsAndRedactsAPIErrors(t *testing.T) {
	for _, body := range []string{
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team","annotations":{"kubernetes.io/service-account.name":"default"}},"stringData":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team","finalizers":["keep"]},"stringData":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"kubernetes.io/service-account-token","stringData":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"other"},"stringData":{"key":"sensitive-marker"}}`,
	} {
		kube, req := managementFixture(t)
		req.Resource, req.Method, req.Name, req.Body = "secrets", http.MethodPost, "", json.RawMessage(body)
		response := (Executor{Client: kube}).Execute(t.Context(), req)
		if response.Status != 400 || strings.Contains(string(response.Body), "sensitive-marker") {
			t.Fatalf("unsafe Secret body was accepted or reflected: %s", response.Body)
		}
		for _, action := range kube.Actions() {
			if action.GetVerb() != "get" {
				t.Fatal("rejected Secret request produced a write")
			}
		}
	}
	kube, req := managementFixture(t)
	req.Resource, req.Method, req.Name, req.Body = "secrets", http.MethodPost, "", json.RawMessage(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"stringData":{"key":"sensitive-marker"}}`)
	kube.PrependReactor("create", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInvalid(resourceAPI("secrets").GroupVersion().WithKind("Secret").GroupKind(), "credentials", field.ErrorList{field.Invalid(field.NewPath("data"), "sensitive-marker", "https://private.example/sensitive-marker")})
	})
	response := (Executor{Client: kube}).Execute(t.Context(), req)
	if response.Status != 422 || strings.Contains(string(response.Body), "sensitive-marker") || strings.Contains(string(response.Body), "private.example") {
		t.Fatalf("Secret validation failure leaked submitted content: %s", response.Body)
	}
}

func TestStorageQueriesFilterScopeAndPrivateFields(t *testing.T) {
	kube, req := managementFixture(t)
	req.Resource, req.Namespace, req.Name = "agentworkspaces", "", ""
	for name, group := range map[string]string{"own": req.Scope.GroupUID, "foreign": "other-group"} {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "arally.io/v1alpha1", "kind": "AgentWorkspace",
			"metadata": map[string]any{"name": name, "annotations": map[string]any{"private": "sensitive-marker"},
				"labels": map[string]any{workspace.GroupUIDLabel: req.Scope.GroupUID, workspace.NamespaceUIDLabel: req.Scope.NamespaceUID}},
			"spec": map[string]any{"groupName": req.Scope.GroupName, "groupUID": group, "namespace": "agentrally-team", "namespaceUID": req.Scope.NamespaceUID}}}
		if err := kube.Tracker().Create(nativeResource("agentworkspaces"), object, ""); err != nil {
			t.Fatal(err)
		}
	}
	response := (Executor{Client: kube}).Execute(t.Context(), req)
	var list unstructured.UnstructuredList
	if response.Status != 200 || json.Unmarshal(response.Body, &list) != nil || len(list.Items) != 1 || list.Items[0].GetName() != "own" || strings.Contains(string(response.Body), "sensitive-marker") {
		t.Fatalf("workspace query leaked another scope or metadata: %s", response.Body)
	}
	req.Name = "foreign"
	if response := (Executor{Client: kube}).Execute(t.Context(), req); response.Status != 403 {
		t.Fatalf("named workspace escaped its scope: %s", response.Body)
	}
	req.Resource, req.Namespace, req.Name = "persistentvolumeclaims", "agentrally-team", "data"
	pvc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "data", "namespace": req.Namespace, "annotations": map[string]any{"private": "sensitive-marker"}},
		"spec":     map[string]any{"storageClassName": "files", "volumeName": "sensitive-marker", "accessModes": []any{"ReadWriteMany"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}},
		"status":   map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "1Gi"}}}}
	if err := kube.Tracker().Create(resourceAPI(req.Resource), pvc, req.Namespace); err != nil {
		t.Fatal(err)
	}
	response = (Executor{Client: kube}).Execute(t.Context(), req)
	if response.Status != 200 || strings.Contains(string(response.Body), "sensitive-marker") || !strings.Contains(string(response.Body), "ReadWriteMany") || !strings.Contains(string(response.Body), "1Gi") {
		t.Fatalf("PVC projection leaked metadata or lost needed storage fields: %s", response.Body)
	}
	for _, resource := range []string{"agentworkspaces", "persistentvolumeclaims", "secrets"} {
		req.Resource, req.Method = resource, http.MethodDelete
		kube.ClearActions()
		response = (Executor{Client: kube}).Execute(t.Context(), req)
		if response.Status != 403 || len(kube.Actions()) != 0 {
			t.Fatal("read-only reference accepted a write")
		}
	}
}

func TestCreationIdentityCannotBeOverwritten(t *testing.T) {
	kube, req := managementFixture(t)
	req.Resource, req.Method = "agentinstances", http.MethodPut
	object, err := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(t.Context(), req.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	object.SetAnnotations(map[string]string{operationAnnotation: "forged"})
	req.Body = mustJSON(object)
	kube.ClearActions()
	response := (Executor{Client: kube}).Execute(t.Context(), req)
	if response.Status != 400 {
		t.Fatalf("creation identity was overwritten: %s", response.Body)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() == "update" {
			t.Fatal("forged marker reached Kubernetes")
		}
	}
}
