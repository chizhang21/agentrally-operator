package packaging_test

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func renderObjects(t *testing.T, overrides ...string) map[string][]byte {
	t.Helper()
	return renderChartObjects(t, "../../chart", overrides...)
}

func renderChartObjects(t *testing.T, chart string, overrides ...string) map[string][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm required")
	}
	args := append([]string{"template", "agentrally-operator", chart, "-n", "agentrally-system"}, overrides...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm: %v\n%s", err, out)
	}
	objects := map[string][]byte{}
	reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(out)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		var object struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &object); err != nil {
			t.Fatal(err)
		}
		if object.Kind != "" {
			objects[object.Kind+"/"+object.Name] = doc
		}
	}
}

func TestOperatorConfiguration(t *testing.T) {
	values := []byte(`image:
  repository: registry.example.com/agentrally-operator
imagePullSecrets:
  - name: registry-credentials
nodeSelector:
  kubernetes.io/os: linux
tolerations:
  - key: dedicated
    operator: Equal
    value: agents
    effect: NoSchedule
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: workload
              operator: In
              values: [agents]
connector:
  enabled: true
  endpoint: https://backend.example/
  id: cluster-one
  credentialsSecret: backend-credentials
`)
	filename := t.TempDir() + "/values.yaml"
	if err := os.WriteFile(filename, values, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../chart/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(data, &chart); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		tag  string
		uid  int64
	}{
		{name: "kubernetes", args: []string{"--set", "connector.enabled=false"}, tag: chart.AppVersion},
		{name: "custom-image", args: []string{"--set-string", "image.tag=custom-build"}, tag: "custom-build"},
		{name: "explicit-namespace-uid", args: []string{"--set", "podSecurityContext.runAsUser=1001010000"}, tag: chart.AppVersion, uid: 1001010000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := renderObjects(t, append([]string{"-f", filename, "--set", "replicaCount=2"}, tc.args...)...)
			name := "agentrally-operator"
			var dep appsv1.Deployment
			if err := yaml.Unmarshal(objects["Deployment/"+name], &dep); err != nil {
				t.Fatal(err)
			}
			if dep.Name != name || dep.Namespace != "agentrally-system" {
				t.Fatalf("installed deployment identity changed: %+v", dep.ObjectMeta)
			}
			selector := map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/instance": name}
			if dep.Spec.Selector == nil || !reflect.DeepEqual(dep.Spec.Selector.MatchLabels, selector) {
				t.Fatalf("upgrade changes immutable selector: %+v", dep.Spec.Selector)
			}
			for key, value := range selector {
				if dep.Spec.Template.Labels[key] != value {
					t.Fatal("Pod does not match operator selector")
				}
			}
			pod := dep.Spec.Template.Spec
			if pod.ServiceAccountName != name || len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "registry-credentials" {
				t.Fatal("operator identity or registry credentials lost")
			}
			if pod.NodeSelector["kubernetes.io/os"] != "linux" || len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != "dedicated" || pod.Affinity == nil || pod.Affinity.NodeAffinity == nil {
				t.Fatalf("operator scheduling settings lost: %+v", pod)
			}
			if pod.SecurityContext == nil || pod.SecurityContext.FSGroup != nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Fatalf("admission-compatible non-root defaults lost: %+v", pod.SecurityContext)
			}
			// Rendering checks configuration; admission needs a live cluster.
			uid := pod.SecurityContext.RunAsUser
			if tc.uid == 0 && uid != nil {
				t.Fatalf("default UID %d overrides the image or admission identity", *uid)
			}
			if tc.uid != 0 && (uid == nil || *uid != tc.uid) {
				t.Fatalf("explicit UID was not rendered: %+v", pod.SecurityContext)
			}
			if len(pod.Containers) != 1 {
				t.Fatalf("expected one operator container, got %d", len(pod.Containers))
			}
			if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
				t.Fatal("replica count constrained")
			}
			container := pod.Containers[0]
			if len(container.Env) != 2 || container.Env[0].Name != "OPERATOR_RELEASE" || container.Env[0].ValueFrom.FieldRef.FieldPath != "metadata.labels['app.kubernetes.io/instance']" || container.Env[1].Name != "POD_NAMESPACE" {
				t.Fatal("Operator identity environment lost")
			}
			if !strings.Contains(strings.Join(container.Args, " "), "--leader-elect=true") {
				t.Fatal("leader election disabled")
			}
			for _, port := range container.Ports {
				if port.ContainerPort != 8080 && port.ContainerPort != 8081 {
					t.Fatal("unexpected listener")
				}
			}
			if container.Image != "registry.example.com/agentrally-operator:"+tc.tag {
				t.Fatalf("image version not resolved: %s", container.Image)
			}
			sc := container.SecurityContext
			if sc == nil || sc.RunAsUser != nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
				t.Fatalf("restricted container security lost: %+v", sc)
			}
			if container.LivenessProbe == nil || container.ReadinessProbe == nil || container.LivenessProbe.HTTPGet == nil || container.ReadinessProbe.HTTPGet == nil || container.LivenessProbe.HTTPGet.Path != "/healthz" || container.ReadinessProbe.HTTPGet.Path != "/readyz" {
				t.Fatal("health probes lost")
			}
			var svc corev1.Service
			if err := yaml.Unmarshal(objects["Service/agentrally-operator-metrics"], &svc); err != nil {
				t.Fatal(err)
			}
			if svc.Namespace != "agentrally-system" || !reflect.DeepEqual(svc.Spec.Selector, map[string]string{"app.kubernetes.io/name": "agentrally-operator", "app.kubernetes.io/instance": "agentrally-operator"}) {
				t.Fatal("metrics no longer select the manager")
			}
		})
	}
}

func TestServiceMonitorRequiresAPIAndSelectsManager(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm required")
	}
	for _, object := range renderObjects(t) {
		if bytes.Contains(object, []byte("kind: ServiceMonitor")) {
			t.Fatal("default install requires monitoring APIs")
		}
	}
	out, err := exec.Command("helm", "template", "agentrally-operator", "../../chart", "--set", "metrics.serviceMonitor.enabled=true").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "requires monitoring.coreos.com/v1/ServiceMonitor") {
		t.Fatalf("missing monitoring CRD not explained: %v\n%s", err, out)
	}
	objects := renderObjects(t, "--api-versions", "monitoring.coreos.com/v1/ServiceMonitor", "--set", "metrics.serviceMonitor.enabled=true", "--set", "metrics.serviceMonitor.labels.release=prometheus")
	var monitor struct {
		metav1.ObjectMeta `json:"metadata"`
		Spec              struct {
			Selector  metav1.LabelSelector `json:"selector"`
			Endpoints []struct {
				Port string `json:"port"`
			} `json:"endpoints"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(objects["ServiceMonitor/agentrally-operator-metrics"], &monitor); err != nil {
		t.Fatal(err)
	}
	if monitor.Namespace != "agentrally-system" || monitor.Labels["release"] != "prometheus" || !reflect.DeepEqual(monitor.Spec.Selector.MatchLabels, map[string]string{"app.kubernetes.io/name": "agentrally-operator", "app.kubernetes.io/instance": "agentrally-operator"}) || len(monitor.Spec.Endpoints) != 1 || monitor.Spec.Endpoints[0].Port != "metrics" {
		t.Fatalf("ServiceMonitor cannot select manager metrics: %+v", monitor)
	}
}
