package agentgroup

import (
	"strings"
	"testing"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGroupNamespaceNamingAndCollision(t *testing.T) {
	for _, name := range []string{"demo", strings.Repeat("a", 56), strings.Repeat("a", 57), strings.Repeat("a", 253), "team.example"} {
		g := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: name}}
		got := g.NamespaceName()
		if !strings.HasPrefix(got, "arally-") || len(got) > 63 || got != g.NamespaceName() {
			t.Fatalf("invalid namespace %q", got)
		}
	}
	dotted := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "team.example"}}
	dashed := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "team-example"}}
	if dotted.NamespaceName() == dashed.NamespaceName() {
		t.Fatal("normalized names collide")
	}
	g := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: "group"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "arally-demo"}}
	c, scheme := testutil.Client(t, g, ns)
	r := &Reconciler{Client: c, Scheme: scheme}
	if _, _, _, err := r.prepare(ctx, g); err == nil {
		t.Fatal("unowned namespace adopted")
	}
}
