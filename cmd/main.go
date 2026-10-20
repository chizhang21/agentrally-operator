package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	v1 "arally/operator/api/v1alpha1"
	"arally/operator/internal/connector"
	"arally/operator/internal/controller/agentclaim"
	"arally/operator/internal/controller/agentgroup"
	"arally/operator/internal/controller/clustercapabilities"
	"arally/operator/internal/sandbox"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
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
	flag.StringVar(&connectorConfig.TokenFile, "connector-token-file", "/var/run/arally-connector/token", "Mounted backend credential file")
	flag.StringVar(&connectorConfig.CAFile, "connector-ca-file", "", "Optional backend private CA PEM file")
	flag.Parse()
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, managerOptions(probeAddr, metricsAddr, electionNamespace, leaderElection))
	if err != nil {
		return err
	}
	if err := (&agentgroup.Reconciler{}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&agentclaim.Reconciler{}).SetupWithManager(mgr); err != nil {
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
		default:
		}
		return nil
	})
}

func managerOptions(probeAddr, metricsAddr, electionNamespace string, leaderElection bool) ctrl.Options {
	return ctrl.Options{
		Scheme: scheme,
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor:   []client.Object{&corev1.Secret{}},
			Unstructured: true,
		}},
		Controller:                    config.Controller{ReconciliationTimeout: reconcileTimeout},
		Metrics:                       metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                leaderElection,
		LeaderElectionNamespace:       electionNamespace,
		LeaderElectionID:              "arally-operator.arally.io",
		LeaderElectionReleaseOnCancel: true,
	}
}
