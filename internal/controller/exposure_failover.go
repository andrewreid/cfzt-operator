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

// annotationForcePromote carries a caller-chosen one-shot token (timestamp,
// nonce, UUID — any non-empty value). A Standby attempts an acquire regardless
// of expiry only when the token differs from status.failover.lastForcePromoteToken.
// The controller records the honored token in status and never mutates the
// annotation, so a GitOps re-apply of the same token does not replay (D26).
const annotationForcePromote = "cfzt.reid.ee/force-promote"

// defaultSiteID is the chart's single-site upgrade default (values.site.id).
// A failover Exposure on a process still running this identity is refused:
// two clusters on the default would share one lease identity and each see the
// other's lease as self-owned (review feedback). Keep in sync with the chart.
const defaultSiteID = "cfzt-default-site"

// failoverAcquireJitter bounds the per-site promotion delay so two standbys
// racing to acquire an expired lease do not collide in the same instant
// (spec.md ## DR failover ### Split-brain bounding, ±5s).
const failoverAcquireJitter = 5 * time.Second

// failoverResolveRequeue is the short requeue after the controller deletes
// duplicate lease records, so the next reconcile acts on the converged set.
const failoverResolveRequeue = 2 * time.Second

// errLeaseForeign signals that a record at the lease name is not owned by this
// failover group (foreign comment) or its group-owned payload does not parse.
// The controller refuses to clobber it and fails closed (LeaseConflict).
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

// reconcileFailoverRole runs the D26 best-effort lease arbitration at the top
// of the Exposure reconcile, before any shared Access/DNS write. It returns
// proceed=true only when this site holds the lease (Primary) and the caller
// should continue to the shared writes; in every other case it has already
// persisted status.failover and returns done=true with the controller result.
func (r *CloudflareExposureReconciler) reconcileFailoverRole(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client) (proceed bool, requeue time.Duration, result ctrl.Result, done bool, err error) {
	now := r.now()
	group := exposure.Spec.Failover.Group
	leaseSeconds := leaseSecondsOf(exposure)
	half := time.Duration(leaseSeconds) * time.Second / 2
	prevRole := dr.Role(exposure.Status.Failover.Role)
	if prevRole == "" {
		prevRole = dr.RoleUnknown
	}

	// --site-id must be distinct per cluster. The chart default would make two
	// clusters share one lease identity (each sees the other's lease as
	// self-owned). Refuse failover until a real site ID is set.
	if r.SiteID == defaultSiteID {
		fstatus := r.baseFailoverStatus(exposure, prevRole, "", nil, "")
		return false, 0, ctrl.Result{RequeueAfter: 30 * time.Second}, true,
			r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonFailoverRequiresDistinctSiteID,
				"spec.failover requires a distinct --site-id; the chart-default site.id is not safe for failover")
	}

	// Failover requires managed DNS: with dns.manage=false there is no lease
	// substrate. Surface the config error, write no lease.
	if !tunnel.Spec.Dns.Manage {
		fstatus := r.baseFailoverStatus(exposure, prevRole, "", nil, "")
		return false, 0, ctrl.Result{RequeueAfter: 30 * time.Second}, true,
			r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonFailoverRequiresManagedDNS,
				"spec.failover requires the referenced CloudflareTunnel to set dns.manage: true")
	}

	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("resolve zone for failover lease: %v", err))
	}
	leaseName := naming.FailoverLeaseTXTName(group, zone.Name)

	records, foreign, err := r.listGroupLeases(ctx, cfClient, zone.ID, leaseName, exposure)
	if err != nil {
		return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("read failover lease: %v", err))
	}
	if foreign {
		return r.failClosed(ctx, exposure, leaseName)
	}
	if len(records) > 1 {
		return r.resolveDuplicateLeases(ctx, cfClient, zone, exposure, records, now)
	}

	var observed *dr.Lease
	var currentID *string
	if len(records) == 1 {
		observed = &records[0].Lease
		id := records[0].ID
		currentID = &id
	}

	forceToken := exposure.Annotations[annotationForcePromote]
	force := forceToken != "" && forceToken != exposure.Status.Failover.LastForcePromoteToken

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
		return r.standby(ctx, exposure, decision, observedTunnel, false)

	case dr.ActionSplitBrain:
		r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventSplitBrainDetected, "Split brain: lease for %s now held by %s, demoting", exposure.Spec.Hostname, decision.LeaseOwner)
		return r.standby(ctx, exposure, decision, observedTunnel, true)

	case dr.ActionAcquire, dr.ActionRenew:
		return r.writeAndVerifyLease(ctx, cfClient, zone, leaseName, exposure, tunnel, currentID, now, half, force, forceToken)
	}

	return false, 0, ctrl.Result{}, true, fmt.Errorf("unhandled failover action %d", decision.Action)
}

// writeAndVerifyLease performs the (non-atomic) acquire/renew write, then
// re-reads the lease set to verify the outcome. Because the write is not
// conditional, a peer may have written too; the read-back catches duplicates
// (deterministic resolution) and a lost race (demote), bounding the dual-writer
// window to about one reconcile.
func (r *CloudflareExposureReconciler) writeAndVerifyLease(ctx context.Context, cfClient cloudflare.Client, zone *cloudflare.Zone, leaseName string, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, currentID *string, now time.Time, half time.Duration, force bool, forceToken string) (bool, time.Duration, ctrl.Result, bool, error) {
	if err := r.writeLease(ctx, cfClient, zone, leaseName, exposure, tunnel, currentID, now); err != nil {
		return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("write failover lease: %v", err))
	}

	records, foreign, err := r.listGroupLeases(ctx, cfClient, zone.ID, leaseName, exposure)
	if err != nil {
		return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("verify failover lease: %v", err))
	}
	if foreign {
		return r.failClosed(ctx, exposure, leaseName)
	}
	if len(records) > 1 {
		// A peer wrote concurrently; resolve deterministically and converge.
		return r.resolveDuplicateLeases(ctx, cfClient, zone, exposure, records, now)
	}
	if len(records) != 1 || records[0].Lease.Site != r.SiteID {
		// We lost the race: someone else's record survived. Demote.
		r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventLeaseLost, "Lost failover lease for %s", exposure.Spec.Hostname)
		demoteDecision := dr.Decision{LeaseOwner: leaseOwnerOf(records), Requeue: half}
		return r.standby(ctx, exposure, demoteDecision, leaseTunnelOf(records), true)
	}

	// We hold the lease — Primary.
	lease := records[0].Lease
	wasPrimary := dr.Role(exposure.Status.Failover.Role) == dr.RolePrimary
	if force {
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventForcePromoted, "Force-promoted to Primary for %s", exposure.Spec.Hostname)
	}
	if !wasPrimary {
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventLeaseAcquired, "Acquired failover lease for %s", exposure.Spec.Hostname)
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventPromotedToPrimary, "Promoted to Primary for %s", exposure.Spec.Hostname)
		failoverPromotionTotal.WithLabelValues(exposure.Namespace, exposure.Name, exposure.Spec.Failover.Group, r.SiteID).Inc()
	} else {
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventLeaseRenewed, "Renewed failover lease for %s", exposure.Spec.Hostname)
		failoverLeaseRenewTotal.WithLabelValues(exposure.Namespace, exposure.Name, exposure.Spec.Failover.Group, r.SiteID).Inc()
	}
	r.recordRole(exposure, dr.RolePrimary)
	fstatus := r.primaryFailoverStatus(exposure, lease)
	if force {
		fstatus.LastForcePromoteToken = forceToken
	}
	if statusErr := r.setFailoverStatus(ctx, exposure, fstatus); statusErr != nil {
		return false, 0, ctrl.Result{}, true, statusErr
	}
	return true, half, ctrl.Result{}, false, nil
}

// standby persists a Standby status snapshot and returns done. demote=true
// emits the DemotedToStandby event (used by split-brain and lost-race paths).
func (r *CloudflareExposureReconciler) standby(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, decision dr.Decision, observedTunnel string, demote bool) (bool, time.Duration, ctrl.Result, bool, error) {
	if demote {
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDemotedToStandby, "Demoted to Standby for %s", exposure.Spec.Hostname)
	}
	fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, decision.LeaseOwner, leaseExpiryPtr(decision.LeaseExpiresAt), observedTunnel)
	r.recordRole(exposure, dr.RoleStandby)
	msg := fmt.Sprintf("standby; lease held by %s", decision.LeaseOwner)
	if decision.LeaseOwner == "" {
		msg = "standby"
	}
	statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, true, ReasonStandby, msg)
	return false, 0, requeueResult(decision.Requeue), true, statusErr
}

// failClosed handles an ambiguous lease (foreign comment or unparseable
// payload at the lease name): no shared write, Ready=False, requeue.
func (r *CloudflareExposureReconciler) failClosed(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, leaseName string) (bool, time.Duration, ctrl.Result, bool, error) {
	r.recordRole(exposure, dr.RoleStandby)
	r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventLeaseConflict, "Failover lease record %s is foreign or unparseable", leaseName)
	fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, "", nil, "")
	return false, 0, ctrl.Result{RequeueAfter: 30 * time.Second}, true,
		r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonLeaseConflict, "failover lease record is foreign or unparseable")
}

// resolveDuplicateLeases deterministically converges a >1 lease set: the
// winner deletes the others, a non-winner deletes only its own duplicates.
// Both sites compute the same winner, so the set converges without
// coordination. Surfaces LeaseConflict and requeues to act on the result.
func (r *CloudflareExposureReconciler) resolveDuplicateLeases(ctx context.Context, cfClient cloudflare.Client, zone *cloudflare.Zone, exposure *cfztv1alpha1.CloudflareExposure, records []dr.LeaseRecord, now time.Time) (bool, time.Duration, ctrl.Result, bool, error) {
	res := dr.Resolve(records, now, r.SiteID)
	r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventLeaseConflict, "Resolving %d duplicate failover leases for %s (winner %s)", len(records), exposure.Spec.Hostname, res.WinnerSite)
	for _, id := range res.DeleteIDs {
		if err := cfClient.DNSRecords().Delete(ctx, zone.ID, id); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return false, 0, ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, exposure.Status.Cloudflare, fmt.Sprintf("delete duplicate lease %s: %v", id, err))
		}
	}
	r.recordRole(exposure, dr.RoleStandby)
	fstatus := r.baseFailoverStatus(exposure, dr.RoleStandby, res.WinnerSite, nil, "")
	if statusErr := r.setFailoverStatusAndReady(ctx, exposure, fstatus, false, ReasonLeaseConflict, "resolving duplicate failover leases"); statusErr != nil {
		return false, 0, ctrl.Result{}, true, statusErr
	}
	return false, 0, ctrl.Result{RequeueAfter: failoverResolveRequeue}, true, nil
}

// writeLease writes the desired lease payload: Create when no record exists,
// else Update the observed record by ID. Neither is conditional — convergence
// comes from the caller's read-back + duplicate resolution.
func (r *CloudflareExposureReconciler) writeLease(ctx context.Context, cfClient cloudflare.Client, zone *cloudflare.Zone, leaseName string, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, currentID *string, now time.Time) error {
	leaseSeconds := leaseSecondsOf(exposure)
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
		Comment: exposureOwner(exposure).Comment(),
	}
	if currentID == nil {
		_, err := cfClient.DNSRecords().Create(ctx, input)
		return err
	}
	_, err := cfClient.DNSRecords().Update(ctx, *currentID, input)
	return err
}

// listGroupLeases returns the parsed group-owned lease records at leaseName.
// foreign is true if any record at the lease name is not group-owned or its
// group-owned payload does not parse — the caller fails closed in that case.
func (r *CloudflareExposureReconciler) listGroupLeases(ctx context.Context, cfClient cloudflare.Client, zoneID, leaseName string, exposure *cfztv1alpha1.CloudflareExposure) ([]dr.LeaseRecord, bool, error) {
	records, err := cfClient.DNSRecords().List(ctx, zoneID, leaseName, "TXT")
	if err != nil {
		return nil, false, err
	}
	owner := exposureOwner(exposure)
	var out []dr.LeaseRecord
	for i := range records {
		if records[i].Name != leaseName {
			continue
		}
		if !owner.MatchesComment(records[i].Comment) {
			return nil, true, nil
		}
		lease, perr := dr.ParseLease(records[i].Content)
		if perr != nil {
			return nil, true, nil
		}
		out = append(out, dr.LeaseRecord{ID: records[i].ID, Lease: lease})
	}
	return out, false, nil
}

func leaseOwnerOf(records []dr.LeaseRecord) string {
	if len(records) == 1 {
		return records[0].Lease.Site
	}
	return ""
}

func leaseTunnelOf(records []dr.LeaseRecord) string {
	if len(records) == 1 {
		return records[0].Lease.Tunnel
	}
	return ""
}

// readLease returns the single parsed group-owned lease at leaseName, or nil
// when absent. A foreign or unparseable record returns errLeaseForeign. Used
// by the deletion path; the role gate uses listGroupLeases.
func (r *CloudflareExposureReconciler) readLease(ctx context.Context, cfClient cloudflare.Client, zoneID, leaseName string, exposure *cfztv1alpha1.CloudflareExposure) (*dr.Lease, string, error) {
	records, foreign, err := r.listGroupLeases(ctx, cfClient, zoneID, leaseName, exposure)
	if err != nil {
		return nil, "", err
	}
	if foreign {
		return nil, "", errLeaseForeign
	}
	if len(records) == 0 {
		return nil, "", nil
	}
	return &records[0].Lease, records[0].ID, nil
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

// baseFailoverStatus builds a Standby/Unknown-style failover status snapshot.
// It preserves the persisted lastForcePromoteToken so the replay guard holds
// across role transitions.
func (r *CloudflareExposureReconciler) baseFailoverStatus(exposure *cfztv1alpha1.CloudflareExposure, role dr.Role, leaseOwner string, expiresAt *metav1.Time, observedTunnel string) cfztv1alpha1.ExposureFailoverStatus {
	prev := exposure.Status.Failover
	fstatus := cfztv1alpha1.ExposureFailoverStatus{
		Role:                    string(role),
		SiteID:                  r.SiteID,
		LeaseOwner:              leaseOwner,
		LeaseExpiresAt:          expiresAt,
		ObservedPrimaryTunnelID: observedTunnel,
		LastRoleTransitionAt:    prev.LastRoleTransitionAt,
		LastForcePromoteToken:   prev.LastForcePromoteToken,
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

// recordRole publishes the current role to the failover role gauge, labelled
// with this site's identity so central scraping can attribute the reading.
func (r *CloudflareExposureReconciler) recordRole(exposure *cfztv1alpha1.CloudflareExposure, role dr.Role) {
	failoverRoleGauge.WithLabelValues(exposure.Namespace, exposure.Name, exposure.Spec.Failover.Group, r.SiteID).Set(failoverRoleValue(string(role)))
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
