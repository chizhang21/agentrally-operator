//go:build e2e

package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/controller"

	"golang.org/x/net/dns/dnsmessage"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeEgressTraffic(t *testing.T) {
	if os.Getenv("E2E_CNI") != "cilium" {
		t.Skip("requires E2E_CNI=cilium")
	}
	kube, kubeconfig := e2eClient(t)
	ctx := t.Context()
	external := newEgressFixtures(t, kube, kubeconfig)
	group := readyGroup(t, ctx, kube, "native-egress")
	targetNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agentrally-egress-targets"}}
	create(t, ctx, kube, targetNamespace)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, object := range []client.Object{group, targetNamespace} {
			if err := client.IgnoreNotFound(kube.Delete(cleanupCtx, object)); err != nil {
				t.Error("remove egress test resource:", err)
			}
		}
	})
	broker := egressTarget(t, kube, targetNamespace.Name, "broker", "broker")
	wrongPod := egressTarget(t, kube, targetNamespace.Name, "wrong-pod", "unrelated")
	wrongNamespace := egressTarget(t, kube, group.Status.Namespace, "wrong-namespace", "broker")
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "broker", Namespace: targetNamespace.Name}, Spec: corev1.ServiceSpec{
		Selector: broker.Labels, Ports: []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}},
	}}
	create(t, ctx, kube, service)
	definition, agent := testAgent(group, "caller", "sleep 3600")
	create(t, ctx, kube, definition)
	create(t, ctx, kube, agent)
	readyAgent(t, ctx, kube, agent)
	originalPod := agent.Status.PodUID
	caller := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.PodName, Namespace: agent.Namespace}}
	dns := &corev1.Service{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kube-dns"}, dns); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []*corev1.Pod{caller, broker} {
		if got := execPod(t, kubeconfig, pod.Namespace, pod.Name, "sh", "-ec", "command -v wget >/dev/null; command -v nc >/dev/null; command -v base64 >/dev/null; printf ready"); got != "ready" {
			t.Fatal("probe tools unavailable")
		}
	}
	allowed := []egressEndpoint{
		{"broker", "http://" + broker.Status.PodIP + ":8080/", "broker"},
		{"external-80", "http://" + external.allowIP + ":80/", "external-allow"},
		{"external-8080", "http://" + external.allowIP + ":8080/", "external-allow"},
	}
	denied := []egressEndpoint{
		{"wrong-pod", "http://" + wrongPod.Status.PodIP + ":8080/", "wrong-pod"},
		{"wrong-namespace", "http://" + wrongNamespace.Status.PodIP + ":8080/", "wrong-namespace"},
		{"excluded-ip", "http://" + external.exceptIP + ":80/", "external-except"},
		{"wrong-port", "http://" + external.allowIP + ":8081/", "external-allow"},
		{"fake-metadata", "http://169.254.169.254:80/", "external-allow"},
	}
	probeHTTP := func(targets []egressEndpoint, wantAllowed bool) {
		t.Helper()
		for _, target := range targets {
			matches := 0
			await(t, target.name+fmt.Sprintf(" allowed=%t", wantAllowed), func() (bool, error) {
				// A working target must accompany every denial, not just its baseline.
				if !wantAllowed && !egressHTTP(t, kubeconfig, broker, target) {
					return false, fmt.Errorf("unrestricted control cannot reach %s", target.name)
				}
				if egressHTTP(t, kubeconfig, caller, target) == wantAllowed {
					matches++
				} else {
					matches = 0
				}
				return matches >= 3, nil
			})
		}
	}
	probeDNS := func(wantAllowed bool) {
		t.Helper()
		for _, transport := range []string{"udp", "tcp"} {
			matches := 0
			await(t, transport+fmt.Sprintf(" DNS allowed=%t", wantAllowed), func() (bool, error) {
				name := service.Name + "." + service.Namespace + ".svc.cluster.local."
				if !wantAllowed && !egressDNS(t, kubeconfig, broker, dns.Spec.ClusterIP, name, service.Spec.ClusterIP, transport) {
					return false, fmt.Errorf("unrestricted DNS control failed")
				}
				if egressDNS(t, kubeconfig, caller, dns.Spec.ClusterIP, name, service.Spec.ClusterIP, transport) == wantAllowed {
					matches++
				} else {
					matches = 0
				}
				return matches >= 3, nil
			})
		}
	}
	all := append(append([]egressEndpoint{}, allowed...), denied...)
	t.Log("Baseline: every target, port and both DNS transports work")
	probeHTTP(all, true)
	probeDNS(true)
	group.Spec.NetworkPolicy = &networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}
	update(t, ctx, kube, group)
	t.Log("Default deny: DNS, cluster, external and fake metadata are blocked")
	probeHTTP(all, false)
	probeDNS(false)
	group.Spec.NetworkPolicy.Egress = []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{egressPeer("kube-system", "k8s-app", "kube-dns")}, Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}, {Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))},
		}},
		{To: []networkingv1.NetworkPolicyPeer{egressPeer(targetNamespace.Name, "role", "broker")}, Ports: []networkingv1.NetworkPolicyPort{{Port: ptr.To(intstr.FromInt32(8080))}}},
		{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: external.cidr, Except: []string{external.exceptIP + "/32"}}}}, Ports: []networkingv1.NetworkPolicyPort{
			{Port: ptr.To(intstr.FromInt32(80))}, {Port: ptr.To(intstr.FromInt32(8080))},
		}},
	}
	update(t, ctx, kube, group)
	t.Log("Selective allow: DNS, namespace AND Pod, external CIDR except and ports")
	probeDNS(true)
	probeHTTP(append(append([]egressEndpoint{}, allowed...), egressEndpoint{"broker-service", "http://broker." + targetNamespace.Name + ".svc.cluster.local:8080/", "broker"}), true)
	probeHTTP(denied, false)
	extra := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "additional-egress", Namespace: caller.Namespace}, Spec: networkingv1.NetworkPolicySpec{
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{{}},
	}}
	create(t, ctx, kube, extra)
	t.Log("Policies are additive: an independently owned allow-all reopens omitted paths")
	probeHTTP(denied, true)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(extra), extra); err != nil || len(extra.OwnerReferences) != 0 || len(extra.Spec.Egress) != 1 {
		t.Fatal("Operator changed an independent policy", err)
	}
	if err := kube.Delete(ctx, extra); err != nil {
		t.Fatal(err)
	}
	probeHTTP(denied, false)
	group.Spec.InvocationAccess = &v1.InvocationAccess{Mode: "ConnectorOnly"}
	update(t, ctx, kube, group)
	await(t, "ConnectorOnly configured alongside egress", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKeyFromObject(group), group)
		return conditionTrue(group.Status.Conditions, "InvocationAccessReady", group.Generation), err
	})
	t.Log("ConnectorOnly preserves external egress rules")
	probeDNS(true)
	probeHTTP(allowed[1:], true)
	probeHTTP(denied[2:], false)
	group.Spec.InvocationAccess, group.Spec.NetworkPolicy = nil, nil
	update(t, ctx, kube, group)
	await(t, "owned policy removed", func() (bool, error) {
		err := kube.Get(ctx, client.ObjectKey{Namespace: caller.Namespace, Name: controller.GroupNetworkPolicyName}, &networkingv1.NetworkPolicy{})
		return err != nil && client.IgnoreNotFound(err) == nil, client.IgnoreNotFound(err)
	})
	probeHTTP(all, true)
	probeDNS(true)
	readyAgent(t, ctx, kube, agent)
	if agent.Status.PodUID != originalPod {
		t.Fatal("policy changes restarted the Agent")
	}
	report := &v1.ClusterCapabilities{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "cluster"}, report); err != nil {
		t.Fatal(err)
	}
	if report.Status.NetworkPolicy == nil || report.Status.NetworkPolicy.EnforcementVerified {
		t.Fatal("traffic test changed passive discovery into enforcement evidence")
	}
	if condition := meta.FindStatusCondition(group.Status.Conditions, "IngressEnforcementVerified"); condition != nil && condition.Status == metav1.ConditionTrue {
		t.Fatal("configuration claimed ingress enforcement")
	}
	if err := kube.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, agent)
	// Delete the standalone same-namespace target before the existing empty-execution check.
	if err := kube.Delete(ctx, wrongNamespace); err != nil {
		t.Fatal(err)
	}
	awaitExecutionRemoved(t, ctx, kube, caller.Namespace)
	deleteDefinitions(t, ctx, kube, group)
	if err := kube.Delete(ctx, group); err != nil {
		t.Fatal(err)
	}
	awaitDeleted(t, ctx, kube, group)
}

func egressPeer(namespace, key, value string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{key: value}},
	}
}

func egressTarget(t *testing.T, kube client.Client, namespace, name, role string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"role": role, "target": name}}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}, Containers: []corev1.Container{{Name: "target", Image: "docker.io/library/busybox:1.37.0",
			Command:         []string{"sh", "-ec", `mkdir -p /tmp/www; printf '%s' "$1" > /tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www`, "target", name},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("16Mi")}},
			ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1},
		}},
	}}
	create(t, t.Context(), kube, pod)
	await(t, name+" HTTP fixture readiness", func() (bool, error) {
		err := kube.Get(t.Context(), client.ObjectKeyFromObject(pod), pod)
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, err
			}
		}
		return false, err
	})
	return pod
}

type egressEndpoint struct{ name, url, marker string }

func egressHTTP(t *testing.T, kubeconfig string, pod *corev1.Pod, target egressEndpoint) bool {
	t.Helper()
	out := execPod(t, kubeconfig, pod.Namespace, pod.Name, "sh", "-ec", `
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
command -v wget >/dev/null
if wget -Y off -T 1 -O /tmp/egress-body "$1" 2>/tmp/egress-error; then
  printf 'ok:'; cat /tmp/egress-body
elif grep -Eq 'timed out|Connection refused|No route to host|Network is unreachable|Operation not permitted' /tmp/egress-error; then
  printf denied
else
  cat /tmp/egress-error >&2; exit 1
fi`, "probe", target.url)
	switch out {
	case "ok:" + target.marker:
		return true
	case "denied":
		return false
	default:
		t.Fatalf("%s returned an unexpected HTTP response: %q", target.name, out)
		return false
	}
}

func egressDNS(t *testing.T, kubeconfig string, pod *corev1.Pod, server, name, address, transport string) bool {
	t.Helper()
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 1234, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	packet, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if transport == "tcp" {
		packet = append(binary.BigEndian.AppendUint16(nil, uint16(len(packet))), packet...)
	}
	out := execPod(t, kubeconfig, pod.Namespace, pod.Name, "sh", "-ec", `
command -v nc >/dev/null; command -v base64 >/dev/null
printf '%s' "$1" | base64 -d > /tmp/dns-query
# BusyBox nc needs -vv to expose connect failures instead of a silent exit 1.
status=0
if [ "$3" = udp ]; then
  nc -vv -u -n -w 1 "$2" 53 < /tmp/dns-query > /tmp/dns-answer 2>/tmp/dns-error || status=$?
else
  nc -vv -n -w 1 "$2" 53 < /tmp/dns-query > /tmp/dns-answer 2>/tmp/dns-error || status=$?
fi
if ! test -s /tmp/dns-answer && [ "$status" -ne 0 ]; then
  if ! grep -Eq 'timeout|timed out|refused|unreachable|not permitted' /tmp/dns-error; then
    printf 'Unexpected nc failure (transport=%s, status=%s): ' "$3" "$status" >&2
    cat /tmp/dns-error >&2
    exit 1
  fi
fi
if test -s /tmp/dns-answer; then base64 /tmp/dns-answer; else printf denied; fi`, "dns", base64.StdEncoding.EncodeToString(packet), server, transport)
	if out == "denied" {
		return false
	}
	answer, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatal("DNS probe output:", err)
	}
	if transport == "tcp" {
		if len(answer) < 2 || int(binary.BigEndian.Uint16(answer)) != len(answer)-2 {
			t.Fatal("invalid TCP DNS response length")
		}
		answer = answer[2:]
	}
	var message dnsmessage.Message
	if err := message.Unpack(answer); err != nil || message.ID != query.ID || !message.Response || message.RCode != dnsmessage.RCodeSuccess {
		t.Fatal("invalid DNS response", err, message.Header)
	}
	for _, resource := range message.Answers {
		if record, ok := resource.Body.(*dnsmessage.AResource); ok && netip.AddrFrom4(record.A).String() == address {
			return true
		}
	}
	t.Fatal("DNS response omitted the expected Service A record")
	return false
}

type egressFixtures struct{ allowIP, exceptIP, cidr string }

// The metadata route lives only in this disposable Kind's network namespaces.
// No request is made until both the alias and every next hop are verified.
func newEgressFixtures(t *testing.T, kube client.Client, kubeconfig string) egressFixtures {
	t.Helper()
	raw, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil || !strings.HasPrefix(raw.CurrentContext, "kind-agentrally-e2e-") {
		t.Fatal("external fixtures require a disposable Kind context", err)
	}
	cluster := strings.TrimPrefix(raw.CurrentContext, "kind-")
	nodes := &corev1.NodeList{}
	if err := kube.List(t.Context(), nodes); err != nil || len(nodes.Items) == 0 {
		t.Fatal("list Kind nodes", err)
	}
	type dockerNode struct {
		ID              string
		Config          struct{ Labels map[string]string }
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	var nodeIDs []string
	var network, nodeIP string
	for _, node := range nodes.Items {
		var inspected []dockerNode
		if err := json.Unmarshal([]byte(egressCommand(t, "docker", "inspect", node.Name)), &inspected); err != nil || len(inspected) != 1 {
			t.Fatal("inspect Kind node", err)
		}
		item := inspected[0]
		if item.Config.Labels["io.x-k8s.kind.cluster"] != cluster || !strings.HasPrefix(node.Name, cluster+"-") || len(item.NetworkSettings.Networks) != 1 {
			t.Fatal("node does not belong exclusively to this Kind fixture")
		}
		for name, endpoint := range item.NetworkSettings.Networks {
			if network != "" && network != name {
				t.Fatal("Kind nodes use different Docker networks")
			}
			network, nodeIP = name, endpoint.IPAddress
		}
		nodeIDs = append(nodeIDs, item.ID)
	}
	var inspectedNetwork []struct {
		IPAM struct{ Config []struct{ Subnet string } }
	}
	if err := json.Unmarshal([]byte(egressCommand(t, "docker", "network", "inspect", network)), &inspectedNetwork); err != nil || len(inspectedNetwork) != 1 {
		t.Fatal("inspect fixture network", err)
	}
	fixtures := egressFixtures{}
	for _, ipam := range inspectedNetwork[0].IPAM.Config {
		prefix, err := netip.ParsePrefix(ipam.Subnet)
		ip, ipErr := netip.ParseAddr(nodeIP)
		if err == nil && ipErr == nil && ip.Is4() && prefix.Contains(ip) {
			fixtures.cidr = prefix.String()
		}
	}
	if fixtures.cidr == "" {
		t.Fatal("fixture network has no matching IPv4 subnet")
	}
	var containerIDs, routedNodes []string
	t.Cleanup(func() {
		// Never use t.Context here: timeout/failure may already have cancelled it.
		for _, node := range routedNodes {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			output, err := exec.CommandContext(cleanupCtx, "docker", "exec", node, "ip", "route", "del", "169.254.169.254/32", "via", fixtures.allowIP).CombinedOutput()
			cancel()
			if err != nil {
				t.Errorf("remove fake metadata route: %v: %s", err, output)
			}
		}
		for _, id := range containerIDs {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			output, err := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", id).CombinedOutput()
			cancel()
			if err != nil {
				t.Errorf("remove external fixture: %v: %s", err, output)
			}
		}
	})
	for _, name := range []string{"allow", "except"} {
		options := []string{"create", "--network", network, "--label", "io.agentrally.e2e.egress-cluster=" + cluster,
			"--name", cluster + "-egress-" + name, "--memory", "32m", "--cpus", "0.25"}
		if name == "allow" {
			options = append(options, "--cap-add", "NET_ADMIN")
		}
		id := egressCommand(t, "docker", append(options,
			"docker.io/library/busybox:1.37.0", "sh", "-ec", `
mkdir -p /tmp/www
printf 'external-%s' "$1" > /tmp/www/index.html
if [ "$1" = allow ]; then ip addr add 169.254.169.254/32 dev eth0; fi
httpd -p 80 -h /tmp/www
httpd -p 8081 -h /tmp/www
exec httpd -f -p 8080 -h /tmp/www`, "fixture", name)...)
		containerIDs = append(containerIDs, id)
		egressCommand(t, "docker", "start", id)
		ip := egressCommand(t, "docker", "inspect", "--format", "{{(index .NetworkSettings.Networks "+fmt.Sprintf("%q", network)+").IPAddress}}", id)
		if parsed, err := netip.ParseAddr(ip); err != nil || !parsed.Is4() {
			t.Fatal("external fixture has no IPv4 address")
		}
		if name == "allow" {
			fixtures.allowIP = ip
			await(t, "fake metadata address owned by the fixture", func() (bool, error) {
				out := egressCommand(t, "docker", "exec", id, "ip", "-4", "addr", "show", "dev", "eth0")
				return strings.Contains(out, "169.254.169.254/32"), nil
			})
		} else {
			fixtures.exceptIP = ip
		}
	}
	for _, node := range nodeIDs {
		if out := egressCommand(t, "docker", "exec", node, "ip", "route", "show", "exact", "169.254.169.254/32"); out != "" {
			t.Fatal("refusing to replace an existing Kind metadata route")
		}
		egressCommand(t, "docker", "exec", node, "ip", "route", "add", "169.254.169.254/32", "via", fixtures.allowIP)
		routedNodes = append(routedNodes, node)
		var route []struct{ Gateway string }
		if err := json.Unmarshal([]byte(egressCommand(t, "docker", "exec", node, "ip", "-j", "route", "get", "169.254.169.254")), &route); err != nil || len(route) != 1 || route[0].Gateway != fixtures.allowIP {
			t.Fatal("fake metadata next hop verification failed", err)
		}
	}
	return fixtures
}

func egressCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("egress fixture command %s %v: %v: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}
