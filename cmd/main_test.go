package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type apiTransport func(*http.Request) (*http.Response, error)

func (transport apiTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestManagerClientDoesNotCacheSecrets(t *testing.T) {
	var requests atomic.Int32
	httpClient := &http.Client{Transport: apiTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/agentrally-demo/secrets/credentials" {
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"agentrally-demo"},"data":{"token":"dGVzdA=="}}`))}, nil
	})}
	cfg := &rest.Config{Host: "http://api.example.test"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, sandbox.ExtensionsGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	mapper.Add(sandbox.ExtensionsGroupVersion.WithKind("SandboxTemplate"), meta.RESTScopeNamespace)
	options := managerOptions("0", "0", "agentrally-system", false).Client
	options.Scheme, options.Mapper, options.HTTPClient = scheme, mapper, httpClient
	cached, err := cache.New(cfg, cache.Options{Scheme: scheme, Mapper: mapper, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	options.Cache.Reader = cached
	k8s, err := client.New(cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	// Without DisableFor, this read would require a running Secret informer.
	secret := &corev1.Secret{}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "agentrally-demo", Name: "credentials"}, secret); err != nil || string(secret.Data["token"]) != "test" {
		t.Fatalf("reference GET failed without an informer: %v", err)
	}
	// Native template index lookups must use the cache, never an API list.
	err = k8s.List(context.Background(), sandbox.ExtensionList("SandboxTemplate"), client.InNamespace("agentrally-demo"))
	var notStarted *cache.ErrCacheNotStarted
	if !errors.As(err, &notStarted) || requests.Load() != 1 {
		t.Fatalf("unexpected cache or API use: %v (%d requests)", err, requests.Load())
	}
}

func TestManagerControllerDeadlineReachesReconciler(t *testing.T) {
	options := controller.Options{Reconciler: reconcile.Func(func(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("controller did not bound the reconciliation context")
		}
		if request.Name == "blocked" {
			<-ctx.Done()
			return ctrl.Result{}, ctx.Err()
		}
		return ctrl.Result{}, nil
	})}
	options.DefaultFromConfig(managerOptions("0", "0", "agentrally-system", false).Controller)
	worker, err := controller.NewUnmanaged("deadline-regression", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = worker.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "blocked"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked reconciliation ignored caller deadline: %v", err)
	}
}
