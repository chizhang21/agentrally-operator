//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agentrally/operator/internal/connector"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type managementRoundTripFunc func(*http.Request) (*http.Response, error)

func (f managementRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func verifyRegistrySecrets(t *testing.T, namespace string, scope *connector.RequestScope, send func(connector.Request, int) connector.Response) {
	t.Helper()
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "registry-string-data", Namespace: namespace}, Type: corev1.SecretTypeDockerConfigJson,
		StringData: map[string]string{corev1.DockerConfigJsonKey: `{"auths":{"registry.example":{"auth":"sensitive-registry-marker"}}}`}}
	body, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	response := send(connector.Request{ID: secret.Name, Method: http.MethodPost, Resource: "secrets", Namespace: namespace, Scope: scope, Body: body}, http.StatusCreated)
	for _, private := range []string{"sensitive-registry-marker", `"data"`, `"stringData"`, `"type"`, corev1.DockerConfigJsonKey} {
		if strings.Contains(string(response.Body), private) {
			t.Fatal("Secret response exposed credential fields")
		}
	}
}

func verifySecretDeletion(t *testing.T, ctx context.Context, admin client.Client, config *rest.Config, base connector.Request, send func(connector.Request, int) connector.Response) {
	t.Helper()
	request := func(secret *corev1.Secret, id string) connector.Request {
		req := base
		req.ID, req.Method, req.Name = id, http.MethodDelete, secret.Name
		body, err := json.Marshal(metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion}})
		if err != nil {
			t.Fatal(err)
		}
		req.Body = body
		return req
	}
	target := &corev1.Secret{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: base.Namespace, Name: "registry-string-data"}, target); err != nil {
		t.Fatal(err)
	}
	// Keep the resource present while the namespace enters termination.
	target.Finalizers = []string{"example.com/test-hold"}
	if err := admin.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Get(context.Background(), client.ObjectKeyFromObject(target), target); err != nil {
			t.Error(err)
			return
		}
		target.Finalizers = nil
		if err := admin.Update(context.Background(), target); err != nil {
			t.Error(err)
		}
	})
	for _, scenario := range []string{"stale-uid", "stale-rv", "wrong-group", "wrong-namespace"} {
		copy := target.DeepCopy()
		if scenario == "stale-uid" {
			copy.UID = "old-secret"
		} else if scenario == "stale-rv" {
			copy.ResourceVersion = "0"
		}
		req := request(copy, scenario)
		want := http.StatusConflict
		if strings.HasPrefix(scenario, "wrong-") {
			scope := *base.Scope
			if scenario == "wrong-group" {
				scope.GroupUID = "old-group"
			} else {
				scope.NamespaceUID = "old-namespace"
			}
			req.Scope, want = &scope, http.StatusForbidden
		}
		send(req, want)
	}
	response := send(request(target, "delete-secret"), http.StatusAccepted)
	var status metav1.Status
	if json.Unmarshal(response.Body, &status) != nil || status.Kind != "Status" || status.Status != metav1.StatusSuccess || status.Code != 202 || strings.Contains(string(response.Body), "sensitive-registry-marker") {
		t.Fatal("Secret deletion exposed values or lost its generic accepted Status")
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil || target.DeletionTimestamp == nil || len(target.Finalizers) == 0 {
		t.Fatalf("accepted deletion did not preserve finalizer processing: %v", err)
	}
	external := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "external-secret", Namespace: base.Namespace}, Data: map[string][]byte{"key": []byte("sensitive-external-marker")}}
	if err := admin.Create(ctx, external); err != nil {
		t.Fatal(err)
	}
	send(request(external, "delete-external"), http.StatusForbidden)
	if err := admin.Get(ctx, client.ObjectKeyFromObject(external), external); err != nil || external.DeletionTimestamp != nil {
		t.Fatal("external Secret was changed by Connector deletion")
	}
	missing := external.DeepCopy()
	missing.Name = "missing-secret"
	send(request(missing, "delete-missing"), http.StatusNotFound)
	verifyReferenceDeletionRaces(t, ctx, admin, config, base, send)

	authorization, err := authorizationclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"update", "patch", "watch"} {
		review, err := authorization.SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: base.Namespace, Verb: verb, Resource: "secrets"},
		}}, metav1.CreateOptions{})
		if err != nil || review.Status.Allowed {
			t.Fatalf("rendered Operator identity must not have Secret %s: %v", verb, err)
		}
	}
	// The held Secret and ConfigMap keep the real namespace controller waiting.
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: base.Namespace}}
	if err := admin.Delete(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	send(request(external, "delete-in-terminating-namespace"), http.StatusConflict)
}

func verifyReferenceDeletionRaces(t *testing.T, ctx context.Context, admin client.Client, config *rest.Config, base connector.Request, send func(connector.Request, int) connector.Response) {
	t.Helper()
	kind, dataField := "Secret", "stringData"
	if base.Resource == "configmaps" {
		kind, dataField = "ConfigMap", "data"
	}
	metadata, err := connector.NewMetadataClient(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"update", "replace"} {
		name := "race-" + base.Resource + "-" + scenario
		body, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]string{"name": name, "namespace": base.Namespace}, dataField: map[string]string{"key": "sensitive-race-marker"}})
		if err != nil {
			t.Fatal(err)
		}
		create := base
		create.ID, create.Body = name, body
		created := send(create, http.StatusCreated)
		var identity metav1.PartialObjectMetadata
		if err := json.Unmarshal(created.Body, &identity); err != nil {
			t.Fatal(err)
		}
		var injected atomic.Bool
		raceConfig := rest.CopyConfig(config)
		raceConfig.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return managementRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/"+base.Namespace+"/"+base.Resource+"/"+name && injected.CompareAndSwap(false, true) {
					current := &unstructured.Unstructured{}
					current.SetAPIVersion("v1")
					current.SetKind(kind)
					if err := admin.Get(ctx, client.ObjectKey{Namespace: base.Namespace, Name: name}, current); err != nil {
						return nil, err
					}
					if scenario == "update" {
						annotations := current.GetAnnotations()
						annotations["example.com/concurrent-update"] = "changed"
						current.SetAnnotations(annotations)
						if err := admin.Update(ctx, current); err != nil {
							return nil, err
						}
					} else {
						if err := admin.Delete(ctx, current); err != nil {
							return nil, err
						}
						replacement := current.DeepCopy()
						replacement.Object["metadata"] = map[string]any{"name": name, "namespace": base.Namespace}
						if err := admin.Create(ctx, replacement); err != nil {
							return nil, err
						}
					}
				}
				return next.RoundTrip(r)
			})
		})
		kube, err := dynamic.NewForConfig(raceConfig)
		if err != nil {
			t.Fatal(err)
		}
		deletion := base
		deletion.ID, deletion.Method, deletion.Name = "delete-"+name, http.MethodDelete, name
		deletion.Body, err = json.Marshal(metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &identity.UID, ResourceVersion: &identity.ResourceVersion}})
		if err != nil {
			t.Fatal(err)
		}
		response := (connector.Executor{Client: kube, Metadata: metadata}).Execute(ctx, deletion)
		if response.Status != http.StatusConflict || !injected.Load() {
			t.Fatalf("%s race was not rejected by native preconditions: %s", scenario, response.Body)
		}
		current := &unstructured.Unstructured{}
		current.SetAPIVersion("v1")
		current.SetKind(kind)
		if err := admin.Get(ctx, client.ObjectKey{Namespace: base.Namespace, Name: name}, current); err != nil || current.GetDeletionTimestamp() != nil {
			t.Fatalf("%s race deleted the changed Secret: %v", scenario, err)
		}
		if scenario == "update" && (current.GetUID() != identity.UID || current.GetResourceVersion() == identity.ResourceVersion) || scenario == "replace" && current.GetUID() == identity.UID {
			t.Fatal("race fixture did not change the intended identity")
		}
	}
}
