package workspace

import (
	"context"
	"fmt"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/definition"
	"agentrally/operator/internal/sandbox"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPersistentCreationRetryAndReplacementRefusal(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	group := &v1.AgentGroup{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "group"}, Spec: v1.AgentGroupSpec{PersistentWorkspaces: true}, Status: v1.AgentGroupStatus{NamespaceUID: "namespace"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: group.NamespaceName(), UID: "namespace"}}
	instance := &v1.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns.Name, UID: "instance"}, Spec: v1.AgentInstanceSpec{DefinitionRef: v1.AgentDefinitionReference{Name: "definition"}, Workspace: &v1.WorkspaceMount{MountPath: "/workspace", Persistence: &v1.WorkspacePersistence{RetentionSeconds: 3600}, VolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}}}
	template := sandbox.ExtensionObject("SandboxTemplate")
	template.SetName("published")
	template.SetUID("revision")
	revision := &definition.Revision{ID: "hash", DefinitionUID: "definition", Template: template}
	count := 0
	failPin := true
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, ns, instance).WithStatusSubresource(&v1.AgentInstance{}, &v1.AgentWorkspace{}).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			count++
			obj.SetUID(types.UID(fmt.Sprintf("created-%d", count)))
			return c.Create(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, name string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*v1.AgentWorkspace); ok && failPin {
				failPin = false
				return fmt.Errorf("simulated write failure")
			}
			return c.SubResource(name).Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	if err := Ensure(ctx, kube, kube, scheme, instance, group, revision); err == nil {
		t.Fatal("expected crash before PVC UID pin")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(ctx, kube, kube, scheme, instance, group, revision); err != nil {
		t.Fatal(err)
	}
	binding, err := Binding(ctx, kube, instance)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || binding.UID == "" {
		t.Fatal("retry created duplicate storage", count, binding)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(ctx, client.ObjectKey{Name: binding.ClaimName, Namespace: ns.Name}, pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 0 || pvc.Status.Phase == corev1.ClaimBound {
		t.Fatal("PVC should be detached and usable while Pending")
	}
	instance.Status.Workspace = binding
	pod := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: binding.ClaimName}}}}}}
	if err := ValidateVolumes(ctx, kube, instance, pod); err != nil {
		t.Fatal(err)
	}
	unbound := instance.DeepCopy()
	unbound.Status.Workspace = nil
	if err := ValidateVolumes(ctx, kube, unbound, pod); err == nil {
		t.Fatal("raw PVC reference bypassed the workspace binding")
	}
	newer := instance.DeepCopy()
	newer.UID = "replacement"
	newer.Status = v1.AgentInstanceStatus{}
	if err := Ensure(ctx, kube, kube, scheme, newer, group, revision); err == nil {
		t.Fatal("new Instance UID took retained workspace")
	}
	if err := kube.Delete(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(ctx, kube, kube, scheme, instance, group, revision); err == nil {
		t.Fatal("missing pinned PVC was recreated")
	}
}
