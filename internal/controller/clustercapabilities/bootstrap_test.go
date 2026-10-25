package clustercapabilities

import (
	"context"
	"errors"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDefaultReportCreatedAndRestored(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.ClusterCapabilities{}).Build()
	key := client.ObjectKey{Name: "cluster"}
	request := ctrl.Request{NamespacedName: key}
	r := &Reconciler{Client: base, Reader: base}
	for _, action := range []string{"create", "restore"} {
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		report := &v1.ClusterCapabilities{}
		if err := base.Get(ctx, key, report); err != nil {
			t.Fatalf("%s default report: %v", action, err)
		}
		report.Labels = map[string]string{"user": "keep"}
		if err := base.Update(ctx, report); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		if err := base.Get(ctx, key, report); err != nil || report.Labels["user"] != "keep" {
			t.Fatalf("existing report metadata changed: %v", err)
		}
		if err := base.Delete(ctx, report); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "user-report"}}); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, client.ObjectKey{Name: "user-report"}, &v1.ClusterCapabilities{}); !apierrors.IsNotFound(err) {
		t.Fatal("controller created a user-owned report")
	}
}

func TestDefaultReportCreateFailureRetries(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.ClusterCapabilities{}).Build()
	failCreate := true
	guarded := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1.ClusterCapabilities); !ok || obj.GetName() != "cluster" {
				t.Fatal("bootstrap attempted to create another resource")
			}
			if failCreate {
				return errors.New("temporarily unavailable")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := &Reconciler{Client: guarded, Reader: guarded}
	request := ctrl.Request{NamespacedName: client.ObjectKey{Name: "cluster"}}
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("create failure was swallowed instead of retrying through the work queue")
	}
	failCreate = false
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
}
