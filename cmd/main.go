package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	v1 "agentrally/operator/api/v1alpha1"
	"agentrally/operator/internal/connector"
	"agentrally/operator/internal/controller"
	"agentrally/operator/internal/controller/agentdefinition"
	"agentrally/operator/internal/controller/agentgroup"
	"agentrally/operator/internal/controller/agentinstance"
	"agentrally/operator/internal/controller/agentworkspace"
	"agentrally/operator/internal/controller/clustercapabilities"
	"agentrally/operator/internal/metrics"
	"agentrally/operator/internal/sandbox"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

const reconcileTimeout = 30 * time.Second

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	utilruntime.Must(sandbox.AddToScheme(scheme))
}

func main() {
	if err := run(); err != nil {
		ctrl.Log.Error(err, "unable to run Operator")
		os.Exit(1)
	}
}

func run() error {
	var probeAddr, metricsAddr, electionNamespace string
	var leaderElection, connectorEnabled bool
	var connectorConfig connector.Config
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe address")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics address; 0 disables metrics")
	flag.StringVar(&electionNamespace, "leader-election-namespace", os.Getenv("POD_NAMESPACE"), "Leader election namespace")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election")
	flag.BoolVar(&connectorEnabled, "connector-enabled", false, "Enable the outbound Connector")
	flag.StringVar(&connectorConfig.Endpoint, "connector-endpoint", "", "HTTPS backend base URL")
	flag.StringVar(&connectorConfig.ID, "connector-id", "", "Stable backend cluster/connector id")
	flag.StringVar(&connectorConfig.TokenFile, "connector-token-file", "/var/run/agentrally-connector/token", "Mounted backend credential file")
	flag.StringVar(&connectorConfig.CAFile, "connector-ca-file", "", "Optional backend private CA PEM file")
	flag.BoolVar(&connectorConfig.InvocationPreview, "connector-invocation-preview", false, "Enable experimental invocation transport")
	flag.Parse()
	connectorConfig.InvocationPeer = controller.InvocationPeer{Namespace: os.Getenv("POD_NAMESPACE"), ReleaseName: os.Getenv("OPERATOR_RELEASE")}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, managerOptions(probeAddr, metricsAddr, electionNamespace, leaderElection))
	if err != nil {
		return err
	}
	if err := (&agentgroup.Reconciler{InvocationPeer: connectorConfig.InvocationPeer}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&agentdefinition.Reconciler{}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&agentworkspace.Reconciler{}).SetupWithManager(mgr); err != nil {
		return err
	}
	observations := metrics.New(mgr.GetClient())
	if err := controllermetrics.Registry.Register(observations); err != nil {
		return err
	}
	if err := (&agentinstance.Reconciler{Metrics: observations}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&clustercapabilities.Reconciler{}).SetupWithManager(mgr); err != nil {
		return err
	}
	if connectorEnabled {
		if err := addConnector(mgr, cfg, connectorConfig); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

func addConnector(mgr ctrl.Manager, cfg *rest.Config, connectorConfig connector.Config) error {
	connectorKubeConfig := rest.CopyConfig(cfg)
	connectorKubeConfig.Timeout = 15 * time.Second
	kubeClient, err := dynamic.NewForConfig(connectorKubeConfig)
	if err != nil {
		return err
	}
	relay, err := connector.NewClient(connectorConfig, connector.Executor{Client: kubeClient})
	if err != nil {
		return err
	}
	if err := mgr.Add(relay); err != nil {
		return err
	}
	return mgr.AddReadyzCheck("connector", func(_ *http.Request) error {
		select {
		case <-mgr.Elected():
			if !relay.Ready() {
				return errors.New("connector backend unavailable")
			}
			if relay.InvocationFailed() {
				return errors.New("invocation preview stopped; restart the Operator to re-enable it")
			}
		default:
		}
		return nil
	})
}

func managerOptions(probeAddr, metricsAddr, electionNamespace string, leaderElection bool) ctrl.Options {
	return ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}: {Transform: trimNodeCache},
		}},
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor:   []client.Object{&corev1.Secret{}},
			Unstructured: true,
		}},
		Controller:                    config.Controller{ReconciliationTimeout: reconcileTimeout},
		Metrics:                       metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                leaderElection,
		LeaderElectionNamespace:       electionNamespace,
		LeaderElectionID:              "agentrally-operator.arally.io",
		LeaderElectionReleaseOnCancel: true,
	}
}

func trimNodeCache(object interface{}) (interface{}, error) {
	node := object.(*corev1.Node).DeepCopy()
	node.Status.Images = nil
	node.ManagedFields = nil
	return node, nil
}
