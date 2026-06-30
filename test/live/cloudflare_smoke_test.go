//go:build live

package live

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

const (
	lifecycleTimeout      = 10 * time.Minute
	resourceReadyTimeout  = 2 * time.Minute
	restartReadyTimeout   = 90 * time.Second
	conflictReadyTimeout  = 90 * time.Second
	tunnelHashTimeout     = 90 * time.Second
	daemonSetReadyTimeout = 2 * time.Minute
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
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
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
	policy := h.waitAccessPolicyReady(resourceReadyTimeout)
	policyIDBefore := policy.Status.PolicyId
	policyHashBefore := policy.Status.ObservedRulesHash
	if !strings.HasPrefix(policyHashBefore, "sha256:") {
		t.Fatalf("unexpected Access policy rules hash %q", policyHashBefore)
	}
	h.assertOneAccessPolicy(policyIDBefore)

	t.Log("creating tunnel")
	h.createTunnel()
	tunnel := h.waitTunnelReady(resourceReadyTimeout)
	tunnelIDBefore := tunnel.Status.TunnelId
	h.assertTunnelName(tunnel)
	if tunnel.Status.TokenSecretRef.Name != h.cfg.tunnelName+"-token" {
		t.Fatalf("unexpected token Secret %q", tunnel.Status.TokenSecretRef.Name)
	}
	h.waitDaemonSetReady("cloudflared-"+h.cfg.tunnelName, daemonSetReadyTimeout)

	t.Log("creating tunnel private route")
	h.createTunnelRoute()
	tunnelRoute := h.waitTunnelRouteReady(resourceReadyTimeout)
	tunnelRouteIDBefore := tunnelRoute.Status.RouteId
	h.assertOneTunnelRoute(tunnelRouteIDBefore, tunnelIDBefore, h.cfg.tunnelRouteCIDR)

	t.Log("creating public and Access exposures")
	h.createExposures()
	public := h.waitExposureReady(publicExposure, resourceReadyTimeout)
	access := h.waitExposureReady(accessExposure, resourceReadyTimeout)
	publicDNSBefore := public.Status.Cloudflare.DnsRecordId
	publicRouteBefore := public.Status.Cloudflare.PublicHostnameRouteHash
	accessDNSBefore := access.Status.Cloudflare.DnsRecordId
	accessRootBefore := h.accessApplicationStatus(access.Status.Cloudflare.AccessApplications, "root")
	accessAdminBefore := h.accessApplicationStatus(access.Status.Cloudflare.AccessApplications, "admin")
	accessRouteBefore := access.Status.Cloudflare.PublicHostnameRouteHash
	if publicRouteBefore == "" || accessRouteBefore == "" {
		t.Fatalf("expected non-empty route hashes, got public=%q access=%q", publicRouteBefore, accessRouteBefore)
	}

	tunnel = h.waitTunnelReady(tunnelHashTimeout)
	// Slice 6 adjustment: subtask 4 made the tunnel config hash the live signal
	// that the real tunnel config write happened once and can be skipped later.
	ingressHashBefore := tunnel.Status.IngressDocHash
	if ingressHashBefore == "" {
		t.Fatalf("expected non-empty tunnel ingress doc hash after exposures are ready")
	}
	h.waitPublicRoute()
	// Slice 6 adjustment: subtask 7 split write PolicyUUID from read
	// PolicyUUIDs, so live smoke reads the CF app back through the slice shape.
	h.assertAccessApplications(access.Status.Cloudflare.AccessApplications, policyIDBefore)
	h.assertAccessChallenged("/hostname")
	h.assertAccessChallenged("/admin/hostname")

	t.Log("checking idempotency after re-apply and operator restart")
	h.updateAccessPolicyNoop()
	h.updateTunnelNoop()
	h.updateTunnelRouteNoop()
	h.updateExposuresNoop()
	h.restartOperator()

	policy = h.waitAccessPolicyReady(restartReadyTimeout)
	tunnel = h.waitTunnelReady(restartReadyTimeout)
	tunnelRoute = h.waitTunnelRouteReady(restartReadyTimeout)
	public = h.waitExposureReady(publicExposure, restartReadyTimeout)
	access = h.waitExposureReady(accessExposure, restartReadyTimeout)

	assertEqual(t, "Access policy ID", policyIDBefore, policy.Status.PolicyId)
	assertEqual(t, "Access policy rules hash", policyHashBefore, policy.Status.ObservedRulesHash)
	assertEqual(t, "tunnel ID", tunnelIDBefore, tunnel.Status.TunnelId)
	assertEqual(t, "tunnel ingress doc hash", ingressHashBefore, tunnel.Status.IngressDocHash)
	assertEqual(t, "tunnel route ID", tunnelRouteIDBefore, tunnelRoute.Status.RouteId)
	assertEqual(t, "public DNS record ID", publicDNSBefore, public.Status.Cloudflare.DnsRecordId)
	assertEqual(t, "access DNS record ID", accessDNSBefore, access.Status.Cloudflare.DnsRecordId)
	assertEqual(t, "root Access application ID", accessRootBefore.AppID, h.accessApplicationStatus(access.Status.Cloudflare.AccessApplications, "root").AppID)
	assertEqual(t, "admin Access application ID", accessAdminBefore.AppID, h.accessApplicationStatus(access.Status.Cloudflare.AccessApplications, "admin").AppID)
	h.assertAccessApplications(access.Status.Cloudflare.AccessApplications, policyIDBefore)

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
	reason := h.waitExposureConflictReason(conflictExposure, conflictReadyTimeout)
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
	routeReason := h.waitTunnelRouteForeignReason(conflictReadyTimeout)
	t.Logf("conflict tunnel route reported %s", routeReason)
	foreignRoutes := h.listTunnelRoutes(h.cfg.tunnelRouteConflictCIDR)
	if len(foreignRoutes) != 1 {
		t.Fatalf("expected one foreign tunnel route, got %d", len(foreignRoutes))
	}
	assertEqual(t, "foreign tunnel route ID", foreignRoute.ID, foreignRoutes[0].ID)
	assertEqual(t, "foreign tunnel route comment", "cfzt-live-smoke-foreign-route", foreignRoutes[0].Comment)

	t.Log("checking wildcard exposures (issue #13)")
	h.createWildcardExposures()

	// (a) DNS-only wildcard: proxied wildcard CNAME + tunnel ingress rule.
	wildcardDNS := h.waitExposureReady(wildcardDNSExposure, resourceReadyTimeout)
	if wildcardDNS.Status.Cloudflare.DnsRecordId == "" {
		t.Fatalf("wildcard DNS exposure missing DNS record ID")
	}
	if wildcardDNS.Status.Cloudflare.PublicHostnameRouteHash == "" {
		t.Fatalf("wildcard DNS exposure missing route hash")
	}
	wildcardRecord := h.waitDNSCNAME(ctx, "wildcard CNAME", h.cfg.wildcardDNSHostname, tunnelIDBefore+".cfargotunnel.com", resourceReadyTimeout)
	assertEqual(t, "wildcard CNAME name", h.cfg.wildcardDNSHostname, wildcardRecord.Name)
	if !wildcardRecord.Proxied {
		t.Fatalf("wildcard CNAME for %s is not proxied", h.cfg.wildcardDNSHostname)
	}
	// Cloudflare Universal SSL covers the apex and a SINGLE wildcard level
	// (*.zone) only, so a 2-label-deep concrete host (foo.wildcard-<run>.zone)
	// has no edge certificate and its HTTPS handshake is rejected at the edge
	// (TLS alert handshake failure). We therefore do NOT probe the concrete host
	// over HTTPS; instead we prove the wildcard is routable by asserting the
	// operator wrote a tunnel ingress rule for the wildcard hostname into the
	// CloudflareTunnel status (Status.Routes — the same source dumped in
	// diagnostics). Reaching a deep-wildcard host over HTTPS needs an edge cert
	// from Advanced Certificate Manager / Total TLS.
	h.waitTunnelIngressHostname(h.cfg.wildcardDNSHostname, tunnelHashTimeout)

	// (b) standalone wildcard self-hosted Access app (no overlapping concrete).
	// Prove the wildcard Access app exists and the proxied wildcard CNAME is
	// present. As in (a), the concrete host foo.access-wc-<run>.zone is two labels
	// deep and outside Universal SSL, so we do NOT issue the HTTPS Access
	// challenge probe against it — the edge has no cert and the TLS handshake
	// fails. Deep-wildcard edge certs require ACM / Total TLS.
	wildcardAccess := h.waitExposureReady(wildcardAccExposure, resourceReadyTimeout)
	h.assertAccessApplicationsFor(wildcardAccExposure, h.cfg.wildcardAccessHostname, wildcardAccess.Status.Cloudflare.AccessApplications, policyIDBefore)
	wildcardAccessRecord := h.waitDNSCNAME(ctx, "wildcard access CNAME", h.cfg.wildcardAccessHostname, tunnelIDBefore+".cfargotunnel.com", resourceReadyTimeout)
	assertEqual(t, "wildcard access CNAME name", h.cfg.wildcardAccessHostname, wildcardAccessRecord.Name)
	if !wildcardAccessRecord.Proxied {
		t.Fatalf("wildcard access CNAME for %s is not proxied", h.cfg.wildcardAccessHostname)
	}

	// Wildcard+concrete Access OVERLAP is env-gated: it asserts only the
	// operator's fail-closed HostnameConflict guard, not Cloudflare precedence.
	t.Run("WildcardAccessOverlap", func(t *testing.T) {
		if os.Getenv(envWildcardAccessOverlap) != "1" {
			t.Skipf("set %s=1 to exercise the wildcard+concrete Access overlap guard", envWildcardAccessOverlap)
		}
		h.createOverlapConcrete()
		h.waitExposureReady(overlapConcreteExp, resourceReadyTimeout)
		h.createOverlapWildcard()
		reason := h.waitExposureConflictReason(overlapWildcardExp, conflictReadyTimeout)
		t.Logf("overlap wildcard exposure reported %s", reason)
		if reason != reasonHostnameConflict {
			t.Fatalf("expected wildcard overlap to report %s, got %s", reasonHostnameConflict, reason)
		}
		// Fail-closed: the rejected wildcard must own no Cloudflare DNS record and
		// no Access applications for its hostname.
		h.waitDNSAbsent(ctx, "overlap wildcard DNS record", h.cfg.overlapWildcardHostname, conflictReadyTimeout)
		h.waitAccessApplicationsAbsentFor(ctx, h.cfg.overlapWildcardHostname, conflictReadyTimeout)
	})

	t.Log("live Cloudflare smoke passed")
}
