package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

func TestNativeWorkloadOperationsPreserveSpecAndUsePreconditions(t *testing.T) {
	kube := fakeKube()
	executor := Executor{Client: kube}
	ctx := context.Background()
	body := json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstance","metadata":{"name":"agent","namespace":"demo"},"spec":{"agentGroupRef":{"name":"demo"},"definitionRef":{"name":"agent-generic"}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	create := executor.Execute(ctx, Request{ID: "create", Method: "POST", Resource: "agentinstances", Namespace: "demo", Body: body})
	if create.ID != "create" || create.Status != http.StatusCreated {
		t.Fatalf("create failed: %s", create.Body)
	}
	var original, created unstructured.Unstructured
	_ = json.Unmarshal(body, &original)
	_ = json.Unmarshal(create.Body, &created)
	if !reflect.DeepEqual(original.Object["spec"], created.Object["spec"]) {
		t.Fatal("relay modified native spec/env/Secret reference")
	}
	if _, present := created.Object["status"]; present {
		t.Fatal("relay attempted to write controller status")
	}
	created.SetResourceVersion("7")
	created.SetUID(types.UID("uid-7"))
	created.Object["spec"].(map[string]any)["operatingMode"] = "Suspended"
	update := executor.Execute(ctx, Request{Method: "PUT", Resource: "agentinstances", Namespace: "demo", Name: "agent", Body: mustJSON(&created)})
	if update.Status != http.StatusOK {
		t.Fatalf("update failed: %s", update.Body)
	}
	get := executor.Execute(ctx, Request{Method: "GET", Resource: "agentinstances", Namespace: "demo", Name: "agent"})
	var read unstructured.Unstructured
	_ = json.Unmarshal(get.Body, &read)
	if get.Status != 200 || read.GetResourceVersion() != "7" || read.Object["spec"].(map[string]any)["operatingMode"] != "Suspended" {
		t.Fatal("update concurrency token or native fields lost")
	}
	list := executor.Execute(ctx, Request{Method: "GET", Resource: "agentinstances", Namespace: "demo", Limit: 25, Continue: "page-2"})
	if list.Status != 200 {
		t.Fatalf("list failed: %s", list.Body)
	}
	deleted := executor.Execute(ctx, Request{Method: "DELETE", Resource: "agentinstances", Namespace: "demo", Name: "agent", Body: json.RawMessage(`{"preconditions":{"uid":"uid-7","resourceVersion":"7"}}`)})
	if deleted.Status != http.StatusAccepted {
		t.Fatalf("delete failed: %s", deleted.Body)
	}
	options := kube.Actions()[len(kube.Actions())-1].(ktesting.DeleteAction).GetDeleteOptions()
	if *options.Preconditions.UID != "uid-7" || *options.Preconditions.ResourceVersion != "7" || *options.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatal("safe delete preconditions missing")
	}
	if executor.Execute(ctx, Request{Method: "GET", Resource: "agentinstances", Namespace: "demo", Name: "agent"}).Status != 404 {
		t.Fatal("native not found error lost")
	}
	for _, action := range kube.Actions() {
		if action.GetResource() != workloadGVR || action.GetNamespace() != "demo" || action.GetSubresource() != "" {
			t.Fatal("operation escaped allowed main resource/namespace")
		}
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
		response := (Executor{Client: kube}).Execute(context.Background(), Request{ID: "read", Method: "GET", Resource: "agentgroups"})
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
			calls++
			if req.URL.Path != "/apis/arally.io/v1alpha1/namespaces/demo/agentinstances" || req.URL.Query().Get("limit") != wantLimit || req.URL.Query().Get("continue") != "page-2" {
				t.Fatalf("incorrect native list request: %s", req.URL)
			}
			response := remoteResponse(req, 200, `{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstanceList","metadata":{"continue":"page-3"},"items":[]}`)
			response.Header.Set("Content-Type", "application/json")
			return response, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		response := (Executor{Client: kube}).Execute(context.Background(), Request{Method: "GET", Resource: "agentinstances", Namespace: "demo", Limit: limit, Continue: "page-2"})
		var list unstructured.UnstructuredList
		if response.Status != 200 || json.Unmarshal(response.Body, &list) != nil || list.GetContinue() != "page-3" || calls != 1 {
			t.Fatal("native pagination result lost")
		}
	}
}

func TestConnectorRelaysDefinitionConfiguration(t *testing.T) {
	kube := fakeKube()
	body := json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentDefinition","metadata":{"name":"assistant","namespace":"demo"},"spec":{"agentGroupRef":{"name":"demo"},"template":"assistant","container":{"image":"example/assistant:v1","env":[{"name":"API_KEY","valueFrom":{"secretKeyRef":{"name":"credentials","key":"key"}}}]}}}`)
	response := (Executor{Client: kube}).Execute(context.Background(), Request{Method: "POST", Resource: "agentdefinitions", Namespace: "demo", Body: body})
	if response.Status != http.StatusCreated {
		t.Fatal(string(response.Body))
	}
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(response.Body, object); err != nil {
		t.Fatal(err)
	}
	original := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, original); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(object.Object["spec"], original.Object["spec"]) || kube.Actions()[0].GetResource() != definitionGVR {
		t.Fatal("definition relay changed credential references or resource routing")
	}
}
