package dr

import (
	"math/rand"
	"time"
)

// Role mirrors the spec.md ## DR failover role state machine
// (Unknown -> Standby -> Primary -> Standby). The string values are
// persisted to CloudflareExposure.status.failover.role and surfaced via
// the kubectl printcolumn; do not rename without a migration story.
type Role string

const (
	RoleUnknown Role = "Unknown"
	RoleStandby Role = "Standby"
	RolePrimary Role = "Primary"
)

// Action is the work the role gate has decided the controller should
// perform during this reconcile pass. Every reconcile resolves to exactly
// one Action; the controller dispatches it through the CAS-capable DNS
// writer and updates status accordingly.
type Action int

const (
	// ActionWait means the lease is held by a live peer. This site stays
	// Standby and reads no further; the controller does not touch the
	// shared Access app or public CNAME. Decision.Requeue is the wait
	// until the observed expiry plus per-site jitter.
	ActionWait Action = iota

	// ActionAcquire attempts a CreateCAS. Used when no lease exists, when
	// the observed lease has expired, or when the user has set the
	// force-promote annotation. Success transitions the controller to
	// Primary; failure (CAS conflict) keeps it Standby.
	ActionAcquire

	// ActionRenew updates the existing lease record this site already
	// owns. Used by Primary on every reconcile inside the renewal
	// window. Success keeps the previous role Primary and pushes
	// LeaseExpiresAt forward by LeaseSeconds.
	ActionRenew

	// ActionSplitBrain fires when this site believes it is Primary but
	// the live lease names a different owner. The controller emits
	// SplitBrainDetected, demotes to Standby, and performs no Cloudflare
	// writes for the shared hostname this pass. The peer that legitimately
	// won the lease keeps writing.
	ActionSplitBrain
)

// Inputs is the snapshot from which Decide derives its verdict. It is
// deliberately a plain struct (no controller-runtime types, no Kubernetes
// objects) so Decide stays pure and trivially testable.
type Inputs struct {
	// Now is the wall-clock instant at reconcile entry. Injected so tests
	// drive the state machine across virtual time.
	Now time.Time
	// SiteID is this operator process's --site-id (D26 process invariant).
	SiteID string
	// PreviousRole is the role most recently persisted to status by this
	// site; RoleUnknown on the first observation.
	PreviousRole Role
	// Observed is the parsed live lease, or nil if the lease TXT record
	// does not currently exist.
	Observed *Lease
	// LeaseSeconds mirrors CloudflareExposure.spec.failover.leaseSeconds
	// and drives the Primary renewal cadence + Standby wait window.
	LeaseSeconds int32
	// AcquireJitter is the symmetric spread applied to the Standby wait
	// before considering an expired lease promotable. ±5s by default per
	// spec.md ## DR failover ### Split-brain bounding.
	AcquireJitter time.Duration
	// ForcePromote signals the cfzt.reid.ee/force-promote=true annotation
	// is set. Overrides expiry so an operator can promote during an
	// outage they have already confirmed.
	ForcePromote bool
	// Rand is the per-process rand source used for jitter. Required when
	// AcquireJitter > 0; nil is a programmer error in that mode.
	Rand *rand.Rand
}

// Decision is the role gate's verdict for one reconcile pass.
type Decision struct {
	// NextRole is the role to persist in status after the controller has
	// completed the dispatched Action. ActionAcquire reports NextRole as
	// Standby because the transition to Primary only happens on
	// successful acquire; the controller writes RolePrimary itself after
	// the CAS succeeds.
	NextRole Role
	// Action is the single piece of work the controller performs this
	// pass.
	Action Action
	// LeaseOwner is the site-id currently holding the lease, or empty
	// when the lease does not exist.
	LeaseOwner string
	// LeaseExpiresAt mirrors Observed.Expires when present; zero otherwise.
	LeaseExpiresAt time.Time
	// Requeue is the duration the controller should wait before the
	// next reconcile if no event fires sooner. Primary uses
	// leaseSeconds/2 so renewal lands before expiry; Standby waits for
	// observed expiry plus jitter so promotion does not undershoot.
	Requeue time.Duration
}

// Decide reads the snapshot and returns the next role + action for this
// reconcile pass. Pure function — no IO, no global state. The controller
// dispatches the returned Action through the CAS-capable DNS writer and
// then writes the returned NextRole / LeaseOwner / LeaseExpiresAt into
// status.
func Decide(in Inputs) Decision {
	leaseTTL := time.Duration(in.LeaseSeconds) * time.Second
	if leaseTTL <= 0 {
		// Defensive: a zero leaseSeconds is rejected at CRD validation,
		// but Decide may be called in tests with a zero value. Default
		// to the spec.md baseline so the renewal cadence remains sane.
		leaseTTL = 60 * time.Second
	}
	half := max(leaseTTL/2, time.Second)

	// No lease observed: anyone can acquire. Stay Standby until the
	// caller's CreateCAS succeeds.
	if in.Observed == nil {
		return Decision{
			NextRole: RoleStandby,
			Action:   ActionAcquire,
			Requeue:  half,
		}
	}

	obs := in.Observed
	isMine := obs.Site == in.SiteID
	expired := obs.Expired(in.Now)

	// Force-promote overrides expiry, but only when a peer holds the
	// lease. ForcePromote against a self-owned lease is a no-op — fall
	// through to the regular renewal path.
	if in.ForcePromote && !isMine {
		return Decision{
			NextRole:       RoleStandby,
			Action:         ActionAcquire,
			LeaseOwner:     obs.Site,
			LeaseExpiresAt: obs.Expires,
			Requeue:        half,
		}
	}

	// I hold the lease.
	if isMine {
		return Decision{
			NextRole:       RolePrimary,
			Action:         ActionRenew,
			LeaseOwner:     in.SiteID,
			LeaseExpiresAt: obs.Expires,
			Requeue:        half,
		}
	}

	// A peer holds a live lease.
	if !expired {
		// I thought I was Primary; the lease says otherwise. Split brain
		// — demote silently this pass and let the legitimate owner keep
		// writing. The controller emits SplitBrainDetected on this Action.
		if in.PreviousRole == RolePrimary {
			return Decision{
				NextRole:       RoleStandby,
				Action:         ActionSplitBrain,
				LeaseOwner:     obs.Site,
				LeaseExpiresAt: obs.Expires,
				Requeue:        half,
			}
		}
		wait := obs.Expires.Sub(in.Now)
		if in.AcquireJitter > 0 && in.Rand != nil {
			wait += Jitter(in.AcquireJitter, in.Rand)
		}
		if wait < time.Second {
			wait = time.Second
		}
		return Decision{
			NextRole:       RoleStandby,
			Action:         ActionWait,
			LeaseOwner:     obs.Site,
			LeaseExpiresAt: obs.Expires,
			Requeue:        wait,
		}
	}

	// A peer's lease has expired; try to acquire it.
	return Decision{
		NextRole:       RoleStandby,
		Action:         ActionAcquire,
		LeaseOwner:     obs.Site,
		LeaseExpiresAt: obs.Expires,
		Requeue:        half,
	}
}
