//go:build live

package live

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	conditionReady          = "Ready"
	reasonHostnameConflict  = "HostnameConflict"
	reasonForeignResource   = "ForeignResource"
	reasonForeignRoute      = "ForeignRoute"
	reasonStandby           = "Standby"
	reasonAwaitingPromotion = "AwaitingPromotion"
	annotationForcePromote  = "cfzt.reid.ee/force-promote"

	operatorReleaseName = "cfzt-operator"
	credentialsSecret   = "cloudflare-credentials"
	echoName            = "smoke-echo"
	publicExposure      = "public-smoke"
	accessExposure      = "access-smoke"
	conflictExposure    = "conflict-smoke"

	operatorReadyTimeout = 2 * time.Minute
	echoReadyTimeout     = 2 * time.Minute
	hostnameHTTPTimeout  = 2 * time.Minute
	accessHTTPTimeout    = 5 * time.Minute
	cleanupTimeout       = 4 * time.Minute
	cleanupWaitTimeout   = 2 * time.Minute
)

type smokeConfig struct {
	operatorNamespace       string
	smokeNamespace          string
	repoRoot                string
	releaseTag              string
	version                 string
	runSuffix               string
	tunnelName              string
	accessPolicy            string
	tunnelRoute             string
	tunnelRouteConflict     string
	publicHostname          string
	accessHostname          string
	conflictHostname        string
	failoverHostname        string
	failoverGroup           string
	siteID                  string
	tunnelRouteCIDR         string
	tunnelRouteConflictCIDR string
	chartRef                string
	imageRepository         string
	imageTag                string
	accountID               string
	apiToken                string
	testZone                string
	zoneID                  string
}

type smokeHarness struct {
	t               *testing.T
	ctx             context.Context
	cfg             smokeConfig
	cf              cloudflare.Client
	k8s             client.Client
	foreignRecordID string
	foreignRouteID  string
	// failoverPolicy is stamped onto the failover Exposure spec by
	// failoverExposureObject so it survives the repeated noop spec updates the
	// failover phases use to trigger reconciles. Empty => Automatic (default).
	failoverPolicy cfztv1alpha1.PromotionPolicy
}

func loadSmokeConfig(t *testing.T) smokeConfig {
	t.Helper()

	releaseTag := envDefault("GITHUB_REF_NAME", "local")
	version := strings.TrimPrefix(releaseTag, "v")
	runID := envDefault("GITHUB_RUN_ID", fmt.Sprintf("local-%d", time.Now().Unix()))
	runAttempt := envDefault("GITHUB_RUN_ATTEMPT", "1")
	testZone := requiredEnv(t, "CF_TEST_ZONE")
	runSuffix := runID + "-" + runAttempt

	routeCIDR := canonicalSmokeCIDR(t, "CF_SMOKE_ROUTE_CIDR", envDefault("CF_SMOKE_ROUTE_CIDR", "100.64.207.0/24"))
	routeConflictCIDR := canonicalSmokeCIDR(t, "CF_SMOKE_ROUTE_CONFLICT_CIDR", envDefault("CF_SMOKE_ROUTE_CONFLICT_CIDR", "100.64.208.0/24"))

	cfg := smokeConfig{
		operatorNamespace:       envDefault("OPERATOR_NAMESPACE", "cfzt-system"),
		smokeNamespace:          envDefault("SMOKE_NAMESPACE", "cfzt-smoke"),
		repoRoot:                repoRoot(t),
		releaseTag:              releaseTag,
		version:                 version,
		runSuffix:               runSuffix,
		tunnelName:              "cfzt-smoke-" + runSuffix,
		accessPolicy:            "cfzt-smoke-policy-" + runSuffix,
		tunnelRoute:             "cfzt-smoke-route-" + runSuffix,
		tunnelRouteConflict:     "cfzt-smoke-route-conflict-" + runSuffix,
		publicHostname:          "public-" + runSuffix + "." + testZone,
		accessHostname:          "access-" + runSuffix + "." + testZone,
		conflictHostname:        "conflict-" + runSuffix + "." + testZone,
		failoverHostname:        "failover-" + runSuffix + "." + testZone,
		failoverGroup:           "cfzt-smoke-fo-" + runSuffix,
		siteID:                  envDefault("SITE_ID", "cfzt-smoke-"+runSuffix),
		tunnelRouteCIDR:         routeCIDR,
		tunnelRouteConflictCIDR: routeConflictCIDR,
		chartRef:                envDefault("CHART_REF", "oci://ghcr.io/andrewreid/charts/cfzt-operator"),
		imageRepository:         envDefault("IMAGE_REPOSITORY", "ghcr.io/andrewreid/cfzt-operator"),
		imageTag:                envDefault("IMAGE_TAG", releaseTag),
		accountID:               requiredEnv(t, "CF_ACCOUNT_ID"),
		apiToken:                requiredEnv(t, "CF_API_TOKEN"),
		testZone:                testZone,
		zoneID:                  os.Getenv("CF_ZONE_ID"),
	}
	maskSecret(cfg.accountID)
	maskSecret(cfg.apiToken)
	maskSecret(cfg.zoneID)
	return cfg
}

func canonicalSmokeCIDR(t *testing.T, name, value string) string {
	t.Helper()
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		t.Fatalf("%s must be a valid CIDR prefix: %v", name, err)
	}
	return prefix.Masked().String()
}

func newCloudflareClient(t *testing.T, cfg smokeConfig) cloudflare.Client {
	t.Helper()
	cfClient, err := cloudflare.New(cfg.accountID, cfg.apiToken)
	if err != nil {
		t.Fatalf("create Cloudflare client: %v", err)
	}
	return cfClient
}

func newKubeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	must(t, clientgoscheme.AddToScheme(scheme), "add Kubernetes scheme")
	must(t, cfztv1alpha1.AddToScheme(scheme), "add cfzt scheme")
	restConfig, err := config.GetConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	k8s, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	return k8s
}

func resolveZoneID(t *testing.T, ctx context.Context, cfClient cloudflare.Client, cfg smokeConfig) string {
	t.Helper()
	if cfg.zoneID != "" {
		t.Log("using Cloudflare zone ID from CF_ZONE_ID")
		return cfg.zoneID
	}
	t.Logf("resolving Cloudflare zone for %s", cfg.testZone)
	zone, err := cfClient.Zones().Resolve(ctx, cfg.testZone)
	if err != nil {
		t.Fatalf("resolve Cloudflare zone for %s: %v; set CF_ZONE_ID if the token cannot list zones", cfg.testZone, err)
	}
	return zone.ID
}

func (h *smokeHarness) installOperator() {
	h.t.Logf("installing released Helm chart %s %s", h.cfg.chartRef, h.cfg.version)
	h.ensureNamespace(h.cfg.operatorNamespace)
	h.ensureNamespace(h.cfg.smokeNamespace)
	h.ensureCredentialsSecret()
	h.applyLocalChartCRDs()
	args := []string{"upgrade", "--install", operatorReleaseName, h.cfg.chartRef,
		"--namespace", h.cfg.operatorNamespace,
		"--create-namespace",
		"--set", "image.repository=" + h.cfg.imageRepository,
		"--set", "image.tag=" + h.cfg.imageTag,
		"--set", "image.pullPolicy=Never",
		"--set", "replicaCount=1",
		"--set", "site.id=" + h.cfg.siteID,
		"--wait",
		"--timeout", "2m"}
	if !isLocalChartRef(h.cfg.chartRef) {
		args = append(args[:4], append([]string{"--version", h.cfg.version}, args[4:]...)...)
	}
	h.run("helm", args...)
	h.restartOperator()
}

func (h *smokeHarness) applyLocalChartCRDs() {
	chartDir, ok := h.localChartDir()
	if !ok {
		return
	}
	crdDir := filepath.Join(chartDir, "crds")
	info, err := os.Stat(crdDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		h.t.Fatalf("stat local chart CRD directory: %v", err)
	}
	if !info.IsDir() {
		return
	}
	h.t.Logf("applying local chart CRDs from %s", crdDir)
	h.run("kubectl", "apply", "-f", crdDir)
}

func (h *smokeHarness) localChartDir() (string, bool) {
	if !isLocalChartRef(h.cfg.chartRef) {
		return "", false
	}
	chartPath := h.cfg.chartRef
	if !filepath.IsAbs(chartPath) {
		chartPath = filepath.Clean(filepath.Join(h.cfg.repoRoot, chartPath))
	}
	info, err := os.Stat(chartPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		h.t.Fatalf("stat local chart reference: %v", err)
	}
	if !info.IsDir() {
		return "", false
	}
	return chartPath, true
}

func (h *smokeHarness) deployEcho() {
	h.t.Log("deploying echo workload")
	labels := map[string]string{"app": echoName}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: echoName, Namespace: h.cfg.smokeNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "agnhost",
						Image: "registry.k8s.io/e2e-test-images/agnhost:2.53",
						Args:  []string{"netexec", "--http-port=8080"},
						Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
					}},
				},
			},
		},
	}
	h.createOrUpdate(deploy, func() {})

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: echoName, Namespace: h.cfg.smokeNamespace},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8080,
				TargetPort: intstr.FromInt32(8080),
			}},
		},
	}
	h.createOrUpdate(service, func() {})
	h.waitDeploymentAvailable(h.cfg.smokeNamespace, echoName, echoReadyTimeout)
}

func (h *smokeHarness) createAccessPolicy() {
	policy := h.accessPolicyObject()
	if err := h.k8s.Create(h.ctx, policy); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create CloudflareAccessPolicy: %v", err)
	}
}

func (h *smokeHarness) updateAccessPolicyNoop() {
	var policy cfztv1alpha1.CloudflareAccessPolicy
	h.get(types.NamespacedName{Name: h.cfg.accessPolicy}, &policy)
	policy.Spec = h.accessPolicyObject().Spec
	must(h.t, h.k8s.Update(h.ctx, &policy), "update CloudflareAccessPolicy")
}

func (h *smokeHarness) accessPolicyObject() *cfztv1alpha1.CloudflareAccessPolicy {
	return &cfztv1alpha1.CloudflareAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: h.cfg.accessPolicy},
		Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
			CredentialsSecretRef: cfztv1alpha1.AccessPolicyCredentialsSecretRef{
				Namespace: h.cfg.operatorNamespace,
				Name:      credentialsSecret,
			},
			PolicyName: h.cfg.accessPolicy,
			Decision:   "allow",
			Rules: cfztv1alpha1.AccessRules{
				Include: []cfztv1alpha1.AccessRule{{EmailDomain: h.cfg.testZone}},
			},
			SessionDuration: "24h",
		},
	}
}

func (h *smokeHarness) createTunnel() {
	tunnel := h.tunnelObject()
	if err := h.k8s.Create(h.ctx, tunnel); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create CloudflareTunnel: %v", err)
	}
}

func (h *smokeHarness) updateTunnelNoop() {
	var tunnel cfztv1alpha1.CloudflareTunnel
	h.get(types.NamespacedName{Name: h.cfg.tunnelName}, &tunnel)
	tunnel.Spec = h.tunnelObject().Spec
	must(h.t, h.k8s.Update(h.ctx, &tunnel), "update CloudflareTunnel")
}

func (h *smokeHarness) tunnelObject() *cfztv1alpha1.CloudflareTunnel {
	return &cfztv1alpha1.CloudflareTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelName},
		Spec: cfztv1alpha1.CloudflareTunnelSpec{
			TunnelName: h.cfg.tunnelName,
			CredentialsSecretRef: cfztv1alpha1.CredentialsSecretRef{
				Name: credentialsSecret,
			},
			Dns: cfztv1alpha1.DnsSpec{Manage: true},
			Cloudflared: cfztv1alpha1.CloudflaredSpec{
				Namespace: h.cfg.operatorNamespace,
			},
		},
	}
}

func (h *smokeHarness) createTunnelRoute() {
	route := h.tunnelRouteObject(h.cfg.tunnelRoute, h.cfg.tunnelRouteCIDR)
	if err := h.k8s.Create(h.ctx, route); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create CloudflareTunnelRoute: %v", err)
	}
}

func (h *smokeHarness) updateTunnelRouteNoop() {
	var route cfztv1alpha1.CloudflareTunnelRoute
	h.get(types.NamespacedName{Name: h.cfg.tunnelRoute}, &route)
	route.Spec = h.tunnelRouteObject(h.cfg.tunnelRoute, h.cfg.tunnelRouteCIDR).Spec
	must(h.t, h.k8s.Update(h.ctx, &route), "update CloudflareTunnelRoute")
}

func (h *smokeHarness) createConflictTunnelRoute() {
	route := h.tunnelRouteObject(h.cfg.tunnelRouteConflict, h.cfg.tunnelRouteConflictCIDR)
	if err := h.k8s.Create(h.ctx, route); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create conflict CloudflareTunnelRoute: %v", err)
	}
}

func (h *smokeHarness) tunnelRouteObject(name, network string) *cfztv1alpha1.CloudflareTunnelRoute {
	return &cfztv1alpha1.CloudflareTunnelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareTunnelRouteSpec{
			TunnelRef: cfztv1alpha1.TunnelRouteTunnelRef{Name: h.cfg.tunnelName},
			Network:   network,
			Comment:   "live smoke",
		},
	}
}

func (h *smokeHarness) createExposures() {
	for _, exposure := range []*cfztv1alpha1.CloudflareExposure{h.publicExposureObject(), h.accessExposureObject()} {
		if err := h.k8s.Create(h.ctx, exposure); err != nil && !apierrors.IsAlreadyExists(err) {
			h.t.Fatalf("create CloudflareExposure %s: %v", exposure.Name, err)
		}
	}
}

func (h *smokeHarness) updateExposuresNoop() {
	for _, desired := range []*cfztv1alpha1.CloudflareExposure{h.publicExposureObject(), h.accessExposureObject()} {
		var exposure cfztv1alpha1.CloudflareExposure
		h.get(types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: desired.Name}, &exposure)
		exposure.Spec = desired.Spec
		must(h.t, h.k8s.Update(h.ctx, &exposure), "update CloudflareExposure "+desired.Name)
	}
}

func (h *smokeHarness) publicExposureObject() *cfztv1alpha1.CloudflareExposure {
	return h.exposureObject(publicExposure, h.cfg.publicHostname, false)
}

func (h *smokeHarness) accessExposureObject() *cfztv1alpha1.CloudflareExposure {
	return h.exposureObject(accessExposure, h.cfg.accessHostname, true)
}

func (h *smokeHarness) createConflictExposure() {
	exposure := h.exposureObject(conflictExposure, h.cfg.conflictHostname, false)
	if err := h.k8s.Create(h.ctx, exposure); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create conflict CloudflareExposure: %v", err)
	}
}

func (h *smokeHarness) exposureObject(name, hostname string, accessEnabled bool) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.smokeNamespace},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			TunnelRef: cfztv1alpha1.TunnelRef{Name: h.cfg.tunnelName},
			Hostname:  hostname,
			SourceRef: &cfztv1alpha1.SourceRef{
				ApiVersion: "v1",
				Kind:       "Service",
				Name:       echoName,
			},
			Access: cfztv1alpha1.AccessSpec{Enabled: accessEnabled},
		},
	}
	if accessEnabled {
		exposure.Spec.Access.Applications = h.accessApplications(hostname)
	}
	return exposure
}

func (h *smokeHarness) accessApplications(hostname string) []cfztv1alpha1.AccessApplicationTarget {
	return []cfztv1alpha1.AccessApplicationTarget{
		{
			Name:    "root",
			Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain(hostname)},
			Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
				PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: h.cfg.accessPolicy},
			}},
		},
		{
			Name: "admin",
			Domains: []cfztv1alpha1.AccessApplicationDomain{
				cfztv1alpha1.AccessApplicationDomain(hostname + "/admin"),
			},
			Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
				PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: h.cfg.accessPolicy},
			}},
		},
	}
}

func (h *smokeHarness) waitAccessPolicyReady(timeout time.Duration) cfztv1alpha1.CloudflareAccessPolicy {
	var policy cfztv1alpha1.CloudflareAccessPolicy
	h.waitFor("CloudflareAccessPolicy ready", timeout, func() (bool, error) {
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Name: h.cfg.accessPolicy}, &policy); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(policy.Status.Conditions, conditionReady)
		return policy.Status.PolicyId != "" && ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == policy.Generation, nil
	})
	return policy
}

func (h *smokeHarness) waitTunnelReady(timeout time.Duration) cfztv1alpha1.CloudflareTunnel {
	var tunnel cfztv1alpha1.CloudflareTunnel
	h.waitFor("CloudflareTunnel ready", timeout, func() (bool, error) {
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Name: h.cfg.tunnelName}, &tunnel); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(tunnel.Status.Conditions, conditionReady)
		return tunnel.Status.TunnelId != "" && ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == tunnel.Generation, nil
	})
	return tunnel
}

func (h *smokeHarness) waitExposureReady(name string, timeout time.Duration) cfztv1alpha1.CloudflareExposure {
	var exposure cfztv1alpha1.CloudflareExposure
	h.waitFor("CloudflareExposure "+name+" ready", timeout, func() (bool, error) {
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: name}, &exposure); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(exposure.Status.Conditions, conditionReady)
		return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == exposure.Generation, nil
	})
	return exposure
}

func (h *smokeHarness) waitTunnelRouteReady(timeout time.Duration) cfztv1alpha1.CloudflareTunnelRoute {
	var route cfztv1alpha1.CloudflareTunnelRoute
	h.waitFor("CloudflareTunnelRoute ready", timeout, func() (bool, error) {
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Name: h.cfg.tunnelRoute}, &route); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(route.Status.Conditions, conditionReady)
		return route.Status.RouteId != "" && ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == route.Generation, nil
	})
	return route
}

func (h *smokeHarness) waitTunnelRouteForeignReason(timeout time.Duration) string {
	var reason string
	h.waitFor("CloudflareTunnelRoute foreign conflict", timeout, func() (bool, error) {
		var route cfztv1alpha1.CloudflareTunnelRoute
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Name: h.cfg.tunnelRouteConflict}, &route); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(route.Status.Conditions, conditionReady)
		if ready == nil {
			return false, nil
		}
		reason = ready.Reason
		return reason == reasonForeignRoute, nil
	})
	return reason
}

func (h *smokeHarness) waitExposureConflictReason(name string, timeout time.Duration) string {
	var reason string
	// Slice 6 adjustment: subtask 14 changed waiting conflicts to fixed 30s
	// requeues. Keep these waits comfortably above that floor, and accept both
	// hostname and finalizer foreign-resource reasons for ownership conflicts.
	h.waitFor("CloudflareExposure "+name+" conflict", timeout, func() (bool, error) {
		var exposure cfztv1alpha1.CloudflareExposure
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: name}, &exposure); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(exposure.Status.Conditions, conditionReady)
		if ready == nil {
			return false, nil
		}
		reason = ready.Reason
		return reason == reasonHostnameConflict || reason == reasonForeignResource, nil
	})
	return reason
}

func (h *smokeHarness) waitDeploymentAvailable(namespace, name string, timeout time.Duration) {
	h.waitFor("Deployment "+namespace+"/"+name+" available", timeout, func() (bool, error) {
		var deploy appsv1.Deployment
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Namespace: namespace, Name: name}, &deploy); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		desired := int32(1)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}
		return deploy.Status.ObservedGeneration >= deploy.Generation &&
			deploy.Status.UpdatedReplicas == desired &&
			deploy.Status.AvailableReplicas == desired &&
			deploy.Status.UnavailableReplicas == 0, nil
	})
}

func (h *smokeHarness) waitDaemonSetReady(name string, timeout time.Duration) {
	h.waitFor("DaemonSet "+h.cfg.operatorNamespace+"/"+name+" ready", timeout, func() (bool, error) {
		var ds appsv1.DaemonSet
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Namespace: h.cfg.operatorNamespace, Name: name}, &ds); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return ds.Status.ObservedGeneration >= ds.Generation && ds.Status.NumberReady > 0, nil
	})
}

func (h *smokeHarness) waitPublicRoute() {
	httpClient := smokeHTTPClient()
	h.t.Logf("waiting for public route https://%s/hostname", h.cfg.publicHostname)
	h.waitFor("public hostname HTTP 200", hostnameHTTPTimeout, func() (bool, error) {
		req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "https://"+h.cfg.publicHostname+"/hostname", nil)
		if err != nil {
			return false, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return resp.StatusCode == http.StatusOK && len(strings.TrimSpace(string(body))) > 0, nil
	})
}

func (h *smokeHarness) assertAccessChallenged(path string) {
	httpClient := smokeHTTPClient()
	h.t.Logf("checking unauthenticated Access response for https://%s%s", h.cfg.accessHostname, path)
	var lastStatus int
	var lastLocation string
	var lastBody string
	var lastErr error
	err := wait.PollUntilContextTimeout(h.ctx, 5*time.Second, accessHTTPTimeout, true, func(context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "https://"+h.cfg.accessHostname+path, nil)
		if err != nil {
			return false, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			return false, nil
		}
		defer resp.Body.Close()
		lastStatus = resp.StatusCode
		lastLocation = resp.Header.Get("Location")
		lastErr = nil
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if readErr != nil {
			lastErr = readErr
		}
		lastBody = strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusUnauthorized, http.StatusForbidden:
			return true, nil
		case http.StatusOK:
			return false, fmt.Errorf("Access hostname returned HTTP 200; policy may be bypassing unauthenticated users")
		default:
			return false, nil
		}
	})
	if err != nil {
		h.dumpDiagnostics("Access hostname challenge")
		h.t.Fatalf("timed out waiting for Access hostname challenge: %v; last status=%d location=%q transport/read error=%v body=%q", err, lastStatus, lastLocation, lastErr, lastBody)
	}
	h.t.Logf("Access path returned expected unauthenticated status %d location=%q", lastStatus, lastLocation)
}

func (h *smokeHarness) assertOneAccessPolicy(policyID string) {
	policies, err := h.cf.AccessPolicies().List(h.ctx)
	if err != nil {
		h.t.Fatalf("list Access policies: %v", err)
	}
	// Slice 6 adjustment: managed policyName is a base name; Cloudflare-side
	// policy names always carry the operator suffix.
	wantName := h.expectedAccessPolicyName()
	var matches []cloudflare.AccessPolicy
	for _, policy := range policies {
		if policy.Name == wantName {
			matches = append(matches, policy)
		}
	}
	if len(matches) != 1 {
		h.t.Fatalf("expected exactly one managed Access policy named %s, got %d", wantName, len(matches))
	}
	assertEqual(h.t, "managed Access policy ID", policyID, matches[0].ID)
}

func (h *smokeHarness) accessApplicationStatus(statuses []cfztv1alpha1.ExposureAccessApplicationStatus, name string) cfztv1alpha1.ExposureAccessApplicationStatus {
	for _, status := range statuses {
		if status.Name == name {
			return status
		}
	}
	h.t.Fatalf("missing Access application status entry %q in %v", name, statuses)
	return cfztv1alpha1.ExposureAccessApplicationStatus{}
}

func (h *smokeHarness) assertAccessApplications(statuses []cfztv1alpha1.ExposureAccessApplicationStatus, policyID string) {
	if len(statuses) != 2 {
		h.t.Fatalf("expected two Access application status entries, got %d: %v", len(statuses), statuses)
	}
	rootStatus := h.accessApplicationStatus(statuses, "root")
	adminStatus := h.accessApplicationStatus(statuses, "admin")
	for _, status := range []cfztv1alpha1.ExposureAccessApplicationStatus{rootStatus, adminStatus} {
		if status.AppID == "" {
			h.t.Fatalf("Access application status entry %q missing app ID: %v", status.Name, statuses)
		}
		if status.CanonicalDomainHash == "" {
			h.t.Fatalf("Access application status entry %q missing domain hash: %v", status.Name, statuses)
		}
		if status.PolicyHash == "" {
			h.t.Fatalf("Access application status entry %q missing policy hash: %v", status.Name, statuses)
		}
	}
	apps, err := h.cf.AccessApplications().List(h.ctx, h.cfg.accessHostname)
	if err != nil {
		h.t.Fatalf("list Access applications for %s: %v", h.cfg.accessHostname, err)
	}
	if len(apps) != 2 {
		h.t.Fatalf("expected exactly two Access applications for %s, got %d: %v", h.cfg.accessHostname, len(apps), apps)
	}
	var rootApp *cloudflare.AccessApplication
	var adminApp *cloudflare.AccessApplication
	for i := range apps {
		app := &apps[i]
		switch app.Name {
		case accessExposure + "-root-cfzt":
			rootApp = app
		case accessExposure + "-admin-cfzt":
			adminApp = app
		}
	}
	if rootApp == nil || adminApp == nil {
		h.t.Fatalf("expected root and admin Access applications for %s, got %v", h.cfg.accessHostname, apps)
	}
	assertEqual(h.t, "root Access application domain", h.cfg.accessHostname, rootApp.Domain)
	assertEqual(h.t, "root Access application name", accessExposure+"-root-cfzt", rootApp.Name)
	assertEqual(h.t, "admin Access application domain", h.cfg.accessHostname+"/admin", adminApp.Domain)
	assertEqual(h.t, "admin Access application name", accessExposure+"-admin-cfzt", adminApp.Name)
	for _, app := range []*cloudflare.AccessApplication{rootApp, adminApp} {
		if len(app.PolicyUUIDs) != 1 || app.PolicyUUIDs[0] != policyID {
			h.t.Fatalf("expected Access application %s policies [%s], got %v", app.Name, policyID, app.PolicyUUIDs)
		}
		if len(app.Domains) != 1 {
			h.t.Fatalf("expected Access application %s to have exactly one domain, got %v", app.Name, app.Domains)
		}
		if !containsString(app.Tags, "managed-by=cfzt-operator") {
			h.t.Fatalf("Access application missing managed-by tag: %v", app.Tags)
		}
		var hasSourceUID bool
		for _, tag := range app.Tags {
			if strings.HasPrefix(tag, "source-uid-0=") {
				hasSourceUID = true
				break
			}
		}
		if !hasSourceUID {
			h.t.Fatalf("Access application missing source UID chunk tag: %v", app.Tags)
		}
	}
	rootStatusID := h.accessApplicationStatus(statuses, "root").AppID
	adminStatusID := h.accessApplicationStatus(statuses, "admin").AppID
	if rootStatusID != rootApp.ID {
		h.t.Fatalf("root Access application status app ID = %s, want %s", rootStatusID, rootApp.ID)
	}
	if adminStatusID != adminApp.ID {
		h.t.Fatalf("admin Access application status app ID = %s, want %s", adminStatusID, adminApp.ID)
	}
}

func (h *smokeHarness) expectedAccessPolicyName() string {
	return h.cfg.accessPolicy + "-cfzt"
}

func (h *smokeHarness) expectedTunnelName(tunnel cfztv1alpha1.CloudflareTunnel) string {
	sum := sha256.Sum256([]byte(string(tunnel.UID)))
	return fmt.Sprintf("%s-cfzt-%x", tunnel.Spec.TunnelName, sum[:4])
}

func (h *smokeHarness) assertTunnelName(tunnel cfztv1alpha1.CloudflareTunnel) {
	cfTunnel, err := h.cf.Tunnels().Get(h.ctx, tunnel.Status.TunnelId)
	if err != nil {
		h.t.Fatalf("get Cloudflare tunnel %s: %v", tunnel.Status.TunnelId, err)
	}
	assertEqual(h.t, "Cloudflare tunnel name", h.expectedTunnelName(tunnel), cfTunnel.Name)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (h *smokeHarness) assertOneTunnelRoute(routeID, tunnelID, network string) {
	routes := h.listTunnelRoutes(network)
	var matches []cloudflare.TunnelRoute
	for _, route := range routes {
		if route.TunnelID == tunnelID {
			matches = append(matches, route)
		}
	}
	if len(matches) != 1 {
		h.t.Fatalf("expected exactly one tunnel route for %s, got %d", network, len(matches))
	}
	assertEqual(h.t, "tunnel route ID", routeID, matches[0].ID)
	if !strings.Contains(matches[0].Comment, "managed-by=cfzt source-uid=") {
		h.t.Fatalf("tunnel route comment missing ownership tag: %q", matches[0].Comment)
	}
}

func (h *smokeHarness) listTunnelRoutes(network string) []cloudflare.TunnelRoute {
	routes, err := h.cf.TunnelRoutes().List(h.ctx, cloudflare.ListTunnelRoutesFilter{Network: network})
	if err != nil {
		h.t.Fatalf("list tunnel routes for %s: %v", network, err)
	}
	return routes
}

func (h *smokeHarness) createForeignTunnelRoute(tunnelID string) cloudflare.TunnelRoute {
	candidates := uniqueCIDRs(
		h.cfg.tunnelRouteConflictCIDR,
		"198.18.208.0/24",
		"198.19.208.0/24",
		"10.255.208.0/24",
		"172.31.208.0/24",
	)
	var failures []string
	for _, network := range candidates {
		if network == h.cfg.tunnelRouteCIDR {
			continue
		}
		route, err := h.cf.TunnelRoutes().Create(h.ctx, cloudflare.TunnelRouteInput{
			Network:  network,
			TunnelID: tunnelID,
			Comment:  "cfzt-live-smoke-foreign-route",
		})
		if err == nil {
			h.cfg.tunnelRouteConflictCIDR = network
			h.foreignRouteID = route.ID
			h.t.Logf("created foreign tunnel route %s for %s", route.ID, network)
			return *route
		}
		failures = append(failures, fmt.Sprintf("%s: %v", network, err))
	}
	h.t.Fatalf("create foreign tunnel route: all candidate CIDRs failed: %s", strings.Join(failures, "; "))
	return cloudflare.TunnelRoute{}
}

func uniqueCIDRs(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (h *smokeHarness) restartOperator() {
	var deploy appsv1.Deployment
	h.get(types.NamespacedName{Namespace: h.cfg.operatorNamespace, Name: operatorReleaseName}, &deploy)
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	deploy.Spec.Template.Annotations["cfzt.reid.ee/live-smoke-restarted-at"] = time.Now().UTC().Format(time.RFC3339Nano)
	must(h.t, h.k8s.Update(h.ctx, &deploy), "restart operator Deployment")
	h.waitDeploymentAvailable(h.cfg.operatorNamespace, operatorReleaseName, operatorReadyTimeout)
}

func (h *smokeHarness) cleanup() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	h.t.Log("cleanup: deleting CloudflareExposure resources")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: publicExposure, Namespace: h.cfg.smokeNamespace}})
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: accessExposure, Namespace: h.cfg.smokeNamespace}})
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: conflictExposure, Namespace: h.cfg.smokeNamespace}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: publicExposure, Namespace: h.cfg.smokeNamespace}, cleanupWaitTimeout)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: accessExposure, Namespace: h.cfg.smokeNamespace}, cleanupWaitTimeout)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: conflictExposure, Namespace: h.cfg.smokeNamespace}, cleanupWaitTimeout)

	h.t.Log("cleanup: deleting CloudflareTunnelRoute resources")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelRoute}})
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelRouteConflict}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{}, types.NamespacedName{Name: h.cfg.tunnelRoute}, cleanupWaitTimeout)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{}, types.NamespacedName{Name: h.cfg.tunnelRouteConflict}, cleanupWaitTimeout)

	if h.foreignRouteID != "" {
		h.t.Log("cleanup: deleting foreign conflict tunnel route")
		if err := h.cf.TunnelRoutes().Delete(cleanupCtx, h.foreignRouteID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			h.t.Errorf("delete foreign tunnel route: %v", err)
		}
		h.waitTunnelRouteAbsent(cleanupCtx, "foreign tunnel route", h.cfg.tunnelRouteConflictCIDR, cleanupWaitTimeout)
	}

	h.t.Log("cleanup: deleting CloudflareAccessPolicy")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.accessPolicy}})
	h.deleteAccessPoliciesByName(cleanupCtx, h.expectedAccessPolicyName())
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareAccessPolicy{}, types.NamespacedName{Name: h.cfg.accessPolicy}, cleanupWaitTimeout)

	h.t.Log("cleanup: deleting CloudflareTunnel")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnel{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelName}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnel{}, types.NamespacedName{Name: h.cfg.tunnelName}, cleanupWaitTimeout)

	if h.foreignRecordID != "" {
		h.t.Log("cleanup: deleting foreign conflict DNS record")
		if err := h.cf.DNSRecords().Delete(cleanupCtx, h.cfg.zoneID, h.foreignRecordID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			h.t.Errorf("delete foreign DNS record: %v", err)
		}
	}
	h.waitDNSAbsent(cleanupCtx, "public DNS record", h.cfg.publicHostname, cleanupWaitTimeout)
	h.waitDNSAbsent(cleanupCtx, "access DNS record", h.cfg.accessHostname, cleanupWaitTimeout)
	h.waitTunnelRouteAbsent(cleanupCtx, "tunnel route", h.cfg.tunnelRouteCIDR, cleanupWaitTimeout)
	h.waitAccessApplicationsAbsent(cleanupCtx, cleanupWaitTimeout)
	h.waitAccessPolicyAbsent(cleanupCtx, cleanupWaitTimeout)
	h.deleteTunnelsByNamePrefix(cleanupCtx, h.cfg.tunnelName+"-cfzt-")
	h.waitTunnelAbsent(cleanupCtx, cleanupWaitTimeout)
}

func (h *smokeHarness) waitTunnelRouteAbsent(ctx context.Context, description, network string, timeout time.Duration) {
	h.waitForContext(ctx, description+" absent", timeout, func() (bool, error) {
		routes, err := h.cf.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{Network: network})
		if err != nil {
			return false, err
		}
		return len(routes) == 0, nil
	})
	h.t.Log(description + " absent")
}

func (h *smokeHarness) waitDNSAbsent(ctx context.Context, description, hostname string, timeout time.Duration) {
	h.waitForContext(ctx, description+" absent", timeout, func() (bool, error) {
		records, err := h.cf.DNSRecords().List(ctx, h.cfg.zoneID, hostname, "CNAME")
		if err != nil {
			return false, err
		}
		return len(records) == 0, nil
	})
	h.t.Log(description + " absent")
}

func (h *smokeHarness) waitAccessApplicationsAbsent(ctx context.Context, timeout time.Duration) {
	h.waitForContext(ctx, "Access application absent", timeout, func() (bool, error) {
		apps, err := h.cf.AccessApplications().List(ctx, h.cfg.accessHostname)
		if err != nil {
			return false, err
		}
		return len(apps) == 0, nil
	})
	h.t.Log("Access application absent")
}

func (h *smokeHarness) waitAccessPolicyAbsent(ctx context.Context, timeout time.Duration) {
	h.waitForContext(ctx, "Access policy absent", timeout, func() (bool, error) {
		policies, err := h.cf.AccessPolicies().List(ctx)
		if err != nil {
			return false, err
		}
		for _, policy := range policies {
			if policy.Name == h.expectedAccessPolicyName() {
				return false, nil
			}
		}
		return true, nil
	})
	h.t.Log("Access policy absent")
}

func (h *smokeHarness) deleteAccessPoliciesByName(ctx context.Context, name string) {
	policies, err := h.cf.AccessPolicies().List(ctx)
	if err != nil {
		h.t.Errorf("cleanup: list Access policies for direct name cleanup: %v", err)
		return
	}
	for _, policy := range policies {
		if policy.Name != name {
			continue
		}
		h.t.Logf("cleanup: deleting lingering Access policy by name %s (%s)", policy.Name, policy.ID)
		if err := h.cf.AccessPolicies().Delete(ctx, policy.ID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			h.t.Errorf("cleanup: delete lingering Access policy %s: %v", policy.ID, err)
		}
	}
}

func (h *smokeHarness) deleteTunnelsByNamePrefix(ctx context.Context, prefix string) {
	tunnels, err := h.cf.Tunnels().List(ctx, "")
	if err != nil {
		h.t.Errorf("cleanup: list tunnels for direct generated-name cleanup: %v", err)
		return
	}
	for _, tunnel := range tunnels {
		if !strings.HasPrefix(tunnel.Name, prefix) {
			continue
		}
		h.t.Logf("cleanup: deleting lingering generated tunnel %s (%s)", tunnel.Name, tunnel.ID)
		if err := h.cf.Tunnels().Delete(ctx, tunnel.ID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			h.t.Errorf("cleanup: delete lingering generated tunnel %s: %v", tunnel.ID, err)
		}
	}
}

func (h *smokeHarness) waitTunnelAbsent(ctx context.Context, timeout time.Duration) {
	h.waitForContext(ctx, "Cloudflare tunnel absent", timeout, func() (bool, error) {
		tunnels, err := h.cf.Tunnels().List(ctx, "")
		if err != nil {
			return false, err
		}
		for _, tunnel := range tunnels {
			if tunnel.Name == h.cfg.tunnelName || strings.HasPrefix(tunnel.Name, h.cfg.tunnelName+"-cfzt-") {
				return false, nil
			}
		}
		return true, nil
	})
	h.t.Log("Cloudflare tunnel absent")
}

func (h *smokeHarness) ensureNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := h.k8s.Create(h.ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create namespace %s: %v", name, err)
	}
}

func (h *smokeHarness) ensureCredentialsSecret() {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentialsSecret, Namespace: h.cfg.operatorNamespace}}
	h.createOrUpdate(secret, func() {
		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			"accountId": h.cfg.accountID,
			"apiToken":  h.cfg.apiToken,
		}
		secret.Data = nil
	})
}

func (h *smokeHarness) createOrUpdate(obj client.Object, mutate func()) {
	key := client.ObjectKeyFromObject(obj)
	current := obj.DeepCopyObject().(client.Object)
	err := h.k8s.Get(h.ctx, key, current)
	if apierrors.IsNotFound(err) {
		mutate()
		must(h.t, h.k8s.Create(h.ctx, obj), "create "+key.String())
		return
	}
	must(h.t, err, "get "+key.String())
	obj.SetResourceVersion(current.GetResourceVersion())
	preserveImmutableServiceFields(obj, current)
	mutate()
	must(h.t, h.k8s.Update(h.ctx, obj), "update "+key.String())
}

func (h *smokeHarness) deleteObject(ctx context.Context, obj client.Object) {
	if err := h.k8s.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		h.t.Errorf("delete %T %s/%s: %v", obj, obj.GetNamespace(), obj.GetName(), err)
	}
}

func (h *smokeHarness) waitObjectAbsent(ctx context.Context, obj client.Object, key types.NamespacedName, timeout time.Duration) {
	h.waitForContext(ctx, fmt.Sprintf("%T %s absent", obj, key.String()), timeout, func() (bool, error) {
		err := h.k8s.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return true, nil
		}
		return false, err
	})
}

func (h *smokeHarness) get(key types.NamespacedName, obj client.Object) {
	must(h.t, h.k8s.Get(h.ctx, key, obj), "get "+key.String())
}

func (h *smokeHarness) waitFor(description string, timeout time.Duration, condition func() (bool, error)) {
	h.waitForContext(h.ctx, description, timeout, condition)
}

func (h *smokeHarness) waitForContext(ctx context.Context, description string, timeout time.Duration, condition func() (bool, error)) {
	h.t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(context.Context) (bool, error) {
		return condition()
	})
	if err != nil {
		h.dumpDiagnostics(description)
		h.t.Fatalf("timed out waiting for %s: %v", description, err)
	}
}

func (h *smokeHarness) run(name string, args ...string) {
	h.t.Helper()
	cmd := exec.CommandContext(h.ctx, name, args...)
	cmd.Dir = h.cfg.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, string(output))
	}
	if len(output) > 0 {
		h.t.Logf("%s", strings.TrimSpace(string(output)))
	}
}

func (h *smokeHarness) dumpDiagnostics(description string) {
	h.t.Helper()
	h.t.Logf("diagnostics after waiting for %s", description)
	commands := [][]string{
		{"kubectl", "get", "cloudflareaccesspolicies", "-o", "yaml"},
		{"kubectl", "get", "cloudflaretunnels", "-o", "yaml"},
		{"kubectl", "get", "cloudflaretunnelroutes", "-o", "yaml"},
		{"kubectl", "get", "cloudflareexposures", "-A", "-o", "yaml"},
		{"kubectl", "-n", h.cfg.operatorNamespace, "get", "deploy,pods,events", "-o", "wide"},
		{"kubectl", "-n", h.cfg.operatorNamespace, "logs", "deploy/" + operatorReleaseName, "--tail=300"},
		{"kubectl", "-n", h.cfg.smokeNamespace, "get", "deploy,svc,pods,events", "-o", "wide"},
	}
	for _, command := range commands {
		h.logCommandOutput(command[0], command[1:]...)
	}
}

func (h *smokeHarness) logCommandOutput(name string, args ...string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.cfg.repoRoot
	output, err := cmd.CombinedOutput()
	label := name + " " + strings.Join(args, " ")
	if err != nil {
		h.t.Logf("diagnostic command failed: %s: %v\n%s", label, err, strings.TrimSpace(string(output)))
		return
	}
	if len(output) == 0 {
		h.t.Logf("diagnostic command output: %s: <empty>", label)
		return
	}
	h.t.Logf("diagnostic command output: %s\n%s", label, strings.TrimSpace(string(output)))
}

func mustListDNS(t *testing.T, ctx context.Context, cfClient cloudflare.Client, zoneID, hostname string) []cloudflare.DNSRecord {
	t.Helper()
	records, err := cfClient.DNSRecords().List(ctx, zoneID, hostname, "CNAME")
	if err != nil {
		t.Fatalf("list DNS records for %s: %v", hostname, err)
	}
	return records
}

func smokeHTTPClient() *http.Client {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 5 * time.Second}
			return dialer.DialContext(ctx, network, "1.1.1.1:53")
		},
	}
	dialer := &net.Dialer{
		Timeout:  10 * time.Second,
		Resolver: resolver,
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext:     dialer.DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("missing required environment variable: %s", name)
	}
	return value
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func isLocalChartRef(ref string) bool {
	return strings.HasPrefix(ref, ".") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "charts/")
}

func preserveImmutableServiceFields(obj, current client.Object) {
	service, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	currentService, ok := current.(*corev1.Service)
	if !ok {
		return
	}
	service.Spec.ClusterIP = currentService.Spec.ClusterIP
	service.Spec.ClusterIPs = currentService.Spec.ClusterIPs
	service.Spec.IPFamilies = currentService.Spec.IPFamilies
	service.Spec.IPFamilyPolicy = currentService.Spec.IPFamilyPolicy
	service.Spec.HealthCheckNodePort = currentService.Spec.HealthCheckNodePort
}

func maskSecret(value string) {
	if value != "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Printf("::add-mask::%s\n", value)
	}
}

func must(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func assertEqual[T comparable](t *testing.T, name string, want, got T) {
	t.Helper()
	if want != got {
		t.Fatalf("%s changed: want %v, got %v", name, want, got)
	}
}

func ptr[T any](value T) *T {
	return &value
}
