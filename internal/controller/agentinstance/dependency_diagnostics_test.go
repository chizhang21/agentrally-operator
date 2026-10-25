package agentinstance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller/agentgroup"
	"agentrally/operator/internal/diagnostics"
	"agentrally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type diagnosticReader struct {
	client.Reader
	calls map[string]int
	err   error
}

func (r *diagnosticReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.calls[fmt.Sprintf("%T/%s", obj, key.Name)]++
	if r.err != nil {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func referenceTestReader(t *testing.T, objects ...client.Object) *diagnosticReader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &diagnosticReader{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), calls: map[string]int{}}
}

func secretReference(name, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{Name: "API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key, Optional: ptr.To(optional)}}}
}

func configReference(name, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{Name: "CONFIG", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key, Optional: ptr.To(optional)}}}
}

func TestNativeReferenceKeyValidation(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "project"}, Data: map[string][]byte{"token": []byte("secret-do-not-expose"), "empty": {}}}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "project"}, Data: map[string]string{"mode": "config-do-not-expose", "empty": ""}, BinaryData: map[string][]byte{"binary": []byte("do-not-expose")}}
	cases := []struct {
		name, reason string
		env          corev1.EnvVar
	}{
		{"secret-present", "", secretReference("credentials", "token", false)},
		{"empty-secret-is-valid", "", secretReference("credentials", "empty", false)},
		{"secret-missing", "DependencyNotFound", secretReference("missing", "token", false)},
		{"optional-secret-missing", "", secretReference("missing", "token", true)},
		{"secret-key-missing", "DependencyKeyMissing", secretReference("credentials", "missing", false)},
		{"optional-secret-key-missing", "", secretReference("credentials", "missing", true)},
		{"secret-name-empty", "InvalidConfiguration", secretReference("", "token", false)},
		{"secret-key-empty", "InvalidConfiguration", secretReference("credentials", "", true)},
		{"config-present", "", configReference("settings", "mode", false)},
		{"empty-config-is-valid", "", configReference("settings", "empty", false)},
		{"config-missing", "DependencyNotFound", configReference("missing", "mode", false)},
		{"config-key-missing", "DependencyKeyMissing", configReference("settings", "missing", false)},
		{"binary-not-env-value", "DependencyKeyMissing", configReference("settings", "binary", false)},
		{"optional-config-missing", "", configReference("missing", "mode", true)},
		{"optional-config-key-missing", "", configReference("settings", "missing", true)},
		{"literal-value", "", corev1.EnvVar{Name: "LITERAL", Value: "literal-do-not-expose"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := referenceTestReader(t, secret, config)
			template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Env: []corev1.EnvVar{tc.env}}}}}
			before := template.DeepCopy()
			err := validateNativeReferences(context.Background(), reader, "project", template)
			reason, message := diagnostics.Classify(err)
			if reason != tc.reason {
				t.Fatalf("reason=%s err=%v", reason, err)
			}
			if strings.Contains(message, "do-not-expose") || !reflect.DeepEqual(before, template) {
				t.Fatal("exposed value or mutated input")
			}
			if tc.reason == "DependencyNotFound" && !apierrors.IsNotFound(err) {
				t.Fatal("NotFound cause lost")
			}
		})
	}
}

func TestNativeReferenceEveryReferencePath(t *testing.T) {
	paths := map[string]func(*corev1.PodTemplateSpec){
		"init-secret-env": func(p *corev1.PodTemplateSpec) {
			p.Spec.InitContainers = []corev1.Container{{Env: []corev1.EnvVar{secretReference("missing", "key", false)}}}
		},
		"init-config-env": func(p *corev1.PodTemplateSpec) {
			p.Spec.InitContainers = []corev1.Container{{Env: []corev1.EnvVar{configReference("missing", "key", false)}}}
		},
		"sidecar-secret-env": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Env: []corev1.EnvVar{secretReference("missing", "key", false)}})
		},
		"secret-envfrom": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}}}}
		},
		"config-envfrom": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}}}}
		},
		"init-envfrom": func(p *corev1.PodTemplateSpec) {
			p.Spec.InitContainers = []corev1.Container{{EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}}}}}}
		},
		"secret-volume": func(p *corev1.PodTemplateSpec) {
			volume := corev1.Volume{Name: "mount"}
			volume.Secret = &corev1.SecretVolumeSource{SecretName: "missing"}
			p.Spec.Volumes = []corev1.Volume{volume}
		},
		"config-volume": func(p *corev1.PodTemplateSpec) {
			volume := corev1.Volume{Name: "mount"}
			volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}}
			p.Spec.Volumes = []corev1.Volume{volume}
		},
		"projected-secret": func(p *corev1.PodTemplateSpec) {
			volume := corev1.Volume{Name: "mount"}
			source := corev1.VolumeProjection{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}}}
			volume.Projected = &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{source}}
			p.Spec.Volumes = []corev1.Volume{volume}
		},
		"projected-config": func(p *corev1.PodTemplateSpec) {
			volume := corev1.Volume{Name: "mount"}
			source := corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}}}
			volume.Projected = &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{source}}
			p.Spec.Volumes = []corev1.Volume{volume}
		},
		"pull-secret": func(p *corev1.PodTemplateSpec) {
			p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "missing"}}
		},
	}
	for name, configure := range paths {
		t.Run(name, func(t *testing.T) {
			template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}}
			configure(template)
			err := validateNativeReferences(context.Background(), referenceTestReader(t), "project", template)
			if reason, _ := diagnostics.Classify(err); reason != "DependencyNotFound" {
				t.Fatalf("reference path not checked: %v", err)
			}
		})
	}
}

func TestNativeReferenceVolumeKeysAndOptionalSources(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "project"}, Data: map[string][]byte{"key": {}}}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "project"}, BinaryData: map[string][]byte{"key": {}}}
	for _, kind := range []string{"Secret", "ConfigMap"} {
		for _, projected := range []bool{false, true} {
			for _, optional := range []bool{false, true} {
				for _, key := range []string{"key", "missing"} {
					t.Run(fmt.Sprintf("%s/projected=%v/optional=%v/key=%s", kind, projected, optional, key), func(t *testing.T) {
						items := []corev1.KeyToPath{{Key: key, Path: "data"}}
						volume := corev1.Volume{Name: "mount"}
						if projected {
							source := corev1.VolumeProjection{}
							if kind == "Secret" {
								source.Secret = &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "data"}, Items: items, Optional: ptr.To(optional)}
							} else {
								source.ConfigMap = &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "data"}, Items: items, Optional: ptr.To(optional)}
							}
							volume.Projected = &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{source}}
						} else if kind == "Secret" {
							volume.Secret = &corev1.SecretVolumeSource{SecretName: "data", Items: items, Optional: ptr.To(optional)}
						} else {
							volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "data"}, Items: items, Optional: ptr.To(optional)}
						}
						p := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{volume}}}
						err := validateNativeReferences(context.Background(), referenceTestReader(t, secret, config), "project", p)
						reason, _ := diagnostics.Classify(err)
						if key == "missing" && !optional {
							if reason != "DependencyKeyMissing" {
								t.Fatalf("missing key accepted: %v", err)
							}
						} else if err != nil {
							t.Fatal(err)
						}
					})
				}
			}
		}
	}
	for _, source := range []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Optional: ptr.To(true)}}, {ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Optional: ptr.To(true)}}} {
		p := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{EnvFrom: []corev1.EnvFromSource{source}}}}}
		if err := validateNativeReferences(context.Background(), referenceTestReader(t), "project", p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeReferenceTerminatingDedupAndReaderErrors(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "project"}, Data: map[string][]byte{"key": {}}}
	p := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{secretReference("credentials", "key", false), secretReference("credentials", "key", false)}, EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}}}}}}, ImagePullSecrets: []corev1.LocalObjectReference{{Name: "credentials"}}}}
	reader := referenceTestReader(t, secret)
	if err := validateNativeReferences(context.Background(), reader, "project", p); err != nil {
		t.Fatal(err)
	}
	if reader.calls["*v1.Secret/credentials"] != 1 {
		t.Fatalf("duplicate API reads: %v", reader.calls)
	}
	now := metav1.Now()
	secret.DeletionTimestamp, secret.Finalizers = &now, []string{"fixture"}
	err := validateNativeReferences(context.Background(), referenceTestReader(t, secret), "project", p)
	if reason, _ := diagnostics.Classify(err); reason != "DependencyTerminating" {
		t.Fatalf("terminating reference accepted: %v", err)
	}
	for _, original := range []error{apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "credentials", errors.New("private-server-detail")), context.DeadlineExceeded, errors.New("private-server-detail")} {
		reader = referenceTestReader(t)
		reader.err = original
		err = validateNativeReferences(context.Background(), reader, "project", p)
		_, message := diagnostics.Classify(fmt.Errorf("wrapper: %w", err))
		if !errors.Is(err, original) || strings.Contains(message, "private-server-detail") || strings.Contains(err.Error(), "private-server-detail") {
			t.Fatalf("unsafe error or cause lost: %v", err)
		}
	}
	reader = referenceTestReader(t)
	optional := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{secretReference("missing", "key", true), secretReference("missing", "key", false)}}}}}
	err = validateNativeReferences(context.Background(), reader, "project", optional)
	if !apierrors.IsNotFound(err) || reader.calls["*v1.Secret/missing"] != 1 {
		t.Fatalf("optional first lookup hid required dependency or reread it: %v %v", err, reader.calls)
	}
}

func TestReconciliationErrorsAreSafeAndPreserveCauses(t *testing.T) {
	original := errors.New("private-server-detail: https://internal.example/admission")
	g, ns, definition, w, source := testutil.Fixture()
	c, scheme := clientWithPublishedDefinitions(t, definition, g, ns, w, nativeTemplate(definition, source))
	guarded := interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if _, ok := object.(*corev1.Namespace); ok {
			return original
		}
		return c.Get(ctx, key, object, opts...)
	}})
	_, err := (&agentgroup.Reconciler{Client: guarded, Scheme: scheme}).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(g)})
	if !errors.Is(err, original) || strings.Contains(err.Error(), "private-server-detail") {
		t.Fatal("group error lost its cause or exposed private detail")
	}
	current := &v1.AgentGroup{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(g), current); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(current.Status)
	if strings.Contains(string(encoded), "private-server-detail") {
		t.Fatal("group status exposed private detail")
	}
	_, err = (&Reconciler{Client: c, Scheme: scheme}).failed(ctx, w, original)
	if !errors.Is(err, original) || strings.Contains(err.Error(), "private-server-detail") {
		t.Fatal("claim error lost its cause or exposed private detail")
	}
	claim := getAgentInstance(t, c, w)
	encoded, _ = json.Marshal(claim.Status)
	if strings.Contains(string(encoded), "private-server-detail") {
		t.Fatal("claim status exposed private detail")
	}
}

func TestAgentInstanceNativeDependencyDiagnostics(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(map[bool]string{false: "required", true: "optional"}[optional], func(t *testing.T) {
			group, ns, definition, w, source := testutil.Fixture()
			source.Spec.Containers[0].Env = append(source.Spec.Containers[0].Env, secretReference("credentials", "missing", optional))
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: w.Namespace}, Data: map[string][]byte{"other": []byte("sensitive-value-never-report")}}
			c, scheme := clientWithPublishedDefinitions(t, definition, nativeTemplate(definition, source), group, ns, w, secret)
			r := &Reconciler{Client: c, Scheme: scheme}
			reconcileWithUpstreamSimulation(t, r, w)
			state := getAgentInstance(t, c, w)
			if !optional {
				assertNoSandbox(t, c, w)
				assertReason(t, state, "ConfigurationAccepted", "DependencyKeyMissing")
			} else {
				getSandbox(t, c, w)
			}
			for _, condition := range state.Status.Conditions {
				if strings.Contains(condition.Message, "sensitive-value") {
					t.Fatal("secret value leaked")
				}
			}
		})
	}
}
