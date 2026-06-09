//go:build live

package live

import (
	"context"
	"errors"
	"testing"
	"time"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/dr"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/ownership"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
// returning-primary self-demote, auto-promote on peer expiry, then under
// promotionPolicy=Manual a refusal to auto-promote on an expired peer lease
// — Ready=False/Reason=AwaitingPromotion with the peer lease left untouched —
// and a force-promote token driving promotion, then clean teardown); packet
// routing is out of scope per the slice plan.
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
	tunnelCNAME := tunnelID + ".cfargotunnel.com"

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
	h.waitDNSCNAME(ctx, "failover CNAME", h.cfg.failoverHostname, tunnelCNAME, failoverRoleTimeout)

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
	h.waitDNSCNAME(ctx, "failover CNAME after auto-promote", h.cfg.failoverHostname, tunnelCNAME, failoverRoleTimeout)

	t.Log("switching to promotionPolicy=Manual; operator holds the lease so stays Primary")
	h.setFailoverPromotionPolicy(cfztv1alpha1.PromotionManual)
	h.waitExposureFailoverRole(string(dr.RolePrimary), failoverRoleTimeout)

	t.Log("peer takes the lease; returning-primary self-demote is policy-independent")
	h.writePeerLease(peerSiteID, "peer-tunnel-id", h.now().Add(failoverLeaseSeconds*time.Second))
	h.updateFailoverExposureNoop()
	h.waitExposureFailoverRole(string(dr.RoleStandby), failoverRoleTimeout)

	t.Log("expiring the peer lease; Manual policy must NOT auto-promote (AwaitingPromotion)")
	h.writePeerLease(peerSiteID, "peer-tunnel-id", h.now().Add(-1*time.Minute))
	h.updateFailoverExposureNoop()
	awaiting := h.waitExposureFailoverReason(reasonAwaitingPromotion, failoverRoleTimeout)
	ready := metav1.ConditionTrue
	if c := meta.FindStatusCondition(awaiting.Status.Conditions, conditionReady); c != nil {
		ready = c.Status
	}
	assertEqual(t, "Ready=False while awaiting manual promotion (hostname down)", string(metav1.ConditionFalse), string(ready))
	// Operator left the expired peer lease alone — it did not steal it.
	lease, _ = h.readFailoverLease()
	assertEqual(t, "lease site while awaiting promotion", peerSiteID, lease.Site)

	t.Log("force-promote token; operator must promote to Primary under Manual policy")
	h.setForcePromoteToken("cfzt-smoke-force-1")
	h.waitExposureFailoverRole(string(dr.RolePrimary), failoverRoleTimeout)
	lease, _ = h.readFailoverLease()
	assertEqual(t, "lease site after force-promote", h.cfg.siteID, lease.Site)
	assertEqual(t, "lease tunnel after force-promote", tunnelID, lease.Tunnel)
	h.waitDNSCNAME(ctx, "failover CNAME after force-promote", h.cfg.failoverHostname, tunnelCNAME, failoverRoleTimeout)

	t.Log("live Cloudflare failover smoke passed")
}

func (h *smokeHarness) now() time.Time { return time.Now().UTC() }

func (h *smokeHarness) failoverLeaseName() string {
	return naming.FailoverLeaseTXTName(h.cfg.failoverGroup, h.cfg.testZone)
}

func (h *smokeHarness) failoverExposureObject() *cfztv1alpha1.CloudflareExposure {
	exposure := h.exposureObject(failoverExposure, h.cfg.failoverHostname, false)
	exposure.Spec.Failover = &cfztv1alpha1.FailoverSpec{
		Group:           h.cfg.failoverGroup,
		LeaseSeconds:    failoverLeaseSeconds,
		PromotionPolicy: h.failoverPolicy,
	}
	return exposure
}

// setFailoverPromotionPolicy records the desired promotionPolicy and reapplies
// the spec so the operator observes it. The harness field keeps it sticky
// across later updateFailoverExposureNoop calls.
func (h *smokeHarness) setFailoverPromotionPolicy(policy cfztv1alpha1.PromotionPolicy) {
	h.failoverPolicy = policy
	h.updateFailoverExposureNoop()
}

// setForcePromoteToken stamps the emergency force-promote annotation. Metadata
// is untouched by the noop spec updates, so the token persists until changed.
// Wrapped in RetryOnConflict because the controller is concurrently writing
// status/failover, so a plain read-modify-Update races (object-has-been-modified).
func (h *smokeHarness) setForcePromoteToken(token string) {
	key := types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: failoverExposure}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var exposure cfztv1alpha1.CloudflareExposure
		if err := h.k8s.Get(h.ctx, key, &exposure); err != nil {
			return err
		}
		if exposure.Annotations == nil {
			exposure.Annotations = map[string]string{}
		}
		exposure.Annotations[annotationForcePromote] = token
		return h.k8s.Update(h.ctx, &exposure)
	})
	must(h.t, err, "set force-promote annotation")
}

// waitExposureFailoverReason blocks until the failover Exposure's Ready
// condition carries the given reason (with role Standby), returning the latest
// observed Exposure. Used to assert a Manual-policy Standby parked on an
// expired/absent lease (AwaitingPromotion) rather than racing a non-transition.
func (h *smokeHarness) waitExposureFailoverReason(reason string, timeout time.Duration) cfztv1alpha1.CloudflareExposure {
	var exposure cfztv1alpha1.CloudflareExposure
	h.waitFor("failover exposure reason "+reason, timeout, func() (bool, error) {
		if err := h.k8s.Get(h.ctx, types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: failoverExposure}, &exposure); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		ready := meta.FindStatusCondition(exposure.Status.Conditions, conditionReady)
		if ready == nil {
			return false, nil
		}
		return exposure.Status.Failover.Role == string(dr.RoleStandby) && ready.Reason == reason, nil
	})
	return exposure
}

func (h *smokeHarness) createFailoverExposure() {
	exposure := h.failoverExposureObject()
	if err := h.k8s.Create(h.ctx, exposure); err != nil && !apierrors.IsAlreadyExists(err) {
		h.t.Fatalf("create failover CloudflareExposure: %v", err)
	}
}

func (h *smokeHarness) updateFailoverExposureNoop() {
	key := types.NamespacedName{Namespace: h.cfg.smokeNamespace, Name: failoverExposure}
	// RetryOnConflict: the controller writes status/failover concurrently, so a
	// plain read-modify-Update races (object-has-been-modified) — re-Get and
	// retry on conflict so these reconcile nudges are reliable.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var exposure cfztv1alpha1.CloudflareExposure
		if err := h.k8s.Get(h.ctx, key, &exposure); err != nil {
			return err
		}
		exposure.Spec = h.failoverExposureObject().Spec
		return h.k8s.Update(h.ctx, &exposure)
	})
	must(h.t, err, "update failover CloudflareExposure")
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
	// The Exposure finalizer only deletes the shared CNAME when THIS site holds
	// the live lease. If the run failed during the deliberately peer-owned
	// window, the finalizer correctly left the CNAME alone — so remove any
	// remaining CNAME at the per-run failover hostname directly (it is unique to
	// this run, so anything there is ours), otherwise shared DNS leaks.
	if records, err := h.cf.DNSRecords().List(cleanupCtx, h.cfg.zoneID, h.cfg.failoverHostname, "CNAME"); err != nil {
		h.t.Errorf("cleanup: list failover CNAME: %v", err)
	} else {
		for _, record := range records {
			if err := h.cf.DNSRecords().Delete(cleanupCtx, h.cfg.zoneID, record.ID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
				h.t.Errorf("cleanup: delete failover CNAME %s: %v", record.ID, err)
			}
		}
	}
	h.waitDNSAbsent(cleanupCtx, "failover CNAME", h.cfg.failoverHostname, failoverCleanupWaitLimit)
	h.deleteTunnelsByNamePrefix(cleanupCtx, h.cfg.tunnelName+"-cfzt-")
	h.waitTunnelAbsent(cleanupCtx, failoverCleanupWaitLimit)
}
