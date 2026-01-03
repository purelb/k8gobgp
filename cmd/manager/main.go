// Copyright 2025 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bgpv1 "github.com/purelb/k8gobgp/api/v1"
	"github.com/purelb/k8gobgp/controllers"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(bgpv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var probeAddr string
	var gobgpEndpoint string
	var metricsPollInterval time.Duration
	var enablePerNeighborMetrics bool
	var maxNeighborsForMetrics int
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&gobgpEndpoint, "gobgp-endpoint", "", "The GoBGP gRPC endpoint (e.g., localhost:50051 or unix:///var/run/gobgp/gobgp.sock). Can also be set via GOBGP_ENDPOINT env var.")
	flag.DurationVar(&metricsPollInterval, "metrics-poll-interval", 15*time.Second, "Interval for polling BGP stats from gobgpd (minimum 15s).")
	flag.BoolVar(&enablePerNeighborMetrics, "enable-per-neighbor-metrics", false, "Enable high-cardinality per-neighbor route metrics (use with caution in large deployments).")
	flag.IntVar(&maxNeighborsForMetrics, "max-neighbors-metrics", 200, "Maximum number of neighbors to export per-neighbor metrics for (0=unlimited).")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Note: Leader election is disabled because this controller runs as a DaemonSet.
	// Each node runs its own instance that manages the local GoBGP daemon.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
		Metrics: server.Options{
			BindAddress: metricsAddr,
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err = (&controllers.BGPConfigurationReconciler{
		Client:        mgr.GetClient(),
		Log:           ctrl.Log.WithName("controllers").WithName("BGPConfiguration"),
		Scheme:        mgr.GetScheme(),
		GoBGPEndpoint: gobgpEndpoint,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BGPConfiguration")
		os.Exit(1)
	}

	// Add BGP metrics collector as a Runnable (runs in background goroutine)
	metricsCollector := &controllers.BGPMetricsController{
		Log:           ctrl.Log.WithName("controllers").WithName("BGPMetrics"),
		GoBGPEndpoint: gobgpEndpoint,
		Config: controllers.MetricsConfig{
			PollInterval:             metricsPollInterval,
			EnablePerNeighborMetrics: enablePerNeighborMetrics,
			MaxNeighborsForMetrics:   maxNeighborsForMetrics,
		},
	}
	if err := mgr.Add(metricsCollector); err != nil {
		setupLog.Error(err, "unable to add metrics collector")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
