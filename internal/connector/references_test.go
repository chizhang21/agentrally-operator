package connector

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

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
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"kubernetes.io/dockerconfigjson","stringData":{".dockerconfigjson":"{}","extra":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"e30=","extra":"c2Vuc2l0aXZlLW1hcmtlcg=="}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"kubernetes.io/dockerconfigjson","stringData":{"wrong-key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"e30="},"stringData":{".dockerconfigjson":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"kubernetes.io/dockercfg","stringData":{".dockercfg":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"type":"custom.example/credential","stringData":{"key":"sensitive-marker"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-team"},"stringData":{"key":"sensitive-marker"}}` + strings.Repeat(" ", MaxMessageBytes),
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

func TestPVCProjectionAndReferenceReadOnlyAccess(t *testing.T) {
	kube, req := managementFixture(t)
	req.Resource, req.Namespace, req.Name = "persistentvolumeclaims", "agentrally-team", "data"
	pvc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "data", "namespace": req.Namespace, "annotations": map[string]any{"private": "sensitive-marker"}},
		"spec":     map[string]any{"storageClassName": "files", "volumeName": "sensitive-marker", "accessModes": []any{"ReadWriteMany"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}},
		"status":   map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "1Gi"}}}}
	if err := kube.Tracker().Create(resourceAPI(req.Resource), pvc, req.Namespace); err != nil {
		t.Fatal(err)
	}
	response := (Executor{Client: kube}).Execute(t.Context(), req)
	if response.Status != 200 || strings.Contains(string(response.Body), "sensitive-marker") || !strings.Contains(string(response.Body), "ReadWriteMany") || !strings.Contains(string(response.Body), "1Gi") {
		t.Fatalf("PVC projection leaked metadata or lost needed storage fields: %s", response.Body)
	}
	for _, operation := range []struct{ resource, method string }{
		{"persistentvolumeclaims", http.MethodDelete},
		{"secrets", http.MethodPut}, {"secrets", http.MethodPatch},
		{"configmaps", http.MethodPut}, {"configmaps", http.MethodPatch},
	} {
		req.Resource, req.Method = operation.resource, operation.method
		kube.ClearActions()
		response = (Executor{Client: kube}).Execute(t.Context(), req)
		if response.Status != 403 || len(kube.Actions()) != 0 {
			t.Fatal("read-only reference accepted a write")
		}
	}
}

func TestReferenceDeletionRequiresSafeMetadataAndIdentity(t *testing.T) {
	for _, resource := range []string{"secrets", "configmaps"} {
		partial := `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"credentials","namespace":"agentrally-team","uid":"secret-uid","resourceVersion":"42","annotations":{"arally.io/connector-operation-id":"create-1","private":"sensitive-marker"}}}`
		options := `{"preconditions":{"uid":"secret-uid","resourceVersion":"42"}}`
		for _, scenario := range []struct {
			name, metadata, body string
			metadataStatus, want int
		}{
			{"allowed", partial, options, 200, 202},
			{"external", strings.ReplaceAll(partial, "arally.io/connector-operation-id", "external"), options, 200, 403},
			{"invalid marker", strings.ReplaceAll(partial, "create-1", "invalid marker"), options, 200, 403},
			{"stale UID", partial, strings.ReplaceAll(options, "secret-uid", "old-secret"), 200, 409},
			{"stale RV", partial, strings.ReplaceAll(options, "42", "41"), 200, 409},
			{"wrong target", strings.ReplaceAll(partial, "credentials", "different"), options, 200, 502},
			{"wrong namespace", strings.ReplaceAll(partial, "agentrally-team", "different"), options, 200, 502},
			{"missing identity", strings.ReplaceAll(partial, `"resourceVersion":"42"`, `"resourceVersion":""`), options, 200, 502},
			{"full Secret", `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials"},"stringData":{"key":"sensitive-marker"}}`, options, 200, 502},
			{"wrong GVK", strings.ReplaceAll(partial, "meta.k8s.io/v1", "v1"), options, 200, 502},
			{"metadata unavailable", `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotAcceptable","code":406}`, options, 406, 406},
			{"not found", `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"NotFound","code":404}`, options, 404, 404},
			{"metadata forbidden", `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","message":"sensitive-marker","code":403}`, options, 403, 403},
			{"metadata not configured", partial, options, 200, 501},
			{"connection error", "", options, 0, 500},
			{"collection delete", partial, options, 200, 400},
			{"unknown option", partial, strings.TrimSuffix(options, "}") + `,"force":"sensitive-marker"}`, 200, 400},
			{"unknown precondition", partial, `{"preconditions":{"uid":"secret-uid","resourceVersion":"42","extra":"sensitive-marker"}}`, 200, 400},
			{"trailing body", partial, options + `{}`, 200, 400},
			{"missing RV", partial, `{"preconditions":{"uid":"secret-uid"}}`, 200, 400},
			{"wrong options kind", partial, `{"kind":"Secret","preconditions":{"uid":"secret-uid","resourceVersion":"42"}}`, 200, 400},
			{"grace period", partial, strings.TrimSuffix(options, "}") + `,"gracePeriodSeconds":0}`, 200, 400},
			{"dry run", partial, strings.TrimSuffix(options, "}") + `,"dryRun":["All"]}`, 200, 400},
			{"background", partial, strings.TrimSuffix(options, "}") + `,"propagationPolicy":"Background"}`, 200, 400},
			{"oversized", partial, options + strings.Repeat(" ", MaxMessageBytes), 200, 400},
		} {
			t.Run(resource+"/"+scenario.name, func(t *testing.T) {
				kube, req := managementFixture(t)
				req.Resource, req.Method, req.Name, req.Body = resource, http.MethodDelete, "credentials", json.RawMessage(scenario.body)
				if scenario.name == "collection delete" {
					req.Name = ""
				}
				reads, deletes := 0, 0
				metadata, err := NewMetadataClient(&rest.Config{Host: "https://kube.example", Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					reads++
					if scenario.metadataStatus == 0 {
						return nil, errors.New("sensitive-marker connection error")
					}
					if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/agentrally-team/"+resource+"/credentials" || r.Header.Get("Accept") != "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1" {
						t.Fatal("deletion did not use the restricted metadata read")
					}
					response := remoteResponse(r, scenario.metadataStatus, scenario.metadata)
					response.Header.Set("Content-Type", "application/json")
					return response, nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				if scenario.name == "metadata not configured" {
					metadata = nil
				}
				kube.PrependReactor("delete", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
					deletes++
					deletion := action.(ktesting.DeleteAction)
					opts := deletion.GetDeleteOptions()
					if deletion.GetName() != req.Name || deletion.GetNamespace() != req.Namespace || opts.Preconditions == nil || *opts.Preconditions.UID != "secret-uid" || *opts.Preconditions.ResourceVersion != "42" || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
						t.Fatal("deletion lost the verified target or native concurrency guards")
					}
					return true, nil, nil
				})
				response := (Executor{Client: kube, Metadata: metadata}).Execute(t.Context(), req)
				if response.Status != scenario.want || strings.Contains(string(response.Body), "sensitive-marker") || (deletes == 1) != (scenario.want == http.StatusAccepted) {
					t.Fatalf("unsafe deletion or incorrect failure (%d metadata reads, %d deletes): %s", reads, deletes, response.Body)
				}
				localRejection := scenario.want == 400 || scenario.want == 501
				if localRejection && reads != 0 || !localRejection && reads != 1 {
					t.Fatal("invalid request read metadata or metadata read was retried")
				}
				for _, action := range kube.Actions() {
					if action.GetResource().Resource == resource && action.GetVerb() != "delete" {
						t.Fatal("Secret deletion fetched the full object")
					}
				}
				if scenario.want == http.StatusAccepted {
					var status metav1.Status
					if json.Unmarshal(response.Body, &status) != nil || status.Kind != "Status" || status.Status != metav1.StatusSuccess || status.Code != 202 || status.Details != nil {
						t.Fatal("deletion did not return a generic accepted Status")
					}
				}
			})
		}
	}
}

func TestSecretDeletionRejectsTerminatingGroupAndReportsChangedScope(t *testing.T) {
	for _, scenario := range []string{"terminating-group", "changed-after-delete"} {
		t.Run(scenario, func(t *testing.T) {
			kube, req := managementFixture(t)
			req.Resource, req.Method, req.Name = "secrets", http.MethodDelete, "credentials"
			req.Body = json.RawMessage(`{"preconditions":{"uid":"secret-uid","resourceVersion":"42"}}`)
			metadata, err := NewMetadataClient(&rest.Config{Host: "https://kube.example", Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if scenario == "terminating-group" {
					t.Fatal("terminating scope must be rejected before reading target metadata")
				}
				response := remoteResponse(r, 200, `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"credentials","namespace":"agentrally-team","uid":"secret-uid","resourceVersion":"42","annotations":{"arally.io/connector-operation-id":"create-1"}}}`)
				response.Header.Set("Content-Type", "application/json")
				return response, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			group, err := kube.Tracker().Get(groupGVR, "", req.Scope.GroupName)
			if err != nil {
				t.Fatal(err)
			}
			changed := group.(*unstructured.Unstructured).DeepCopy()
			if scenario == "terminating-group" {
				now := metav1.Now()
				changed.SetDeletionTimestamp(&now)
				if err := kube.Tracker().Update(groupGVR, changed, ""); err != nil {
					t.Fatal(err)
				}
			}
			deletes := 0
			kube.PrependReactor("delete", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
				deletes++
				changed.SetUID("replacement-group")
				if err := kube.Tracker().Update(groupGVR, changed, ""); err != nil {
					t.Fatal(err)
				}
				return true, nil, nil
			})
			response := (Executor{Client: kube, Metadata: metadata}).Execute(t.Context(), req)
			if response.Status != http.StatusConflict || (deletes == 1) != (scenario == "changed-after-delete") {
				t.Fatalf("scope guard lost deletion outcome: %s", response.Body)
			}
			if scenario == "changed-after-delete" && !strings.Contains(string(response.Body), "ScopeChanged") {
				t.Fatal("post-delete scope change must not claim rollback")
			}
		})
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
