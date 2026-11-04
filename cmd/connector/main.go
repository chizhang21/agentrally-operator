// The Connector runs independently of the Operator controllers.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"agentrally/operator/internal/connector"
	"agentrally/operator/internal/controller"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	if err := run(); err != nil {
		ctrl.Log.Error(err, "unable to run Connector")
		os.Exit(1)
	}
}

func run() error {
	var probeAddr, electionNamespace, electionID string
	var config connector.Config
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe address")
	flag.StringVar(&electionNamespace, "leader-election-namespace", os.Getenv("POD_NAMESPACE"), "Leader election namespace")
	flag.StringVar(&electionID, "leader-election-id", "agentrally-connector", "Leader election Lease name")
	flag.StringVar(&config.Endpoint, "connector-endpoint", "", "HTTPS backend base URL")
	flag.StringVar(&config.ID, "connector-id", "", "Stable backend cluster/connector id")
	flag.StringVar(&config.TokenFile, "connector-token-file", "/var/run/agentrally-connector/token", "Mounted backend credential file")
	flag.StringVar(&config.CAFile, "connector-ca-file", "", "Optional backend private CA PEM file")
	flag.BoolVar(&config.InvocationPreview, "connector-invocation-preview", false, "Enable experimental invocation transport")
	flag.Parse()
	config.InvocationPeer = controller.InvocationPeer{Namespace: os.Getenv("POD_NAMESPACE"), ReleaseName: os.Getenv("OPERATOR_RELEASE")}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, managerOptions(probeAddr, electionNamespace, electionID))
	if err != nil {
		return err
	}
	if err := addConnector(mgr, cfg, config, client.ObjectKey{Namespace: electionNamespace, Name: electionID}); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// The manager supplies probes, graceful shutdown and leader election only.
// Connector reads use direct dynamic/metadata clients; no controllers or informers run here.
func managerOptions(probeAddr, electionNamespace, electionID string) ctrl.Options {
	return ctrl.Options{
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                true,
		LeaderElectionNamespace:       electionNamespace,
		LeaderElectionID:              electionID,
		LeaderElectionReleaseOnCancel: true,
	}
}

func addConnector(mgr ctrl.Manager, cfg *rest.Config, connectorConfig connector.Config, electionLease client.ObjectKey) error {
	connectorKubeConfig := rest.CopyConfig(cfg)
	connectorKubeConfig.Timeout = 15 * time.Second
	kubeClient, err := dynamic.NewForConfig(connectorKubeConfig)
	if err != nil {
		return err
	}
	metadataClient, err := connector.NewMetadataClient(connectorKubeConfig)
	if err != nil {
		return err
	}
	relay, err := connector.NewClient(connectorConfig, connector.Executor{Client: kubeClient, Metadata: metadataClient})
	if err != nil {
		return err
	}
	if err := mgr.Add(relay); err != nil {
		return err
	}
	return mgr.AddReadyzCheck("connector", func(request *http.Request) error {
		select {
		case <-mgr.Elected():
			if !relay.Ready() {
				return errors.New("connector backend unavailable")
			}
			if relay.InvocationFailed() {
				return errors.New("invocation preview stopped; restart the Connector to re-enable it")
			}
		default:
			// A standby may be ready during rolling updates only while a valid
			// election exists. A missing Helm-owned Lease cannot be recreated here.
			ctx, cancel := context.WithTimeout(request.Context(), time.Second)
			defer cancel()
			lease := &coordinationv1.Lease{}
			if err := mgr.GetAPIReader().Get(ctx, electionLease, lease); err != nil {
				return errors.New("connector leader election lease unavailable")
			}
			spec := lease.Spec
			if lease.DeletionTimestamp != nil || spec.HolderIdentity == nil || *spec.HolderIdentity == "" ||
				spec.RenewTime == nil || spec.LeaseDurationSeconds == nil || *spec.LeaseDurationSeconds <= 0 ||
				!spec.RenewTime.Add(time.Duration(*spec.LeaseDurationSeconds)*time.Second).After(time.Now()) {
				return errors.New("connector has no active leader")
			}
		}
		return nil
	})
}
