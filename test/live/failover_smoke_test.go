//go:build live

package live

import (
	"context"
	"testing"
	"time"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/dr"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/ownership"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	failoverExposure         = "failover-smoke"
	failoverLeaseSeconds     = 600
	failoverRoleTimeout      = 3 * time.Minute
	peerSiteID               = "cfzt-smoke-peer"
	failoverCleanupTimeout   = 4 * time.Minute
	failoverCleanupWaitLimit = 2 * time.Minute
)

// TestFailoverLifecycle exercises the D26 failover lease lifecycle against a
// real Cloudflare account through one operator process and a single test
// hostname. A single kind cluster hosts exactly one operator, so the peer
// site is simulated by writing the lease TXT record directly via the
// Cloudflare API — the operator under test sees a real cross-site lease and
// reacts. This validates the CF-side lease + DNS lifecycle (one Primary,
// returning-primary self-demote, auto-promote on peer expiry, clean
// teardown); packet routing is out of scope per the slice plan.
func TestFailoverLifecycle(t *testing.T) {
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
	t.Cleanup(h.cleanupFailover)

	h.installOperator()
	h.deployEcho()

	t.Log("creating tunnel")
	h.createTunnel()
	tunnel := h.waitTunnelReady(resourceReadyTimeout)
	h.waitDaemonSetReady("cloudflared-"+h.cfg.tunnelName, daemonSetReadyTimeout)
	tunnelID := tunnel.Status.TunnelId

	t.Log("creating failover exposure; operator should acquire the lease as Primary")
	h.createFailoverExposure()
	h.waitExposureFailoverRole(string(dr.RolePrimary), failoverRoleTimeout)

	// The operator owns the lease: TXT names this site + this tunnel.
	lease, record := h.readFailoverLease()
	if record == nil {
		t.Fatalf("expected failover lease record %s to exist", h.failoverLeaseName())
	}
	assertEqual(t, "lease site", h.cfg.siteID, lease.Site)
	assertEqual(t, "lease tunnel", tunnelID, lease.Tunnel)
	// Public CNAME points at this site's tunnel.
	cnames := mustListDNS(t, ctx, h.cf, h.cfg.zoneID, h.cfg.failoverHostname)
	if len(cnames) != 1 {
		t.Fatalf("expected one failover CNAME, got %d", len(cnames))
	}
	assertEqual(t, "failover CNAME content", tunnelID+".cfargotunnel.com", cnames[0].Content)

	t.Log("simulating a peer site taking the lease; operator should self-demote to Standby")
	h.writePeerLease(peerSiteID, "peer-tunnel-id", h.now().Add(failoverLeaseSeconds*time.Second))
	h.updateFailoverExposureNoop()
	h.waitExposureFailoverRole(string(dr.RoleStandby), failoverRoleTimeout)
	// Returning-primary self-demote performed no write: lease still owned by peer.
	lease, _ = h.readFailoverLease()
	assertEqual(t, "lease site after demote", peerSiteID, lease.Site)

	t.Log("expiring the peer lease; operator should auto-promote back to Primary")
	h.writePeerLease(peerSiteID, "peer-tunnel-id", h.now().Add(-1*time.Minute))
	h.updateFailoverExposureNoop()
	h.waitExposureFailoverRole(string(dr.RolePrimary), failoverRoleTimeout)
	lease, _ = h.readFailoverLease()
	assertEqual(t, "lease site after auto-promote", h.cfg.siteID, lease.Site)
	assertEqual(t, "lease tunnel after auto-promote", tunnelID, lease.Tunnel)

	t.Log("live Cloudflare failover smoke passed")
}

func (h *smokeHarness) now() time.Time { return time.Now().UTC() }

func (h *smokeHarness) failoverLeaseName() string {
	return naming.FailoverLeaseTXTName(h.cfg.failoverGroup, h.cfg.testZone)
}

func (h *smokeHarness) failoverExposureObject() *cfztv1alpha1.CloudflareExposure {
	exposure := h.exposureObject(failoverExposure, h.cfg.failoverHostname, false)
	exposure.Spec.Failover = &cfztv1alpha1.FailoverSpec{
		Group:        h.cfg.failoverGroup,
		LeaseSeconds: failoverLeaseSeconds,
	}
	return exposure
}

func (h *smokeHarness) createFailoverExposure() {
	exposure := h.failoverExposureObject()
	if err := h.k8s.Create(h.ctx, exposure); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create failover CloudflareExposure: %v", err)
	}
}

func (h *smokeHarness) updateFailoverExposureNoop() {
	var exposure cfztv1alpha1.CloudflareExposure
	h.get(types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: failoverExposure}, &exposure)
	exposure.Spec = h.failoverExposureObject().Spec
	must(h.t, h.k8s.Update(h.ctx, &exposure), "update failover CloudflareExposure")
}

func (h *smokeHarness) waitExposureFailoverRole(role string, timeout time.Duration) {
	var exposure cfztv1alpha1.CloudflareExposure
	h.waitFor("failover exposure role "+role, timeout, func() (bool, error) {
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: failoverExposure}, &exposure); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return exposure.Status.Failover.Role == role, nil
	})
}

func (h *smokeHarness) readFailoverLease() (dr.Lease, *cloudflare.DNSRecord) {
	return h.readFailoverLeaseWithContext(h.ctx)
}

func (h *smokeHarness) readFailoverLeaseWithContext(ctx context.Context) (dr.Lease, *cloudflare.DNSRecord) {
	records, err := h.cf.DNSRecords().List(ctx, h.cfg.zoneID, h.failoverLeaseName(), "TXT")
	if err != nil {
		h.t.Fatalf("list failover lease TXT: %v", err)
	}
	if len(records) == 0 {
		return dr.Lease{}, nil
	}
	lease, perr := dr.ParseLease(records[0].Content)
	if perr != nil {
		h.t.Fatalf("parse failover lease %q: %v", records[0].Content, perr)
	}
	record := records[0]
	return lease, &record
}

// writePeerLease overwrites (or creates) the lease TXT record as if a peer
// site holds it. It stamps the same failover-group ownership comment the
// operator expects so the operator treats it as a legitimate group lease, not
// a foreign record.
func (h *smokeHarness) writePeerLease(site, tunnelID string, expires time.Time) {
	lease := dr.Lease{
		Version: dr.LeaseSchemaVersion,
		Site:    site,
		Tunnel:  tunnelID,
		Renewed: expires.Add(-failoverLeaseSeconds * time.Second),
		Expires: expires,
	}
	input := cloudflare.DNSRecordInput{
		ZoneID:  h.cfg.zoneID,
		Name:    h.failoverLeaseName(),
		Type:    "TXT",
		Content: lease.Serialize(),
		Comment: ownership.FromFailoverGroup(h.cfg.failoverGroup).Comment(),
	}
	_, record := h.readFailoverLease()
	if record == nil {
		if _, err := h.cf.DNSRecords().Create(h.ctx, input); err != nil {
			h.t.Fatalf("create peer lease: %v", err)
		}
		return
	}
	if _, err := h.cf.DNSRecords().Update(h.ctx, record.ID, input); err != nil {
		h.t.Fatalf("update peer lease: %v", err)
	}
}

func (h *smokeHarness) cleanupFailover() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), failoverCleanupTimeout)
	defer cancel()

	h.t.Log("cleanup: deleting failover CloudflareExposure")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareExposure{ObjectMeta: metav1.ObjectMeta{Name: failoverExposure, Namespace: h.cfg.smokeNamespace}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareExposure{}, types.NamespacedName{Name: failoverExposure, Namespace: h.cfg.smokeNamespace}, failoverCleanupWaitLimit)

	h.t.Log("cleanup: deleting CloudflareTunnel")
	h.deleteObject(cleanupCtx, &cfztv1alpha1.CloudflareTunnel{ObjectMeta: metav1.ObjectMeta{Name: h.cfg.tunnelName}})
	h.waitObjectAbsent(cleanupCtx, &cfztv1alpha1.CloudflareTunnel{}, types.NamespacedName{Name: h.cfg.tunnelName}, failoverCleanupWaitLimit)

	// The operator removes the lease TXT + CNAME when it owned them; if a
	// simulated peer lease is the last writer, remove it directly.
	if _, record := h.readFailoverLeaseWithContext(cleanupCtx); record != nil {
		if err := h.cf.DNSRecords().Delete(cleanupCtx, h.cfg.zoneID, record.ID); err != nil {
			h.t.Errorf("cleanup: delete failover lease TXT: %v", err)
		}
	}
	h.waitDNSAbsent(cleanupCtx, "failover CNAME", h.cfg.failoverHostname, failoverCleanupWaitLimit)
	h.deleteTunnelsByNamePrefix(cleanupCtx, h.cfg.tunnelName+"-cfzt-")
	h.waitTunnelAbsent(cleanupCtx, failoverCleanupWaitLimit)
}
