package agentgroup

import (
	"context"
	"testing"

	"arally/operator/internal/sandbox"
	"arally/operator/internal/testutil"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPoolSummaryUsesCurrentAPIWithoutStartingAnInformer(t *testing.T) {
	group, ns, _, _, _ := testutil.Fixture()
	pool := sandbox.ExtensionObject("SandboxWarmPool")
	pool.SetName("spares")
	pool.SetNamespace(ns.Name)
	pool.SetGeneration(1)
	pool.Object["spec"] = map[string]interface{}{"replicas": int64(2)}
	pool.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "readyReplicas": int64(1)}
	c, _ := testutil.Client(t, ns, pool)
	cached := interceptor.NewClient(c, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if object, ok := list.(*unstructured.UnstructuredList); ok && object.GetKind() == "SandboxWarmPoolList" {
			t.Fatal("summary tried to start a pool informer without watch permission")
		}
		return c.List(ctx, list, opts...)
	}})
	r := &Reconciler{Client: cached, Reader: c}
	for _, ready := range []int64{1, 2} {
		pool.Object["status"].(map[string]interface{})["readyReplicas"] = ready
		if err := c.Update(context.Background(), pool); err != nil {
			t.Fatal(err)
		}
		summary, err := r.summary(context.Background(), group)
		if err != nil || summary == nil || summary.Pools.Total != 1 || summary.Pools.DesiredSpareReplicas != 2 || summary.Pools.ReadySpareReplicas != int32(ready) {
			t.Fatalf("pool summary did not observe refill changes: %+v, %v", summary, err)
		}
	}
}
