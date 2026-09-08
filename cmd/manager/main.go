/*
Copyright 2026 SK Telecom Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command manager runs the operator in a Cluster API management cluster.
package main

import (
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/capi"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/controller"
	"github.com/petasus-ai/nvsentinel-capi-remediator/internal/decision"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		clusterSelector string
		leaderElect     bool
		dryRun          bool
		pollInterval    time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		"The address the metrics endpoint binds to. 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health and readiness probes bind to.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election so that only one replica acts at a time.")
	flag.BoolVar(&dryRun, "dry-run", true,
		"Log every decision but touch no Machine and record no Event.")
	flag.DurationVar(&pollInterval, "poll-interval", controller.DefaultPollInterval,
		"How often each workload cluster's signals are read.")
	flag.StringVar(&clusterSelector, "cluster-selector", "",
		"Label selector limiting which Clusters are watched, e.g. environment=gpu. Empty selects every Cluster.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	var selector labels.Selector
	if clusterSelector != "" {
		s, err := labels.Parse(clusterSelector)
		if err != nil {
			setupLog.Error(err, "invalid --cluster-selector")
			os.Exit(1)
		}
		selector = s
	}

	cacheOpts := cache.Options{}
	if selector != nil {
		// Restricting the Cluster informer also keeps the cluster cache from
		// connecting to Clusters outside the selector.
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&clusterv1.Cluster{}: {Label: selector},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       controller.ControllerName + "." + capi.AnnotationPrefix,
		Cache:                  cacheOpts,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	clusterCache, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
		// The kubeconfig Secret is read once per connection, so an uncached
		// reader avoids holding every Secret of the management cluster in
		// memory.
		SecretClient: mgr.GetAPIReader(),
		Cache: clustercache.CacheOptions{
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		Client: clustercache.ClientOptions{
			UserAgent: controller.ControllerName,
			Cache: clustercache.ClientCacheOptions{
				DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}},
			},
		},
	}, ctrlcontroller.Options{MaxConcurrentReconciles: 10})
	if err != nil {
		setupLog.Error(err, "unable to set up cluster cache")
		os.Exit(1)
	}

	table := decision.Default()
	reconciler := &controller.ClusterReconciler{
		Client:          mgr.GetClient(),
		ClusterCache:    clusterCache,
		Recorder:        mgr.GetEventRecorder(controller.ControllerName),
		Table:           table,
		DryRun:          dryRun,
		PollInterval:    pollInterval,
		ClusterSelector: selector,
	}
	if err := reconciler.SetupWithManager(ctx, mgr, ctrlcontroller.Options{MaxConcurrentReconciles: 1}); err != nil {
		setupLog.Error(err, "unable to set up controller", "controller", controller.ControllerName)
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

	setupLog.Info("starting manager",
		"dryRun", dryRun,
		"pollInterval", pollInterval,
		"clusterSelector", clusterSelector,
		"decisionTable", table.Entries(),
	)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
