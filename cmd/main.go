/*
Copyright 2026.

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

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(cfztv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// validateSiteID enforces the D26 invariant that every operator process carries
// a non-empty --site-id. Identity is a process-level invariant; the per-Exposure
// reasons surface lease/role state, not missing identity.
func validateSiteID(siteID string) error {
	if strings.TrimSpace(siteID) == "" {
		return errors.New("--site-id is required (D26: every operator process must carry a stable site identity)")
	}
	return nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var siteID string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	// D12: leader election required ON. Together with D11 single-writer doc
	// invariant + D19 MaxConcurrentReconciles=1 this guarantees one writer per
	// tunnel-config doc per cluster.
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	// D26: stable identity this operator process uses when arbitrating
	// per-Exposure failover leases. Required; empty is a fatal start-up error.
	flag.StringVar(&siteID, "site-id", "",
		"Stable identity for this operator process. Written into the DR failover lease record "+
			"and compared on every reconcile. Required (D26).")
	// Default to production-grade logging. Override via --zap-log-level=debug.
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := validateSiteID(siteID); err != nil {
		setupLog.Error(err, "Invalid --site-id")
		os.Exit(1)
	}

	// Metrics options configure the controller-runtime metrics server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a82a8396.reid.ee",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.CloudflareTunnelReconciler{
		Base: controller.Base{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: controller.NewEventRecorder(mgr.GetEventRecorder("cloudflaretunnel-controller")),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cloudflaretunnel")
		os.Exit(1)
	}
	httpRouteSourceEnabled, err := controller.HTTPRouteCRDPresent(context.Background(), mgr.GetAPIReader())
	if err != nil {
		setupLog.Error(err, "Failed to discover HTTPRoute CRD")
		os.Exit(1)
	}
	if !httpRouteSourceEnabled {
		setupLog.Info("HTTPRoute CRD not found, controller disabled")
	}

	if err := (&controller.CloudflareExposureReconciler{
		Base: controller.Base{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: controller.NewEventRecorder(mgr.GetEventRecorder("cloudflareexposure-controller")),
		},
		HTTPRouteSourceEnabled: httpRouteSourceEnabled,
		SiteID:                 siteID,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cloudflareexposure")
		os.Exit(1)
	}

	if err := (&controller.CloudflareAccessPolicyReconciler{
		Base: controller.Base{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: controller.NewEventRecorder(mgr.GetEventRecorder("cloudflareaccesspolicy-controller")),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cloudflareaccesspolicy")
		os.Exit(1)
	}
	if err := (&controller.CloudflareTunnelRouteReconciler{
		Base: controller.Base{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: controller.NewEventRecorder(mgr.GetEventRecorder("cloudflaretunnelroute-controller")),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cloudflaretunnelroute")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
