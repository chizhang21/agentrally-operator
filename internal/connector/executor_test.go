package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

var groupGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "agentgroups"}
var workloadGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "agentinstances"}
var definitionGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "agentdefinitions"}

func fakeKube(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		policyResource: "NetworkPolicyList", endpointResource: "EndpointSliceList",
		groupGVR: "AgentGroupList", definitionGVR: "AgentDefinitionList", workloadGVR: "AgentInstanceList",
		nativeResource("agentworkspaces"): "AgentWorkspaceList", resourceAPI("persistentvolumeclaims"): "PersistentVolumeClaimList",
	}, objects...)
}

func TestDisallowedRequestsNeverReachKubernetes(t *testing.T) {
	for _, req := range []Request{
		{Method: "GET", Resource: "secrets", Namespace: "demo"},
		{Method: "GET", Resource: "pods", Namespace: "demo"},
		{Method: "PUT", Resource: "agentinstances/status", Namespace: "demo"},
		{Method: "PATCH", Resource: "agentgroups", Name: "demo"},
		{Method: "GET", Resource: "agentgroups", Namespace: "demo"},
		{Method: "GET", Resource: "agentinstances"},
		{Method: "GET", Resource: "agentdefinitions"},
		{Method: "POST", Resource: "agentdefinitions", Namespace: "demo", Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstance","metadata":{"name":"agent"}}`)},
		{Method: "GET", Resource: "agentinstances", Namespace: "demo/other"},
		{Method: "GET", Resource: "agentgroups", Name: "../secrets"},
		{Method: "GET", Resource: "agentgroups", Limit: 501},
		{Method: "GET", Resource: "agentgroups", Name: "demo", Continue: "token"},
		{Method: "GET", Resource: "agentgroups", Body: json.RawMessage(`{}`)},
		{Method: "POST", Resource: "clustercapabilities"},
		{Method: "DELETE", Resource: "clustercapabilities", Name: "cluster"},
		{Method: "POST", Resource: "agentgroups", Body: json.RawMessage(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"demo"}}`)},
		{Method: "POST", Resource: "agentgroups", Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"generateName":"demo-"}}`)},
		{Method: "POST", Resource: "agentinstances", Namespace: "demo", Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstance","metadata":{"name":"agent","namespace":"other"}}`)},
		{Method: "PUT", Resource: "agentgroups", Name: "demo", Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"name":"demo"}}`)},
		{Method: "PUT", Resource: "agentgroups", Name: "demo", Body: json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"name":"other","resourceVersion":"1"}}`)},
		{Method: "DELETE", Resource: "agentgroups", Name: "demo", Body: json.RawMessage(`{}`)},
		{Method: "DELETE", Resource: "agentgroups", Name: "demo", Body: json.RawMessage(`{"preconditions":{"uid":"1"}}`)},
		{Method: "DELETE", Resource: "agentgroups", Name: "demo", Body: json.RawMessage(`{"preconditions":{"uid":"1","resourceVersion":"1"},"propagationPolicy":"Orphan"}`)},
	} {
		t.Run(req.Method+"/"+req.Resource+"/"+req.Name+"/"+req.Namespace+string(req.Body), func(t *testing.T) {
			kube := fakeKube()
			response := (Executor{Client: kube}).Execute(context.Background(), req)
			if response.Status != 400 && response.Status != 403 {
				t.Fatalf("unexpected status %d", response.Status)
			}
			if len(kube.Actions()) != 0 {
				t.Fatal("disallowed request reached Kubernetes")
			}
		})
	}
}

func TestNativeAPIErrorClassificationSurvivesWithoutReflectedValues(t *testing.T) {
	for _, apiErr := range []error{
		apierrors.NewConflict(groupGVR.GroupResource(), "demo", errors.New("resourceVersion mismatch")),
		apierrors.NewForbidden(groupGVR.GroupResource(), "demo", errors.New("denied")),
		apierrors.NewResourceExpired("continue token expired"),
		apierrors.NewInvalid(schema.GroupKind{Group: "arally.io", Kind: "AgentGroup"}, "demo", field.ErrorList{
			field.Invalid(field.NewPath("spec", "value"), "sensitive-marker", "rejected sensitive-marker"),
		}),
	} {
		kube := fakeKube()
		kube.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, apiErr })
		response := (Executor{Client: kube}).Execute(context.Background(), Request{ID: "read", Method: "GET", Resource: "agentgroups", Name: "demo", Scope: &RequestScope{GroupName: "demo", GroupUID: "group-id"}})
		var status metav1.Status
		if err := json.Unmarshal(response.Body, &status); err != nil {
			t.Fatal(err)
		}
		want := apiErr.(apierrors.APIStatus).Status()
		if response.ID != "read" || response.Status != int(want.Code) || status.Reason != want.Reason || status.Message == "" || status.Kind != "Status" || status.Details != nil || strings.Contains(string(response.Body), "sensitive-marker") {
			t.Fatal("native API error classification lost or submitted values reflected")
		}
	}
}

func TestPaginationReachesNativeAPI(t *testing.T) {
	// dynamic/fake uses legacy list actions which discard limit and continue.
	// Verify the real dynamic client's HTTP request instead, without a listener.
	for _, limit := range []int64{0, 25} {
		wantLimit := "100"
		if limit != 0 {
			wantLimit = "25"
		}
		calls := 0
		kube, err := dynamic.NewForConfig(&rest.Config{Host: "https://kube.example", Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := ""
			switch req.URL.Path {
			case "/apis/arally.io/v1alpha1/agentgroups/team":
				body = `{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","metadata":{"name":"team","uid":"group-id"},"spec":{}}`
			case "/api/v1/namespaces/agentrally-team":
				body = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"agentrally-team","uid":"namespace-id","ownerReferences":[{"apiVersion":"arally.io/v1alpha1","kind":"AgentGroup","name":"team","uid":"group-id","controller":true}]}}`
			case "/apis/arally.io/v1alpha1/namespaces/agentrally-team/agentinstances":
				calls++
				if req.URL.Query().Get("limit") != wantLimit || req.URL.Query().Get("continue") != "page-2" {
					t.Fatalf("incorrect native list request: %s", req.URL)
				}
				body = `{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstanceList","metadata":{"continue":"page-3"},"items":[]}`
			default:
				t.Fatalf("unexpected request: %s", req.URL)
			}
			response := remoteResponse(req, 200, body)
			response.Header.Set("Content-Type", "application/json")
			return response, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		response := (Executor{Client: kube}).Execute(context.Background(), Request{Method: "GET", Resource: "agentinstances", Namespace: "agentrally-team", Limit: limit, Continue: "page-2", Scope: &RequestScope{GroupName: "team", GroupUID: "group-id", NamespaceUID: "namespace-id"}})
		var list unstructured.UnstructuredList
		if response.Status != 200 || json.Unmarshal(response.Body, &list) != nil || list.GetContinue() != "page-3" || calls != 1 {
			t.Fatal("native pagination result lost")
		}
	}
}
