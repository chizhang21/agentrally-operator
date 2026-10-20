package agentclaim

import (
	"context"
	"reflect"
	"testing"

	"arally/operator/internal/podconfig"
	"arally/operator/internal/testutil"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func configurationFixture() *corev1.PodTemplateSpec {
	value := false
	nonRoot := true
	return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: &value, SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &nonRoot}, Containers: []corev1.Container{{Name: "agent", Image: "example/agent:v1", Env: []corev1.EnvVar{{Name: "OPTION", Value: "one"}}}}}}
}

func configurationReader(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestConfigurationRevisionDeterminismAndRotation(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "team", ResourceVersion: "10"}, Data: map[string][]byte{"key": []byte("DO_NOT_HASH")}}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "team", ResourceVersion: "4"}, Data: map[string]string{"config": "value"}}
	template := configurationFixture()
	template.Spec.Containers[0].Env = append(template.Spec.Containers[0].Env, corev1.EnvVar{Name: "KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}, Key: "key"}}})
	template.Spec.InitContainers = []corev1.Container{{Name: "prepare", Image: "example/init:v1", EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}}}}}
	before := template.DeepCopy()
	reader := configurationReader(t, secret, config)
	first, err := configRevision(ctx, reader, "team", template, "")
	if err != nil || len(first) != 64 {
		t.Fatalf("revision %q, error %v", first, err)
	}
	again, err := configRevision(ctx, reader, "team", template, "")
	if err != nil || first != again {
		t.Fatalf("non-deterministic revision %q / %q: %v", first, again, err)
	}
	if !reflect.DeepEqual(before, template) {
		t.Fatal("input template mutated")
	}
	// Plaintext changes with the same RV are intentionally not fingerprint inputs.
	changed := secret.DeepCopy()
	changed.Data["key"] = []byte("DIFFERENT_CONTENT")
	sameRV, err := configRevision(ctx, configurationReader(t, changed, config), "team", template, "")
	if err != nil || sameRV != first {
		t.Fatalf("plaintext entered fingerprint: %v", err)
	}
	changed.ResourceVersion = "11"
	rotated, err := configRevision(ctx, configurationReader(t, changed, config), "team", template, "")
	if err != nil || rotated == first {
		t.Fatalf("rotation not detected: %v", err)
	}
	if err := podconfig.Stamp(template, "agent", first); err != nil {
		t.Fatal(err)
	}
	stamped, err := configRevision(ctx, reader, "team", template, "")
	if err != nil || stamped != first {
		t.Fatalf("stamp changed its own revision: %v", err)
	}
	if _, err := configRevision(ctx, reader, "other-team", template, ""); err == nil {
		t.Fatal("cross-namespace reference accepted")
	}
}

func TestConfigurationReferenceCoverageAndOptionalMissing(t *testing.T) {
	yes := true
	template := configurationFixture()
	template.Spec.Containers[0].Env = append(template.Spec.Containers[0].Env, corev1.EnvVar{Name: "OPTIONAL", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "optional"}, Key: "value", Optional: &yes}}})
	template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shared"}, Optional: &yes}}}
	template.Spec.Volumes = []corev1.Volume{
		{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "shared", Optional: &yes}}},
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "optional"}, Optional: &yes}}},
		{Name: "projected", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "projected"}, Optional: &yes}}, {ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "projected"}, Optional: &yes}}}}}},
	}
	template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "pull"}}
	refs := configurationReferences(template)
	if len(refs) != 5 {
		t.Fatalf("references %#v", refs)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "team", ResourceVersion: "1"}}
	missing, err := configRevision(context.Background(), configurationReader(t, secret), "team", template, "")
	if err != nil {
		t.Fatal(err)
	}
	optional := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "optional", Namespace: "team", ResourceVersion: "1"}}
	exists, err := configRevision(context.Background(), configurationReader(t, secret, optional), "team", template, "")
	if err != nil || exists == missing {
		t.Fatalf("optional existence not fingerprinted: %v", err)
	}
	// The same name referenced optionally and as a pull Secret is required.
	template.Spec.ImagePullSecrets = append(template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: "shared"})
	if _, err := configRevision(context.Background(), configurationReader(t, secret), "team", template, ""); err == nil {
		t.Fatal("required duplicate was downgraded to optional")
	}
}

func TestConfigurationMarkerAndContainerSelection(t *testing.T) {
	template := configurationFixture()
	template.Spec.Containers = append(template.Spec.Containers, corev1.Container{Name: "tools", Image: "example/tools:v1"})
	reader := configurationReader(t)
	first, err := configRevision(ctx, reader, "team", template, "")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := configRevision(ctx, reader, "team", template, "agent")
	if err != nil || explicit != first {
		t.Fatal("equivalent first-container selections split pool identity", err)
	}
	other, err := configRevision(ctx, reader, "team", template, "tools")
	if err != nil || other == first {
		t.Fatal("different Agent containers collide in snapshot identity", err)
	}
	if err := podconfig.Stamp(template, "tools", "revision"); err != nil {
		t.Fatal(err)
	}
	if template.Spec.Containers[0].Env[len(template.Spec.Containers[0].Env)-1].Name == podconfig.RevisionEnv {
		t.Fatal("wrong container stamped")
	}
	if err := validateConfigurationMarker(template); err == nil {
		t.Fatal("reserved env accepted")
	}
	if err := podconfig.Stamp(template, "tools", "new"); err != nil {
		t.Fatal(err)
	}
	if len(template.Spec.Containers[1].Env) != 1 || template.Spec.Containers[1].Env[0].Value != "new" {
		t.Fatal("stamp not idempotent")
	}
	if err := podconfig.Stamp(template, "missing", "revision"); err == nil {
		t.Fatal("missing agent accepted")
	}
	if err := podconfig.Stamp(template, "tools", ""); err == nil {
		t.Fatal("empty revision accepted")
	}
	template = configurationFixture()
	template.Spec.InitContainers = []corev1.Container{{Name: "init", Env: []corev1.EnvVar{{Name: podconfig.RevisionEnv, Value: "spoof"}}}}
	if err := validateConfigurationMarker(template); err == nil {
		t.Fatal("init spoof accepted")
	}
}

func TestConfigurationObservationRequiresActualPodEvidence(t *testing.T) {
	template := configurationFixture()
	if err := podconfig.Stamp(template, "", "revision"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"revision": "newer"}}, Spec: *template.Spec.DeepCopy()}
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
		status metav1.ConditionStatus
		reason string
	}{
		{"current", func(*corev1.Pod) {}, metav1.ConditionTrue, "ConfigurationCurrent"},
		{"mutable metadata ignored", func(p *corev1.Pod) {
			p.Labels["revision"] = "anything"
			p.Annotations = map[string]string{podconfig.RevisionEnv: "wrong"}
		}, metav1.ConditionTrue, "ConfigurationCurrent"},
		{"old immutable revision", func(p *corev1.Pod) { p.Spec.Containers[0].Env[len(p.Spec.Containers[0].Env)-1].Value = "old" }, metav1.ConditionFalse, "RestartRequired"},
		{"markerless pool", func(p *corev1.Pod) { podconfig.StripMarkers(&p.Spec) }, metav1.ConditionUnknown, "ConfigurationUnverified"},
		{"removed env", func(p *corev1.Pod) { p.Spec.Containers[0].Env = p.Spec.Containers[0].Env[1:] }, metav1.ConditionFalse, "RestartRequired"},
		{"changed command", func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"old-command"} }, metav1.ConditionFalse, "RestartRequired"},
		{"changed resources", func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
		}, metav1.ConditionFalse, "RestartRequired"},
		{"changed security", func(p *corev1.Pod) {
			v := false
			p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &v}
		}, metav1.ConditionFalse, "RestartRequired"},
		{"changed runtime", func(p *corev1.Pod) { v := "kata"; p.Spec.RuntimeClassName = &v }, metav1.ConditionFalse, "RestartRequired"},
		{"duplicate marker", func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: podconfig.RevisionEnv, Value: "revision"})
		}, metav1.ConditionFalse, "RestartRequired"},
		{"sidecar allowed", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "injected", Image: "example/sidecar:v1"})
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "sidecar-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}, metav1.ConditionTrue, "ConfigurationCurrent"},
		{"admission additions", func(p *corev1.Pod) {
			p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "service-account-registry"}}
			p.Spec.NodeName = "node1"
			p.Spec.Hostname = "generated"
			p.Spec.Subdomain = "generated-service"
		}, metav1.ConditionTrue, "ConfigurationCurrent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := pod.DeepCopy()
			tc.mutate(actual)
			before := actual.DeepCopy()
			got := observeConfiguration(template, actual, "", "revision")
			if got.Condition.Status != tc.status || got.Condition.Reason != tc.reason {
				t.Fatalf("observation %#v", got)
			}
			if !reflect.DeepEqual(actual, before) {
				t.Fatal("observed Pod mutated")
			}
		})
	}
	if got := observeConfiguration(template, nil, "", "revision"); got.Condition.Status != metav1.ConditionUnknown || got.Condition.Reason != "PodNotObserved" {
		t.Fatalf("nil Pod %#v", got)
	}
}

func TestConfigurationMaterialRemovalAndDefaults(t *testing.T) {
	template := configurationFixture()
	template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health"}}}
	template.Spec.Volumes = []corev1.Volume{{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "creds"}}}}
	template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "secret", MountPath: "/keys", ReadOnly: true}}
	actual := template.Spec.DeepCopy()
	actual.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
	mode := int32(420)
	actual.Volumes[0].Secret.DefaultMode = &mode
	if !podconfig.MatchesAdmitted(&template.Spec, actual) {
		t.Fatal("native defaults differ")
	}
	withoutLimits := template.Spec.DeepCopy()
	withoutLimits.Containers[0].Resources = corev1.ResourceRequirements{}
	if podconfig.MatchesAdmitted(withoutLimits, actual) {
		t.Fatal("removed resource requirement missed")
	}
	withoutMount := template.Spec.DeepCopy()
	withoutMount.Containers[0].VolumeMounts = nil
	withoutMount.Volumes = nil
	if podconfig.MatchesAdmitted(withoutMount, actual) {
		t.Fatal("removed mount missed")
	}
	requested := template.Spec.DeepCopy()
	requested.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "requested-registry"}}
	actual.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "different-registry"}}
	if podconfig.MatchesAdmitted(requested, actual) {
		t.Fatal("explicit imagePullSecrets drift ignored")
	}
}

func TestConfigurationObservationAdmissionSecurityAndToken(t *testing.T) {
	template := configurationFixture()
	yes := true
	template.Spec.AutomountServiceAccountToken = &yes
	if err := podconfig.Stamp(template, "", "revision"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{Spec: *template.Spec.DeepCopy()}
	uid := int64(1000650000)
	pod.Spec.SecurityContext.RunAsUser = &uid
	pod.Spec.SecurityContext.FSGroup = &uid
	pod.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &uid, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	pod.Spec.NodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
	pod.Spec.Tolerations = []corev1.Toleration{{Key: "admission-selected", Operator: corev1.TolerationOpExists}}
	pod.Spec.Volumes = []corev1.Volume{{Name: "kube-api-access-abc", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}}}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "kube-api-access-abc", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}}
	if got := observeConfiguration(template, pod, "", "revision"); got.Condition.Status != metav1.ConditionTrue {
		t.Fatalf("admission defaults falsely require restart: %#v", got)
	}
	no := false
	pod.Spec.SecurityContext.RunAsNonRoot = &no
	if got := observeConfiguration(template, pod, "", "revision"); got.Condition.Status != metav1.ConditionFalse {
		t.Fatalf("explicit security conflict ignored: %#v", got)
	}
}

func TestConfigurationReferenceOptionalDefaults(t *testing.T) {
	tests := []struct {
		name  string
		build func(*bool) *corev1.PodTemplateSpec
	}{
		{"secret env", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "secret"}, Key: "key", Optional: optional}}}}
			return v
		}},
		{"configmap env", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KEY", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}, Key: "key", Optional: optional}}}}
			return v
		}},
		{"secret envFrom", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "secret"}, Optional: optional}}}
			return v
		}},
		{"configmap envFrom", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}, Optional: optional}}}
			return v
		}},
		{"secret volume", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Volumes = []corev1.Volume{{Name: "ref", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "secret", Optional: optional}}}}
			return v
		}},
		{"configmap volume", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Volumes = []corev1.Volume{{Name: "ref", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}, Optional: optional}}}}
			return v
		}},
		{"projected secret", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Volumes = []corev1.Volume{{Name: "ref", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "secret"}, Optional: optional}}}}}}}
			return v
		}},
		{"projected configmap", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.Volumes = []corev1.Volume{{Name: "ref", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}, Optional: optional}}}}}}}
			return v
		}},
		{"init secret env", func(optional *bool) *corev1.PodTemplateSpec {
			v := configurationFixture()
			v.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "example/init:v1", Env: []corev1.EnvVar{{Name: "KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "secret"}, Key: "key", Optional: optional}}}}}}
			return v
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			no, yes := false, true
			implicit, explicit, optional := tc.build(nil), tc.build(&no), tc.build(&yes)
			before := explicit.DeepCopy()
			if !podconfig.MatchesAdmitted(&implicit.Spec, &explicit.Spec) || !podconfig.MatchesAdmitted(&explicit.Spec, &implicit.Spec) {
				t.Fatal("implicit required differs from optional:false")
			}
			if podconfig.MatchesAdmitted(&implicit.Spec, &optional.Spec) || podconfig.MatchesAdmitted(&optional.Spec, &implicit.Spec) {
				t.Fatal("optional:true incorrectly normalized to required")
			}
			if !reflect.DeepEqual(before, explicit) {
				t.Fatal("normalization mutated caller template")
			}
		})
	}
}

func TestAgentClaimAppliedRevisionAndRotation(t *testing.T) {
	group, ns, w, source := testutil.Fixture()
	source.Spec.Containers[0].Env = append(source.Spec.Containers[0].Env, secretReference("credentials", "token", false))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: w.Namespace}, Data: map[string][]byte{"token": []byte("old-value")}}
	c, scheme := testutil.Client(t, nativeTemplate(w, source), group, ns, w, secret)
	r := &Reconciler{Client: c, Scheme: scheme}
	reconcileWithUpstreamSimulation(t, r, w)
	pod := ownedExecutionPod(t, c, w)
	reconcileWithUpstreamSimulation(t, r, w)
	state := getAgentClaim(t, c, w)
	testutil.Condition(t, state.Status.Conditions, "ConfigurationApplied", metav1.ConditionTrue)
	testutil.Condition(t, state.Status.Conditions, "Ready", metav1.ConditionTrue)
	testutil.Condition(t, state.Status.Conditions, "Available", metav1.ConditionTrue)
	old := state.Status.DesiredRevision
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["token"] = []byte("rotated-value")
	if err := c.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentClaim(t, c, w)
	assertReason(t, state, "ConfigurationApplied", "RestartRequired")
	assertReason(t, state, "Ready", "RestartRequired")
	testutil.Condition(t, state.Status.Conditions, "Available", metav1.ConditionTrue)
	if state.Status.DesiredRevision == old || state.Status.AppliedRevision != old {
		t.Fatal("rotation was not distinguished from applied revision")
	}
	// Stale Core readiness must not hide a confirmed unapplied configuration.
	execution := getSandbox(t, c, w)
	execution.SetGeneration(execution.GetGeneration() + 1)
	if err := c.Update(ctx, execution); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentClaim(t, c, w)
	testutil.Condition(t, state.Status.Conditions, "Available", metav1.ConditionUnknown)
	assertReason(t, state, "Ready", "RestartRequired")
	actual := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), actual); err != nil || actual.UID != pod.UID {
		t.Fatal("configuration change replaced Pod automatically")
	}
	// Metadata may be updated by Core without changing process configuration.
	actual.Annotations = map[string]string{"revision": state.Status.DesiredRevision}
	if err := c.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	assertReason(t, getAgentClaim(t, c, w), "ConfigurationApplied", "RestartRequired")
	actual.OwnerReferences[0].UID = "foreign-sandbox"
	if err := c.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	reconcileWithUpstreamSimulation(t, r, w)
	state = getAgentClaim(t, c, w)
	assertReason(t, state, "ConfigurationApplied", "PodObservationBlocked")
	if state.Status.AppliedRevision != "" || state.Status.PodUID != "" {
		t.Fatal("foreign Pod used as application evidence")
	}
}
