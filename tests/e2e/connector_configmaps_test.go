//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agentrally/operator/internal/connector"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyConfigMapCreation(t *testing.T, base connector.Request, send func(connector.Request, int) connector.Response) {
	t.Helper()
	for _, name := range []string{"empty-settings", "settings"} {
		object := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: base.Namespace}, Immutable: ptr.To(true)}
		request := base
		request.ID, request.Resource = "create-"+name, "configmaps"
		if name == "settings" {
			request.ID = "create-configmap"
			object.Data = map[string]string{"mode": "sensitive-config-marker"}
			object.BinaryData = map[string][]byte{"bytes": {1, 2, 3}}
		}
		var err error
		request.Body, err = json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		response := send(request, http.StatusCreated)
		if strings.Contains(string(response.Body), "sensitive-config-marker") || strings.Contains(string(response.Body), `"data"`) || strings.Contains(string(response.Body), `"binaryData"`) {
			t.Fatal("ConfigMap creation exposed its body")
		}
	}
	query := connector.Request{ID: "read-configmap", Method: http.MethodGet, Resource: "configmaps", Namespace: base.Namespace, Name: "settings", Scope: base.Scope}
	send(query, http.StatusOK)
	query.Name, query.Limit = "", 1
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatal("ConfigMap pagination did not terminate")
		}
		query.ID = fmt.Sprintf("list-configmaps-%d", page)
		response := send(query, http.StatusOK)
		if strings.Contains(string(response.Body), "sensitive-config-marker") || strings.Contains(string(response.Body), `"data"`) || strings.Contains(string(response.Body), `"binaryData"`) {
			t.Fatal("ConfigMap discovery exposed content")
		}
		var list metav1.PartialObjectMetadataList
		if json.Unmarshal(response.Body, &list) != nil || len(list.Items) > 1 {
			t.Fatal("ConfigMap metadata pagination was not preserved")
		}
		for _, item := range list.Items {
			if seen[item.Name] {
				t.Fatal("ConfigMap pagination repeated an object")
			}
			seen[item.Name] = true
		}
		query.Continue = list.Continue
		if query.Continue == "" {
			break
		}
	}
	if !seen["empty-settings"] || !seen["settings"] {
		t.Fatal("ConfigMap pagination omitted a created object")
	}
}

func verifyConfigMapDeletion(t *testing.T, ctx context.Context, admin client.Client, config *rest.Config, base connector.Request, send func(connector.Request, int) connector.Response) {
	t.Helper()
	base.Resource = "configmaps"
	request := func(object *corev1.ConfigMap, id string) connector.Request {
		req := base
		req.ID, req.Method, req.Name = id, http.MethodDelete, object.Name
		var err error
		req.Body, err = json.Marshal(metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &object.UID, ResourceVersion: &object.ResourceVersion}})
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	target := &corev1.ConfigMap{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: base.Namespace, Name: "settings"}, target); err != nil {
		t.Fatal(err)
	}
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
	stale := target.DeepCopy()
	stale.ResourceVersion = "0"
	send(request(stale, "delete-stale-configmap"), http.StatusConflict)
	response := send(request(target, "delete-configmap"), http.StatusAccepted)
	if strings.Contains(string(response.Body), "sensitive-config-marker") || strings.Contains(string(response.Body), `"data"`) {
		t.Fatal("ConfigMap deletion returned its content")
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil || target.DeletionTimestamp == nil || len(target.Finalizers) == 0 {
		t.Fatalf("ConfigMap deletion bypassed finalizer processing: %v", err)
	}
	external := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "external-settings", Namespace: base.Namespace}, Data: map[string]string{"key": "sensitive-config-marker"}}
	if err := admin.Create(ctx, external); err != nil {
		t.Fatal(err)
	}
	send(request(external, "delete-external-configmap"), http.StatusForbidden)
	read := request(external, "read-external-configmap")
	read.Method, read.Body = http.MethodGet, nil
	send(read, http.StatusOK)
	if err := admin.Get(ctx, client.ObjectKeyFromObject(external), external); err != nil || external.DeletionTimestamp != nil {
		t.Fatal("external ConfigMap was modified")
	}
	verifyReferenceDeletionRaces(t, ctx, admin, config, base, send)
	authorization, err := authorizationclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"update", "patch"} {
		review, err := authorization.SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: base.Namespace, Verb: verb, Resource: "configmaps"},
		}}, metav1.CreateOptions{})
		if err != nil || review.Status.Allowed {
			t.Fatalf("Operator must not have ConfigMap %s: %v", verb, err)
		}
	}
}
