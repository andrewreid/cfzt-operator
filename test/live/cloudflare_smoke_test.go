//go:build live

package live

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

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
