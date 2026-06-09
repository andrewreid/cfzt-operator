package dr_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/dr"
)

const (
	siteSelf = "homelab-primary"
	sitePeer = "homelab-dr"
	tunnelID = "t-1"
)

func mkLease(site string, expires, renewed time.Time) *dr.Lease {
	return &dr.Lease{Version: 1, Site: site, Tunnel: tunnelID, Expires: expires, Renewed: renewed}
}

func TestFailoverLeaseAcquire(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	in := dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RoleUnknown,
		Observed:     nil,
		LeaseSeconds: 60,
	}
	d := dr.Decide(in)
	if d.Action != dr.ActionAcquire {
		t.Fatalf("Action = %v, want ActionAcquire (no lease present)", d.Action)
	}
	if d.NextRole != dr.RoleStandby {
		t.Fatalf("NextRole = %v, want RoleStandby until acquire succeeds", d.NextRole)
	}
	if d.Requeue != 30*time.Second {
		t.Fatalf("Requeue = %v, want 30s (leaseSeconds/2)", d.Requeue)
	}
}

func TestFailoverLeaseRenew(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// Renewed 40s ago with half=30s => renewal window is open => ActionRenew.
	lease := mkLease(siteSelf, now.Add(20*time.Second), now.Add(-40*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RolePrimary,
		Observed:     lease,
		LeaseSeconds: 60,
	})
	if d.Action != dr.ActionRenew {
		t.Fatalf("Action = %v, want ActionRenew (self-owned lease, renewal due)", d.Action)
	}
	if d.NextRole != dr.RolePrimary {
		t.Fatalf("NextRole = %v, want RolePrimary", d.NextRole)
	}
	if d.LeaseOwner != siteSelf {
		t.Fatalf("LeaseOwner = %q, want %q", d.LeaseOwner, siteSelf)
	}
	if d.Requeue != 30*time.Second {
		t.Fatalf("Requeue = %v, want 30s (leaseSeconds/2)", d.Requeue)
	}
}

func TestFailoverDecideHoldsPrimaryBeforeRenewWindow(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// Just-renewed self lease: renewed=now, half=30s => window opens at now+30s,
	// so Decide must hold (stay Primary) without rewriting the lease. This is
	// what stops a Primary from renewing on every reconcile.
	lease := mkLease(siteSelf, now.Add(60*time.Second), now)
	d := dr.Decide(dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RolePrimary,
		Observed:     lease,
		LeaseSeconds: 60,
	})
	if d.Action != dr.ActionHoldPrimary {
		t.Fatalf("Action = %v, want ActionHoldPrimary (renewal window not open)", d.Action)
	}
	if d.NextRole != dr.RolePrimary {
		t.Fatalf("NextRole = %v, want RolePrimary (hold keeps Primary)", d.NextRole)
	}
	if d.LeaseOwner != siteSelf {
		t.Fatalf("LeaseOwner = %q, want %q", d.LeaseOwner, siteSelf)
	}
	// Requeue ~ time until the renewal window opens (renewed+half - now = 30s).
	if d.Requeue != 30*time.Second {
		t.Fatalf("Requeue = %v, want 30s (until renewal due)", d.Requeue)
	}
}

func TestFailoverDecideWaitsForLivePeer(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	lease := mkLease(sitePeer, now.Add(40*time.Second), now.Add(-20*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:           now,
		SiteID:        siteSelf,
		PreviousRole:  dr.RoleStandby,
		Observed:      lease,
		LeaseSeconds:  60,
		AcquireJitter: 5 * time.Second,
		Rand:          rand.New(rand.NewSource(1)),
	})
	if d.Action != dr.ActionWait {
		t.Fatalf("Action = %v, want ActionWait (peer holds live lease)", d.Action)
	}
	if d.LeaseOwner != sitePeer {
		t.Fatalf("LeaseOwner = %q, want %q", d.LeaseOwner, sitePeer)
	}
	// Requeue ≈ 40s ± 5s jitter, with the 1s floor never tripping at this scale.
	if d.Requeue < 35*time.Second || d.Requeue > 45*time.Second {
		t.Fatalf("Requeue = %v, want ~40s ± 5s jitter", d.Requeue)
	}
}

func TestFailoverDecideAcquiresExpiredPeer(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	lease := mkLease(sitePeer, now.Add(-1*time.Second), now.Add(-61*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RoleStandby,
		Observed:     lease,
		LeaseSeconds: 60,
	})
	if d.Action != dr.ActionAcquire {
		t.Fatalf("Action = %v, want ActionAcquire (peer expired)", d.Action)
	}
	if d.NextRole != dr.RoleStandby {
		t.Fatalf("NextRole = %v, want RoleStandby until acquire succeeds", d.NextRole)
	}
}

func TestFailoverDecideManualWaitsOnExpiredPeer(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	lease := mkLease(sitePeer, now.Add(-1*time.Second), now.Add(-61*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:             now,
		SiteID:          siteSelf,
		PreviousRole:    dr.RoleStandby,
		Observed:        lease,
		LeaseSeconds:    60,
		ManualPromotion: true,
	})
	if d.Action != dr.ActionWait {
		t.Fatalf("Action = %v, want ActionWait (manual policy suppresses auto-acquire)", d.Action)
	}
	if d.NextRole != dr.RoleStandby {
		t.Fatalf("NextRole = %v, want RoleStandby", d.NextRole)
	}
	if !d.AwaitingPromotion {
		t.Fatalf("AwaitingPromotion = false, want true (expired peer, manual policy)")
	}
	if d.LeaseOwner != sitePeer {
		t.Fatalf("LeaseOwner = %q, want %q", d.LeaseOwner, sitePeer)
	}
}

func TestFailoverDecideManualWaitsOnAbsentLease(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := dr.Decide(dr.Inputs{
		Now:             now,
		SiteID:          siteSelf,
		PreviousRole:    dr.RoleUnknown,
		Observed:        nil,
		LeaseSeconds:    60,
		ManualPromotion: true,
	})
	if d.Action != dr.ActionWait {
		t.Fatalf("Action = %v, want ActionWait (manual policy does not auto-elect day-1 Primary)", d.Action)
	}
	if !d.AwaitingPromotion {
		t.Fatalf("AwaitingPromotion = false, want true (absent lease, manual policy)")
	}
}

func TestFailoverDecideManualForcePromoteAcquiresExpiredPeer(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	lease := mkLease(sitePeer, now.Add(-1*time.Second), now.Add(-61*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:             now,
		SiteID:          siteSelf,
		PreviousRole:    dr.RoleStandby,
		Observed:        lease,
		LeaseSeconds:    60,
		ManualPromotion: true,
		ForcePromote:    true,
	})
	if d.Action != dr.ActionAcquire {
		t.Fatalf("Action = %v, want ActionAcquire (force-promote overrides manual policy)", d.Action)
	}
	if d.AwaitingPromotion {
		t.Fatalf("AwaitingPromotion = true, want false (force-promote acquires, not waits)")
	}
}

func TestFailoverDecideManualForcePromoteAcquiresAbsentLease(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := dr.Decide(dr.Inputs{
		Now:             now,
		SiteID:          siteSelf,
		PreviousRole:    dr.RoleUnknown,
		Observed:        nil,
		LeaseSeconds:    60,
		ManualPromotion: true,
		ForcePromote:    true,
	})
	if d.Action != dr.ActionAcquire {
		t.Fatalf("Action = %v, want ActionAcquire (force-promote elects first Primary under manual policy)", d.Action)
	}
}

func TestFailoverDecideManualKeepsRenewingSelfHeldLease(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// Renewal window open (renewed 40s ago, half=30s): manual policy must still
	// renew a lease this site already holds — it only gates *acquiring*.
	lease := mkLease(siteSelf, now.Add(20*time.Second), now.Add(-40*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:             now,
		SiteID:          siteSelf,
		PreviousRole:    dr.RolePrimary,
		Observed:        lease,
		LeaseSeconds:    60,
		ManualPromotion: true,
	})
	if d.Action != dr.ActionRenew {
		t.Fatalf("Action = %v, want ActionRenew (manual policy still renews a self-held lease)", d.Action)
	}
}

func TestFailoverDecideForcePromoteOverridesLivePeer(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	lease := mkLease(sitePeer, now.Add(40*time.Second), now.Add(-20*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RoleStandby,
		Observed:     lease,
		LeaseSeconds: 60,
		ForcePromote: true,
	})
	if d.Action != dr.ActionAcquire {
		t.Fatalf("Action = %v, want ActionAcquire under ForcePromote", d.Action)
	}
}

func TestFailoverDecideForcePromoteNoopOnSelf(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// Renewal window open so the no-op falls through to ActionRenew (not a
	// fresh hold): force-promote against an own lease must not acquire/steal.
	lease := mkLease(siteSelf, now.Add(20*time.Second), now.Add(-40*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RolePrimary,
		Observed:     lease,
		LeaseSeconds: 60,
		ForcePromote: true,
	})
	if d.Action != dr.ActionRenew {
		t.Fatalf("Action = %v, want ActionRenew (force-promote against own lease is a no-op)", d.Action)
	}
}

func TestFailoverDecideSplitBrainDemote(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	lease := mkLease(sitePeer, now.Add(40*time.Second), now.Add(-20*time.Second))
	d := dr.Decide(dr.Inputs{
		Now:          now,
		SiteID:       siteSelf,
		PreviousRole: dr.RolePrimary,
		Observed:     lease,
		LeaseSeconds: 60,
	})
	if d.Action != dr.ActionSplitBrain {
		t.Fatalf("Action = %v, want ActionSplitBrain (was Primary, peer holds live lease)", d.Action)
	}
	if d.NextRole != dr.RoleStandby {
		t.Fatalf("NextRole = %v, want RoleStandby (must demote)", d.NextRole)
	}
}

func TestFailoverDecideRenewWindowClampedAtOneSecond(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	// leaseSeconds=0 falls back to the 60s default inside Decide.
	d := dr.Decide(dr.Inputs{Now: now, SiteID: siteSelf, LeaseSeconds: 0})
	if d.Requeue < time.Second {
		t.Fatalf("Requeue = %v, want ≥1s floor under degenerate input", d.Requeue)
	}
}
