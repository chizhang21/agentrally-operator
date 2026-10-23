package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/controller/clustercapabilities"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var groupGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "agentgroups"}
var workloadGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "agentinstances"}
var definitionGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "agentdefinitions"}
var capabilityGVR = schema.GroupVersionResource{Group: "arally.io", Version: "v1alpha1", Resource: "clustercapabilities"}

func fakeKube(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		groupGVR: "AgentGroupList", definitionGVR: "AgentDefinitionList", workloadGVR: "AgentInstanceList", capabilityGVR: "ClusterCapabilitiesList",
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

func TestNativeAPIErrorsSurviveRelay(t *testing.T) {
	for _, apiErr := range []error{
		apierrors.NewConflict(groupGVR.GroupResource(), "demo", errors.New("resourceVersion mismatch")),
		apierrors.NewForbidden(groupGVR.GroupResource(), "demo", errors.New("denied")),
		apierrors.NewResourceExpired("continue token expired"),
	} {
		kube := fakeKube()
		kube.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, apiErr })
		response := (Executor{Client: kube}).Execute(context.Background(), Request{ID: "read", Method: "GET", Resource: "agentgroups"})
		var status metav1.Status
		if err := json.Unmarshal(response.Body, &status); err != nil {
			t.Fatal(err)
		}
		want := apiErr.(apierrors.APIStatus).Status()
		if response.ID != "read" || response.Status != int(want.Code) || status.Reason != want.Reason || status.Message != want.Message || status.Kind != "Status" {
			t.Fatal("native API error lost")
		}
	}
}

func TestDiscoveredCapabilitiesReadableAsNativeCR(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1.AddToScheme, corev1.AddToScheme, nodev1.AddToScheme, storagev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	report := &v1.ClusterCapabilities{TypeMeta: metav1.TypeMeta{APIVersion: "arally.io/v1alpha1", Kind: "ClusterCapabilities"}, ObjectMeta: metav1.ObjectMeta{Name: "cluster", Generation: 2}}
	storage := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs"}, Provisioner: "nfs.csi.k8s.io"}
	driver := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: storage.Provisioner}}
	local := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(report).WithObjects(report, storage, driver).Build()
	r := &clustercapabilities.Reconciler{Client: local, Reader: local}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "cluster"}}); err != nil {
		t.Fatal(err)
	}
	if err := local.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
		t.Fatal(err)
	}
	// The controller-runtime fake clears TypeMeta on typed GETs, unlike the API.
	report.SetGroupVersionKind(v1.GroupVersion.WithKind("ClusterCapabilities"))
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(report)
	if err != nil {
		t.Fatal(err)
	}
	// The fake's plural guess incorrectly uses clustercapabilitieses.
	kube := fakeKube()
	if err := kube.Tracker().Create(capabilityGVR, &unstructured.Unstructured{Object: object}, ""); err != nil {
		t.Fatal(err)
	}
	response := (Executor{Client: kube}).Execute(ctx, Request{ID: "capabilities", Method: "GET", Resource: "clustercapabilities", Name: "cluster"})
	if response.Status != 200 {
		t.Fatalf("capability read failed: %s", response.Body)
	}
	var received v1.ClusterCapabilities
	if err := json.Unmarshal(response.Body, &received); err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || !reflect.DeepEqual(report.Status, received.Status) || received.Status.ObservedAt == nil || len(received.Status.StorageClasses) != 1 || received.Status.ObservedGeneration != 2 {
		t.Fatal("discovery status did not survive native CR read")
	}
	if action := kube.Actions()[0]; action.GetVerb() != "get" || action.GetNamespace() != "" || action.GetResource() != capabilityGVR {
		t.Fatal("capability read scope is incorrect")
	}
	list := (Executor{Client: kube}).Execute(ctx, Request{Method: "GET", Resource: "clustercapabilities"})
	var reports v1.ClusterCapabilitiesList
	if json.Unmarshal(list.Body, &reports) != nil || list.Status != 200 || len(reports.Items) != 1 || !reflect.DeepEqual(reports.Items[0].Status, report.Status) {
		t.Fatal("native capability list lost status")
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
