package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestCreateRequiresCurrentGroupAndNamespaceIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*Request)
		status int
	}{
		{"allowed", func(*Request) {}, http.StatusCreated},
		{"no scope", func(r *Request) { r.Scope = nil }, http.StatusBadRequest},
		{"different group", func(r *Request) { r.Scope.GroupUID = "another-group" }, http.StatusForbidden},
		{"different namespace", func(r *Request) { r.Scope.NamespaceUID = "another-namespace" }, http.StatusForbidden},
		{"different reference", func(r *Request) { r.Body = json.RawMessage(strings.ReplaceAll(string(r.Body), `"team"`, `"other"`)) }, http.StatusForbidden},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			kube, req := managementFixture(t)
			req.ID, req.Resource, req.Method, req.Name = "create-instance", "agentinstances", http.MethodPost, ""
			req.Body = json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentInstance","metadata":{"name":"new-agent","namespace":"agentrally-team"},"spec":{"agentGroupRef":{"name":"team"},"definitionRef":{"name":"definition"}},"status":{"conditions":[]}}`)
			scenario.change(&req)
			response := (Executor{Client: kube}).Execute(t.Context(), req)
			if response.Status != scenario.status {
				t.Fatalf("status=%d, want=%d: %s", response.Status, scenario.status, response.Body)
			}
			creates := 0
			for _, action := range kube.Actions() {
				if action.GetVerb() != "create" {
					continue
				}
				creates++
				object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
				if object.GetAnnotations()["arally.io/connector-operation-id"] != req.ID || object.Object["status"] != nil {
					t.Fatal("create lost its operation identity or wrote controller status")
				}
			}
			if (creates == 1) != (scenario.status == http.StatusCreated) {
				t.Fatalf("unexpected writes: %d", creates)
			}
		})
	}
}

func TestCreateRejectsTerminatingScopeAndReportsChangedScope(t *testing.T) {
	for _, resource := range []string{"agentgroups", "namespaces", "changed-after-write"} {
		t.Run(resource, func(t *testing.T) {
			kube, req := managementFixture(t)
			req.ID, req.Method, req.Resource, req.Name = "create-definition", http.MethodPost, "agentdefinitions", ""
			req.Body = json.RawMessage(`{"apiVersion":"arally.io/v1alpha1","kind":"AgentDefinition","metadata":{"name":"new-definition","namespace":"agentrally-team"},"spec":{"agentGroupRef":{"name":"team"},"template":"generic","container":{"image":"example/agent"}}}`)
			if resource == "changed-after-write" {
				kube.PrependReactor("create", "agentdefinitions", func(ktesting.Action) (bool, runtime.Object, error) {
					ns, _ := kube.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", req.Namespace)
					copy := ns.(*unstructured.Unstructured).DeepCopy()
					copy.SetUID("replacement")
					if err := kube.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, copy, ""); err != nil {
						t.Fatal(err)
					}
					return false, nil, nil
				})
			} else {
				gvr, name := groupGVR, "team"
				if resource == "namespaces" {
					gvr, name = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, req.Namespace
				}
				object, err := kube.Tracker().Get(gvr, "", name)
				if err != nil {
					t.Fatal(err)
				}
				copy := object.(*unstructured.Unstructured).DeepCopy()
				now := metav1.Now()
				copy.SetDeletionTimestamp(&now)
				if err := kube.Tracker().Update(gvr, copy, ""); err != nil {
					t.Fatal(err)
				}
			}
			response := (Executor{Client: kube}).Execute(t.Context(), req)
			if response.Status != http.StatusConflict {
				t.Fatalf("terminating/changed scope was accepted: %s", response.Body)
			}
			object, err := kube.Resource(definitionGVR).Namespace(req.Namespace).Get(context.Background(), "new-definition", metav1.GetOptions{})
			if resource == "changed-after-write" {
				if err != nil || object == nil || !strings.Contains(string(response.Body), "ScopeChanged") {
					t.Fatal("scope change incorrectly claimed the write was rolled back")
				}
			} else if err == nil {
				t.Fatal("terminating scope received a new object")
			}
		})
	}
}

func TestManagementCannotBypassControllerCleanup(t *testing.T) {
	for _, scenario := range []string{"create finalizer", "create ownership", "remove finalizer", "replace ownership", "update deleting object"} {
		t.Run(scenario, func(t *testing.T) {
			kube, req := managementFixture(t)
			req.Resource, req.Method = "agentinstances", http.MethodPut
			object, err := kube.Resource(workloadGVR).Namespace(req.Namespace).Get(t.Context(), req.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			object.SetFinalizers([]string{"arally.io/agent-instance-cleanup"})
			if scenario == "update deleting object" {
				now := metav1.Now()
				object.SetDeletionTimestamp(&now)
			}
			if err := kube.Tracker().Update(workloadGVR, object, req.Namespace); err != nil {
				t.Fatal(err)
			}
			object = object.DeepCopy()
			switch scenario {
			case "create finalizer", "create ownership":
				req.Method, req.Name = http.MethodPost, ""
				object.Object["metadata"] = map[string]any{"name": "new-agent", "namespace": req.Namespace}
				if scenario == "create ownership" {
					object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "other", UID: "other"}})
				} else {
					object.SetFinalizers([]string{"arally.io/agent-instance-cleanup"})
				}
			case "remove finalizer", "update deleting object":
				object.SetFinalizers(nil)
			case "replace ownership":
				object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "other", UID: "other"}})
			}
			req.Body = mustJSON(object)
			kube.ClearActions()
			response := (Executor{Client: kube}).Execute(t.Context(), req)
			want := http.StatusBadRequest
			if scenario == "update deleting object" {
				want = http.StatusConflict
			}
			if response.Status != want {
				t.Fatalf("management bypassed lifecycle ownership: %s", response.Body)
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() != "get" {
					t.Fatal("lifecycle metadata reached a write")
				}
			}
		})
	}
}
