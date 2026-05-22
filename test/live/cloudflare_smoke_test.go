//go:build live

package live

import (
	"context"
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
	conditionReady         = "Ready"
	reasonHostnameConflict = "HostnameConflict"
	reasonForeignResource  = "ForeignResource"
	reasonForeignRoute     = "ForeignRoute"

	operatorReleaseName = "cfzt-operator"
	credentialsSecret   = "cloudflare-credentials"
	echoName            = "smoke-echo"
	publicExposure      = "public-smoke"
	accessExposure      = "access-smoke"
	conflictExposure    = "conflict-smoke"
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
}

func TestCloudflarePreflight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg := loadSmokeConfig(t)
	cfClient := newCloudflareClient(t, cfg)
	zoneID := resolveZoneID(t, ctx, cfClient, cfg)

	mustListDNS(t, ctx, cfClient, zoneID, "__cfzt-smoke-preflight-"+cfg.runSuffix+"."+cfg.testZone)
	if _, err := cfClient.AccessPolicies().List(ctx); err != nil {
		t.Fatalf("list Cloudflare Access policies: %v", err)
	}
	preflightPolicy, err := cfClient.AccessPolicies().Create(ctx, cloudflare.AccessPolicyInput{
		Name:            "__cfzt-smoke-preflight-policy-" + cfg.runSuffix,
		Decision:        "allow",
		Include:         []cloudflare.AccessRule{{EmailDomain: cfg.testZone}},
		SessionDuration: "24h",
	})
	if err != nil {
		t.Fatalf("create/delete Cloudflare Access policy preflight: create: %v", err)
	}
	if err := cfClient.AccessPolicies().Delete(ctx, preflightPolicy.ID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
		t.Fatalf("create/delete Cloudflare Access policy preflight: delete %s: %v", preflightPolicy.ID, err)
	}
	if _, err := cfClient.AccessApplications().List(ctx, "__cfzt-smoke-preflight-"+cfg.runSuffix+"."+cfg.testZone); err != nil {
		t.Fatalf("list Cloudflare Access applications: %v", err)
	}
	if _, err := cfClient.Tunnels().List(ctx, "__cfzt-smoke-preflight-"+cfg.runSuffix); err != nil {
		t.Fatalf("list Cloudflare tunnels: %v", err)
	}
	for _, network := range []string{cfg.tunnelRouteCIDR} {
		routes, err := cfClient.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{Network: network})
		if err != nil {
			t.Fatalf("list Cloudflare tunnel routes for %s: %v", network, err)
		}
		if len(routes) > 0 {
			t.Fatalf("Cloudflare tunnel route smoke CIDR %s already exists; set CF_SMOKE_ROUTE_CIDR / CF_SMOKE_ROUTE_CONFLICT_CIDR to unused CIDRs", network)
		}
	}
	routes, err := cfClient.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{Network: cfg.tunnelRouteConflictCIDR})
	if err != nil {
		t.Fatalf("list Cloudflare tunnel routes for %s: %v", cfg.tunnelRouteConflictCIDR, err)
	}
	if len(routes) > 0 {
		t.Logf("configured foreign-route conflict CIDR %s already exists; lifecycle will try fallback CIDRs", cfg.tunnelRouteConflictCIDR)
	}

	t.Log("Cloudflare smoke preflight passed")
}

func TestCloudflareLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cfg := loadSmokeConfig(t)
	h := &smokeHarness{
		t:   t,
		ctx: ctx,
		cfg: cfg,
		cf:  newCloudflareClient(t, cfg),
		k8s: newKubeClient(t),
	}
	h.cfg.zoneID = resolveZoneID(t, ctx, h.cf, cfg)
	t.Cleanup(h.cleanup)

	h.installOperator()
	h.deployEcho()

	t.Log("creating managed Access policy")
	h.createAccessPolicy()
	policy := h.waitAccessPolicyReady(7 * time.Minute)
	policyIDBefore := policy.Status.PolicyId
	policyHashBefore := policy.Status.ObservedRulesHash
	if !strings.HasPrefix(policyHashBefore, "sha256:") {
		t.Fatalf("unexpected Access policy rules hash %q", policyHashBefore)
	}
	h.assertOneAccessPolicy(policyIDBefore)

	t.Log("creating tunnel")
	h.createTunnel()
	tunnel := h.waitTunnelReady(7 * time.Minute)
	tunnelIDBefore := tunnel.Status.TunnelId
	if tunnel.Status.TokenSecretRef.Name != h.cfg.tunnelName+"-token" {
		t.Fatalf("unexpected token Secret %q", tunnel.Status.TokenSecretRef.Name)
	}
	h.waitDaemonSetReady("cloudflared-"+h.cfg.tunnelName, 3*time.Minute)

	t.Log("creating tunnel private route")
	h.createTunnelRoute()
	tunnelRoute := h.waitTunnelRouteReady(7 * time.Minute)
	tunnelRouteIDBefore := tunnelRoute.Status.RouteId
	h.assertOneTunnelRoute(tunnelRouteIDBefore, tunnelIDBefore, h.cfg.tunnelRouteCIDR)

	t.Log("creating public and Access exposures")
	h.createExposures()
	public := h.waitExposureReady(publicExposure, 7*time.Minute)
	access := h.waitExposureReady(accessExposure, 7*time.Minute)
	publicDNSBefore := public.Status.Cloudflare.DnsRecordId
	publicRouteBefore := public.Status.Cloudflare.PublicHostnameRouteHash
	accessDNSBefore := access.Status.Cloudflare.DnsRecordId
	accessAppBefore := access.Status.Cloudflare.AccessApplicationId
	accessRouteBefore := access.Status.Cloudflare.PublicHostnameRouteHash
	if publicRouteBefore == "" || accessRouteBefore == "" {
		t.Fatalf("expected non-empty route hashes, got public=%q access=%q", publicRouteBefore, accessRouteBefore)
	}

	h.waitPublicRoute()
	h.assertAccessChallenged()

	t.Log("checking idempotency after re-apply and operator restart")
	h.updateAccessPolicyNoop()
	h.updateTunnelNoop()
	h.updateTunnelRouteNoop()
	h.updateExposuresNoop()
	h.restartOperator()

	policy = h.waitAccessPolicyReady(4 * time.Minute)
	tunnel = h.waitTunnelReady(4 * time.Minute)
	tunnelRoute = h.waitTunnelRouteReady(4 * time.Minute)
	public = h.waitExposureReady(publicExposure, 4*time.Minute)
	access = h.waitExposureReady(accessExposure, 4*time.Minute)

	assertEqual(t, "Access policy ID", policyIDBefore, policy.Status.PolicyId)
	assertEqual(t, "Access policy rules hash", policyHashBefore, policy.Status.ObservedRulesHash)
	assertEqual(t, "tunnel ID", tunnelIDBefore, tunnel.Status.TunnelId)
	assertEqual(t, "tunnel route ID", tunnelRouteIDBefore, tunnelRoute.Status.RouteId)
	assertEqual(t, "public DNS record ID", publicDNSBefore, public.Status.Cloudflare.DnsRecordId)
	assertEqual(t, "access DNS record ID", accessDNSBefore, access.Status.Cloudflare.DnsRecordId)
	assertEqual(t, "Access application ID", accessAppBefore, access.Status.Cloudflare.AccessApplicationId)

	t.Log("checking foreign DNS conflict safety")
	record, err := h.cf.DNSRecords().Create(ctx, cloudflare.DNSRecordInput{
		ZoneID:  h.cfg.zoneID,
		Name:    h.cfg.conflictHostname,
		Type:    "CNAME",
		Content: "example.com",
		Proxied: false,
		Comment: "cfzt-live-smoke-foreign",
	})
	if err != nil {
		t.Fatalf("create foreign DNS record: %v", err)
	}
	h.foreignRecordID = record.ID
	h.createConflictExposure()
	reason := h.waitExposureConflictReason(conflictExposure, 4*time.Minute)
	t.Logf("conflict exposure reported %s", reason)
	foreignRecords := mustListDNS(t, ctx, h.cf, h.cfg.zoneID, h.cfg.conflictHostname)
	if len(foreignRecords) != 1 {
		t.Fatalf("expected one foreign DNS record, got %d", len(foreignRecords))
	}
	assertEqual(t, "foreign DNS content", "example.com", foreignRecords[0].Content)
	assertEqual(t, "foreign DNS comment", "cfzt-live-smoke-foreign", foreignRecords[0].Comment)

	t.Log("checking foreign tunnel route conflict safety")
	foreignRoute := h.createForeignTunnelRoute(tunnelIDBefore)
	h.createConflictTunnelRoute()
	routeReason := h.waitTunnelRouteForeignReason(4 * time.Minute)
	t.Logf("conflict tunnel route reported %s", routeReason)
	foreignRoutes := h.listTunnelRoutes(h.cfg.tunnelRouteConflictCIDR)
	if len(foreignRoutes) != 1 {
		t.Fatalf("expected one foreign tunnel route, got %d", len(foreignRoutes))
	}
	assertEqual(t, "foreign tunnel route ID", foreignRoute.ID, foreignRoutes[0].ID)
	assertEqual(t, "foreign tunnel route comment", "cfzt-live-smoke-foreign-route", foreignRoutes[0].Comment)

	t.Log("live Cloudflare smoke passed")
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
		"--wait",
		"--timeout", "3m"}
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
	if filepath.IsAbs(h.cfg.chartRef) {
		return h.cfg.chartRef, true
	}
	return filepath.Clean(filepath.Join(h.cfg.repoRoot, h.cfg.chartRef)), true
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
	h.waitDeploymentAvailable(h.cfg.smokeNamespace, echoName, 3*time.Minute)
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
	exposure := h.exposureObject(accessExposure, h.cfg.accessHostname, true)
	exposure.Spec.Access.PolicyRef.Name = h.cfg.accessPolicy
	return exposure
}

func (h *smokeHarness) createConflictExposure() {
	exposure := h.exposureObject(conflictExposure, h.cfg.conflictHostname, false)
	if err := h.k8s.Create(h.ctx, exposure); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create conflict CloudflareExposure: %v", err)
	}
}

func (h *smokeHarness) exposureObject(name, hostname string, accessEnabled bool) *cfztv1alpha1.CloudflareExposure {
	return &cfztv1alpha1.CloudflareExposure{
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
	h.waitFor("public hostname HTTP 200", 10*time.Minute, func() (bool, error) {
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

func (h *smokeHarness) assertAccessChallenged() {
	httpClient := smokeHTTPClient()
	h.t.Logf("checking unauthenticated Access response for https://%s/hostname", h.cfg.accessHostname)
	var lastStatus int
	h.waitFor("Access hostname challenge", 10*time.Minute, func() (bool, error) {
		req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "https://"+h.cfg.accessHostname+"/hostname", nil)
		if err != nil {
			return false, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		lastStatus = resp.StatusCode
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusUnauthorized, http.StatusForbidden:
			return true, nil
		case http.StatusOK:
			return false, fmt.Errorf("Access hostname returned HTTP 200; policy may be bypassing unauthenticated users")
		default:
			return false, nil
		}
	})
	h.t.Logf("Access hostname returned expected unauthenticated status %d", lastStatus)
}

func (h *smokeHarness) assertOneAccessPolicy(policyID string) {
	policies, err := h.cf.AccessPolicies().List(h.ctx)
	if err != nil {
		h.t.Fatalf("list Access policies: %v", err)
	}
	var matches []cloudflare.AccessPolicy
	for _, policy := range policies {
		if policy.Name == h.cfg.accessPolicy {
			matches = append(matches, policy)
		}
	}
	if len(matches) != 1 {
		h.t.Fatalf("expected exactly one managed Access policy named %s, got %d", h.cfg.accessPolicy, len(matches))
	}
	assertEqual(h.t, "managed Access policy ID", policyID, matches[0].ID)
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
	h.waitDeploymentAvailable(h.cfg.operatorNamespace, operatorReleaseName, 3*time.Minute)
}

func (h *smokeHarness) cleanup() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	h.t.Log("cleanup: deleting CloudflareExposure resources")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: publicExposure, Namespace: h.cfg.smokeNamespace}})
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: accessExposure, Namespace: h.cfg.smokeNamespace}})
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: conflictExposure, Namespace: h.cfg.smokeNamespace}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: publicExposure, Namespace: h.cfg.smokeNamespace}, 5*time.Minute)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: accessExposure, Namespace: h.cfg.smokeNamespace}, 5*time.Minute)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: conflictExposure, Namespace: h.cfg.smokeNamespace}, 5*time.Minute)

	h.t.Log("cleanup: deleting CloudflareTunnelRoute resources")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelRoute}})
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelRouteConflict}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{}, types.NamespacedName{Name: h.cfg.tunnelRoute}, 5*time.Minute)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnelRoute{}, types.NamespacedName{Name: h.cfg.tunnelRouteConflict}, 5*time.Minute)

	if h.foreignRouteID != "" {
		h.t.Log("cleanup: deleting foreign conflict tunnel route")
		if err := h.cf.TunnelRoutes().Delete(cleanupCtx, h.foreignRouteID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			h.t.Errorf("delete foreign tunnel route: %v", err)
		}
		h.waitTunnelRouteAbsent(cleanupCtx, "foreign tunnel route", h.cfg.tunnelRouteConflictCIDR, 3*time.Minute)
	}

	h.t.Log("cleanup: deleting CloudflareAccessPolicy")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.accessPolicy}})
	h.deleteAccessPoliciesByName(cleanupCtx, h.cfg.accessPolicy)
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareAccessPolicy{}, types.NamespacedName{Name: h.cfg.accessPolicy}, 5*time.Minute)

	h.t.Log("cleanup: deleting CloudflareTunnel")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnel{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelName}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnel{}, types.NamespacedName{Name: h.cfg.tunnelName}, 5*time.Minute)

	if h.foreignRecordID != "" {
		h.t.Log("cleanup: deleting foreign conflict DNS record")
		if err := h.cf.DNSRecords().Delete(cleanupCtx, h.cfg.zoneID, h.foreignRecordID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			h.t.Errorf("delete foreign DNS record: %v", err)
		}
	}
	h.waitDNSAbsent(cleanupCtx, "public DNS record", h.cfg.publicHostname, 3*time.Minute)
	h.waitDNSAbsent(cleanupCtx, "access DNS record", h.cfg.accessHostname, 3*time.Minute)
	h.waitTunnelRouteAbsent(cleanupCtx, "tunnel route", h.cfg.tunnelRouteCIDR, 3*time.Minute)
	h.waitAccessApplicationsAbsent(cleanupCtx, 3*time.Minute)
	h.waitAccessPolicyAbsent(cleanupCtx, 3*time.Minute)
	h.waitTunnelAbsent(cleanupCtx, 3*time.Minute)
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
			if policy.Name == h.cfg.accessPolicy {
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

func (h *smokeHarness) waitTunnelAbsent(ctx context.Context, timeout time.Duration) {
	h.waitForContext(ctx, "Cloudflare tunnel absent", timeout, func() (bool, error) {
		tunnels, err := h.cf.Tunnels().List(ctx, h.cfg.tunnelName)
		if err != nil {
			return false, err
		}
		return len(tunnels) == 0, nil
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
