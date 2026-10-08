// Command manager runs the PostgresBackup controller.
//
// It is built to run out of cluster against a kubeconfig as well as in one.
// There is no image and no Deployment manifest in this repo, because nothing
// here has ever pushed one and a manifest referring to a tag that does not
// exist is worse than its absence.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
	"github.com/Sam-Farooq/k8s-operator/internal/controller"
	"github.com/Sam-Farooq/k8s-operator/internal/version"
)

// version is overridden at build time by the Makefile's -ldflags. The default
// is the package constant, so a `go build` with no flags still reports
// something true.
var operatorVersion = version.Version

func main() {
	var (
		metricsAddr      string
		probeAddr        string
		leaderElect      bool
		leaderElectionID string
		watchNamespace   string
		showVersion      bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address for the metrics endpoint. Plain HTTP and unauthenticated; 0 turns it off.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address for /healthz and /readyz.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Hold a lease so only one replica reconciles. Required with more than one replica.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "postgresbackup.ops.sam-farooq.dev",
		"Name of the Lease object used for leader election.")
	flag.StringVar(&watchNamespace, "namespace", "",
		"Watch one namespace instead of all of them. Empty watches all.")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println(operatorVersion)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		logger.Error(err, "registering the built-in types")
		os.Exit(1)
	}
	if err := opsv1alpha1.AddToScheme(scheme); err != nil {
		logger.Error(err, "registering the PostgresBackup types")
		os.Exit(1)
	}

	cacheOpts := cache.Options{}
	if watchNamespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       leaderElectionID,
		// Give up the lease on a clean shutdown so a rolling restart does not
		// wait out the lease duration before the new pod can reconcile.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		logger.Error(err, "building the manager")
		os.Exit(1)
	}

	if err := (&controller.PostgresBackupReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "wiring the PostgresBackup controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		logger.Error(err, "adding the health check")
		os.Exit(1)
	}
	// Readiness is the cache, not the process. A manager whose informers have
	// not synced will reconcile against an empty world and decide that every
	// slot needs a Job.
	if err := mgr.AddReadyzCheck("cache-sync", func(_ *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(context.Background()) {
			return errors.New("informer cache has not synced")
		}
		return nil
	}); err != nil {
		logger.Error(err, "adding the readiness check")
		os.Exit(1)
	}

	logger.Info("starting manager", "version", operatorVersion, "leaderElection", leaderElect, "namespace", orAll(watchNamespace))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager stopped")
		os.Exit(1)
	}
}

func orAll(ns string) string {
	if ns == "" {
		return "(all)"
	}
	return ns
}
