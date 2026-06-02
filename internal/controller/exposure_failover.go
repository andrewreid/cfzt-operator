package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/dr"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/ownership"
)

// annotationForcePromote, when set to "true" on a failover Exposure, makes a
// Standby attempt a CAS acquire regardless of lease expiry. The controller
// removes the annotation after a successful acquisition so it cannot persist
// in GitOps and re-trigger every reconcile.
const annotationForcePromote = "cfzt.reid.ee/force-promote"

// failoverAcquireJitter bounds the per-site promotion delay so two standbys
// racing to acquire an expired lease do not collide in the same instant
// (spec.md ## DR failover ### Split-brain bounding, ±5s).
const failoverAcquireJitter = 5 * time.Second

// errLeaseHeldByPeer signals that, on re-read inside the CAS loop, a live peer
// already owns the lease so this site lost the acquire race. It is not a CAS
// conflict — the controller stays Standby without error.
var errLeaseHeldByPeer = errors.New("failover lease held by another site")

// errLeaseForeign signals that the lease record at the computed name is not
// owned by this failover group (foreign comment or unparseable payload). The
// controller refuses to clobber it and surfaces LeaseConflict.
var errLeaseForeign = errors.New("failover lease record is foreign")

// exposureOwner returns the ownership identity to stamp on the shared
// Cloudflare resources for an Exposure. Failover Exposures use the
// cross-cluster group ID (D26) so either site's writes pass the mutation
// guard; non-failover Exposures use the per-CR UID.
func exposureOwner(exposure *cfztv1alpha1.CloudflareExposure) ownership.Owner {
	if exposure.Spec.Failover != nil {
		return ownership.FromFailoverGroup(exposure.Spec.Failover.Group)
	}
	return ownership.From(exposure.UID)
}

// isPrimary reports whether this site currently holds the failover lease for
// the Exposure per its last-persisted role.
func isPrimary(exposure *cfztv1alpha1.CloudflareExposure) bool {
	return exposure.Status.Failover.Role == string(dr.RolePrimary)
}

func (r *CloudflareExposureReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *CloudflareExposureReconciler) rng() *rand.Rand {
	// MaxConcurrentReconciles=1 per controller (D19) so a single per-reconciler
	// source is race-free. Lazily seeded; tests may inject a deterministic one.
	if r.Rand == nil {
		r.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return r.Rand
}

func leaseSecondsOf(exposure *cfztv1alpha1.CloudflareExposure) int32 {
	ls := exposure.Spec.Failover.LeaseSeconds
	if ls <= 0 {
		ls = 60
	}
	return ls
}

// reconcileFailoverRole runs the D26 lease arbitration at the top of the
// Exposure reconcile, before any shared Access/DNS write. It returns
// proceed=true only when this site is Primary and the caller should continue
// to the shared writes; in every other case it has already persisted
// status.failover and returns done=true with the controller result to use.
func (r *CloudflareExposureReconciler) reconcileFailoverRole(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client) (proceed bool, requeue time.Duration, result ctrl.Result, done bool, err error) {
	now := r.now()
	group := exposure.Spec.Failover.Group
	leaseSeconds := leaseSecondsOf(exposure)
	half := time.Duration(leaseSeconds) * time.Second / 2
	prevRole := dr.Role(exposure.Status.Failover.Role)
	if prevRole == "" {
		prevRole = dr.RoleUnknown
	}

	// Failover requires managed DNS: with dns.manage=false there is no lease
	// substrate. Surface the config error, write no lease.
	if !tunnel.Spec.Dns.Manage {
		fstatus := r.baseFailoverStatus(exposure, prevRole, "", nil, "")
		statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonFailoverRequiresManagedDNS,
			"spec.failover requires the referenced CloudflareTunnel to set dns.manage: true")
		if statusErr != nil {
			return false, 0, ctrl.Result{}, true, statusErr
		}
		return false, 0, ctrl.Result{RequeueAfter: 30 * time.Second}, true, nil
	}

	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("resolve zone for failover lease: %v", err))
	}
	leaseName := naming.FailoverLeaseTXTName(group, zone.Name)

	observed, _, readErr := r.readLease(ctx, cfClient, zone.ID, leaseName, exposure)
	if readErr != nil && !errors.Is(readErr, errLeaseForeign) {
		return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("read failover lease: %v", readErr))
	}
	if errors.Is(readErr, errLeaseForeign) {
		fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, "", nil, "")
		r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventLeaseConflict, "Failover lease record %s is not owned by this group", leaseName)
		statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonLeaseConflict, "failover lease record is owned by a foreign resource")
		if statusErr != nil {
			return false, 0, ctrl.Result{}, true, statusErr
		}
		return false, 0, ctrl.Result{RequeueAfter: 30 * time.Second}, true, nil
	}

	force := exposure.Annotations[annotationForcePromote] == "true"

	decision := dr.Decide(dr.Inputs{
		Now:           now,
		SiteID:        r.SiteID,
		PreviousRole:  prevRole,
		Observed:      observed,
		LeaseSeconds:  leaseSeconds,
		AcquireJitter: failoverAcquireJitter,
		ForcePromote:  force,
		Rand:          r.rng(),
	})

	var observedTunnel string
	if observed != nil {
		observedTunnel = observed.Tunnel
	}

	switch decision.Action {
	case dr.ActionWait:
		fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), observedTunnel)
		r.recordRole(exposure, dr.RoleStandby)
		statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, true, ReasonStandby, fmt.Sprintf("standby; lease held by %s", decision.LeaseOwner))
		return false, 0, requeueResult(decision.Requeue), true, statusErr

	case dr.ActionSplitBrain:
		fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), observedTunnel)
		r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventSplitBrainDetected, "Split brain: lease for %s now held by %s, demoting", exposure.Spec.Hostname, decision.LeaseOwner)
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDemotedToStandby, "Demoted to Standby for %s", exposure.Spec.Hostname)
		r.recordRole(exposure, dr.RoleStandby)
		statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, true, ReasonStandby, fmt.Sprintf("demoted to standby; lease held by %s", decision.LeaseOwner))
		return false, 0, requeueResult(decision.Requeue), true, statusErr

	case dr.ActionAcquire:
		lease, claimed, claimErr := r.claimLease(ctx, cfClient, zone, leaseName, exposure, tunnel, force, now)
		if claimErr != nil {
			return r.handleClaimError(ctx, exposure, claimErr, prevRole, decision)
		}
		if !claimed {
			fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), observedTunnel)
			r.recordRole(exposure, dr.RoleStandby)
			statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, true, ReasonStandby, fmt.Sprintf("standby; lease held by %s", decision.LeaseOwner))
			return false, 0, requeueResult(decision.Requeue), true, statusErr
		}
		// Acquired the lease — this site is now Primary.
		if force {
			if err := r.clearForcePromote(ctx, exposure); err != nil {
				return false, 0, ctrl.Result{}, true, err
			}
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventForcePromoted, "Force-promoted to Primary for %s", exposure.Spec.Hostname)
		}
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventLeaseAcquired, "Acquired failover lease for %s", exposure.Spec.Hostname)
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventPromotedToPrimary, "Promoted to Primary for %s", exposure.Spec.Hostname)
		failoverPromotionTotal.WithLabelValues(exposure.Namespace, exposure.Name, group).Inc()
		r.recordRole(exposure, dr.RolePrimary)
		fstatus := r.primaryFailoverStatus(exposure, lease)
		if statusErr := r.setFailoverStatus(ctx, exposure, fstatus); statusErr != nil {
			return false, 0, ctrl.Result{}, true, statusErr
		}
		return true, half, ctrl.Result{}, false, nil

	case dr.ActionRenew:
		lease, claimed, claimErr := r.claimLease(ctx, cfClient, zone, leaseName, exposure, tunnel, false, now)
		if claimErr != nil || !claimed {
			// Lost the lease we believed we held — demote without writes.
			if claimErr != nil && !errors.Is(claimErr, errLeaseHeldByPeer) && !errors.Is(claimErr, dr.ErrLeaseConflictExhausted) && !errors.Is(claimErr, errLeaseForeign) {
				return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("renew failover lease: %v", claimErr))
			}
			fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), observedTunnel)
			r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventLeaseLost, "Lost failover lease for %s", exposure.Spec.Hostname)
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDemotedToStandby, "Demoted to Standby for %s", exposure.Spec.Hostname)
			r.recordRole(exposure, dr.RoleStandby)
			statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, true, ReasonStandby, "demoted to standby; lost lease")
			return false, 0, requeueResult(half), true, statusErr
		}
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventLeaseRenewed, "Renewed failover lease for %s", exposure.Spec.Hostname)
		failoverLeaseRenewTotal.WithLabelValues(exposure.Namespace, exposure.Name, group).Inc()
		r.recordRole(exposure, dr.RolePrimary)
		fstatus := r.primaryFailoverStatus(exposure, lease)
		if statusErr := r.setFailoverStatus(ctx, exposure, fstatus); statusErr != nil {
			return false, 0, ctrl.Result{}, true, statusErr
		}
		return true, half, ctrl.Result{}, false, nil
	}

	return false, 0, ctrl.Result{}, true, fmt.Errorf("unhandled failover action %d", decision.Action)
}

// handleClaimError maps a claimLease failure to the right controller outcome:
// a lost race stays Standby, an exhausted/foreign conflict surfaces
// LeaseConflict, and anything else returns a backoff-eligible error.
func (r *CloudflareExposureReconciler) handleClaimError(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, claimErr error, prevRole dr.Role, decision dr.Decision) (bool, time.Duration, ctrl.Result, bool, error) {
	group := exposure.Spec.Failover.Group
	if errors.Is(claimErr, errLeaseHeldByPeer) {
		fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), "")
		r.recordRole(exposure, dr.RoleStandby)
		statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, true, ReasonStandby, fmt.Sprintf("standby; lease held by %s", decision.LeaseOwner))
		return false, 0, requeueResult(decision.Requeue), true, statusErr
	}
	if errors.Is(claimErr, dr.ErrLeaseConflictExhausted) || errors.Is(claimErr, errLeaseForeign) {
		fstatus := r.baseFailoverStatus(exposure, prevRole, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), "")
		r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventLeaseConflict, "Failover lease contention for %s", exposure.Spec.Hostname)
		statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonLeaseConflict, "could not acquire failover lease after retries")
		if statusErr != nil {
			return false, 0, ctrl.Result{}, true, statusErr
		}
		return false, 0, ctrl.Result{RequeueAfter: 30 * time.Second}, true, nil
	}
	_ = group
	return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("acquire failover lease: %v", claimErr))
}

// claimLease attempts to take or renew the failover lease through the
// CAS-capable DNS writer, re-reading the record on every attempt so a stale
// record_id never clobbers a peer's acquire. force bypasses the peer-liveness
// check (emergency promotion). Returns claimed=false only via the
// errLeaseHeldByPeer sentinel.
func (r *CloudflareExposureReconciler) claimLease(ctx context.Context, cfClient cloudflare.Client, zone *cloudflare.Zone, leaseName string, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, force bool, now time.Time) (dr.Lease, bool, error) {
	leaseSeconds := leaseSecondsOf(exposure)
	owner := exposureOwner(exposure)
	desired := dr.Lease{
		Version: dr.LeaseSchemaVersion,
		Site:    r.SiteID,
		Tunnel:  tunnel.Status.TunnelId,
		Renewed: now,
		Expires: now.Add(time.Duration(leaseSeconds) * time.Second),
	}
	input := cloudflare.DNSRecordInput{
		ZoneID:  zone.ID,
		Name:    leaseName,
		Type:    "TXT",
		Content: desired.Serialize(),
		Proxied: false,
		Comment: owner.Comment(),
	}
	claimed := false
	op := func() error {
		records, err := cfClient.DNSRecords().List(ctx, zone.ID, leaseName, "TXT")
		if err != nil {
			return err
		}
		var current *cloudflare.DNSRecord
		for i := range records {
			if records[i].Name == leaseName {
				current = &records[i]
				break
			}
		}
		if current == nil {
			if _, err := cfClient.DNSRecords().CreateCAS(ctx, input); err != nil {
				return err
			}
			claimed = true
			return nil
		}
		if !owner.MatchesComment(current.Comment) {
			return errLeaseForeign
		}
		existing, perr := dr.ParseLease(current.Content)
		if perr != nil {
			return errLeaseForeign
		}
		mine := existing.Site == r.SiteID
		if !force && !mine && !existing.Expired(now) {
			return errLeaseHeldByPeer
		}
		if _, err := cfClient.DNSRecords().UpdateCAS(ctx, current.ID, input); err != nil {
			return err
		}
		claimed = true
		return nil
	}
	err := dr.CASRetry(ctx, dr.DefaultRetryConfig(), r.rng(), func(e error) bool { return errors.Is(e, cloudflare.ErrDNSCASConflict) }, op)
	if err != nil {
		return dr.Lease{}, claimed, err
	}
	return desired, claimed, nil
}

// readLease returns the parsed lease at leaseName, or nil when absent. A
// present-but-foreign or unparseable record returns errLeaseForeign so the
// caller refuses to clobber it.
func (r *CloudflareExposureReconciler) readLease(ctx context.Context, cfClient cloudflare.Client, zoneID, leaseName string, exposure *cfztv1alpha1.CloudflareExposure) (*dr.Lease, string, error) {
	records, err := cfClient.DNSRecords().List(ctx, zoneID, leaseName, "TXT")
	if err != nil {
		return nil, "", err
	}
	owner := exposureOwner(exposure)
	for i := range records {
		if records[i].Name != leaseName {
			continue
		}
		if !owner.MatchesComment(records[i].Comment) {
			return nil, records[i].ID, errLeaseForeign
		}
		lease, perr := dr.ParseLease(records[i].Content)
		if perr != nil {
			return nil, records[i].ID, errLeaseForeign
		}
		return &lease, records[i].ID, nil
	}
	return nil, "", nil
}

// deleteOwnedLeaseIfPresent removes the failover lease TXT record when this
// site currently owns it (lease site == this --site-id). A Standby never
// deletes a peer's lease.
func (r *CloudflareExposureReconciler) deleteOwnedLeaseIfPresent(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) error {
	if exposure.Spec.Failover == nil {
		return nil
	}
	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		if errors.Is(err, cloudflare.ErrNotFound) {
			return nil
		}
		return err
	}
	leaseName := naming.FailoverLeaseTXTName(exposure.Spec.Failover.Group, zone.Name)
	lease, recordID, err := r.readLease(ctx, cfClient, zone.ID, leaseName, exposure)
	if err != nil {
		if errors.Is(err, errLeaseForeign) {
			return nil
		}
		return err
	}
	if lease == nil || lease.Site != r.SiteID {
		return nil
	}
	if err := cfClient.DNSRecords().Delete(ctx, zone.ID, recordID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
		return err
	}
	return nil
}

func (r *CloudflareExposureReconciler) clearForcePromote(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) error {
	latest := &cfztv1alpha1.CloudflareExposure{}
	key := types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	if latest.Annotations == nil {
		return nil
	}
	if _, ok := latest.Annotations[annotationForcePromote]; !ok {
		return nil
	}
	delete(latest.Annotations, annotationForcePromote)
	if err := r.Update(ctx, latest); err != nil {
		return err
	}
	delete(exposure.Annotations, annotationForcePromote)
	return nil
}

// baseFailoverStatus builds a Standby/Unknown-style failover status snapshot.
func (r *CloudflareExposureReconciler) baseFailoverStatus(exposure *cfztv1alpha1.CloudflareExposure, role dr.Role, leaseOwner string, expiresAt *metav1.Time, observedTunnel string) cfztv1alpha1.ExposureFailoverStatus {
	prev := exposure.Status.Failover
	fstatus := cfztv1alpha1.ExposureFailoverStatus{
		Role:                    string(role),
		SiteID:                  r.SiteID,
		LeaseOwner:              leaseOwner,
		LeaseExpiresAt:          expiresAt,
		ObservedPrimaryTunnelID: observedTunnel,
		LastRoleTransitionAt:    prev.LastRoleTransitionAt,
	}
	if prev.Role != string(role) {
		now := metav1.NewTime(r.now())
		fstatus.LastRoleTransitionAt = &now
	}
	return fstatus
}

// primaryFailoverStatus builds the Primary status snapshot after a successful
// acquire or renew.
func (r *CloudflareExposureReconciler) primaryFailoverStatus(exposure *cfztv1alpha1.CloudflareExposure, lease dr.Lease) cfztv1alpha1.ExposureFailoverStatus {
	expires := metav1.NewTime(lease.Expires)
	renewed := metav1.NewTime(lease.Renewed)
	fstatus := r.baseFailoverStatus(exposure, dr.RolePrimary, r.SiteID, &expires, lease.Tunnel)
	fstatus.LeaseRenewedAt = &renewed
	return fstatus
}

// recordRole publishes the current role to the failover role gauge.
func (r *CloudflareExposureReconciler) recordRole(exposure *cfztv1alpha1.CloudflareExposure, role dr.Role) {
	failoverRoleGauge.WithLabelValues(exposure.Namespace, exposure.Name, exposure.Spec.Failover.Group).Set(failoverRoleValue(string(role)))
}

func (r *CloudflareExposureReconciler) setFailoverStatus(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, fstatus cfztv1alpha1.ExposureFailoverStatus) error {
	latest := &cfztv1alpha1.CloudflareExposure{}
	key := types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	latest.Status.Failover = fstatus
	exposure.Status.Failover = fstatus
	return r.Status().Update(ctx, latest)
}

func (r *CloudflareExposureReconciler) setFailoverStatusAndReady(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, fstatus cfztv1alpha1.ExposureFailoverStatus, ready bool, reason, message string) error {
	latest := &cfztv1alpha1.CloudflareExposure{}
	key := types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	exposure.Status.Failover = fstatus
	return r.setReady(ctx, latest, &latest.Status.Conditions, latest.Generation, ready, reason, message, func() {
		latest.Status.Failover = fstatus
	})
}

func leaseExpiryPtr(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}

func requeueResult(d time.Duration) ctrl.Result {
	if d <= 0 {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: d}
}
