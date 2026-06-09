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
// one Action; the controller dispatches it through the best-effort DNS lease
// writer (write + read-back verify) and updates status accordingly.
type Action int

const (
	// ActionWait means the lease is held by a live peer. This site stays
	// Standby and reads no further; the controller does not touch the
	// shared Access app or public CNAME. Decision.Requeue is the wait
	// until the observed expiry plus per-site jitter.
	ActionWait Action = iota

	// ActionAcquire writes the lease (create if absent, else update the
	// observed record) then read-back verifies. Used when no lease exists,
	// when the observed lease has expired, or when the user has set the
	// force-promote token. A verified single self-owned record transitions
	// the controller to Primary; a lost race keeps it Standby.
	ActionAcquire

	// ActionRenew updates the existing lease record this site already
	// owns. Used by Primary once the renewal window has opened (now is at
	// or past renewed+leaseSeconds/2). Success keeps the previous role
	// Primary and pushes LeaseExpiresAt forward by LeaseSeconds.
	ActionRenew

	// ActionHoldPrimary fires when this site owns the lease but the renewal
	// window has not opened yet (now < renewed+leaseSeconds/2). The site
	// stays Primary and the controller proceeds to the shared Access/DNS
	// reconcile, but it does NOT rewrite the lease record. This is what keeps
	// a Primary from renewing on every reconcile: without it, each renew
	// rewrites the lease timestamps in status, the self-watch re-enqueues,
	// and the Primary spins (hammering the Cloudflare DNS API and defeating a
	// peer takeover). Decision.Requeue is the time until the renewal window
	// opens.
	ActionHoldPrimary

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
	// ManualPromotion mirrors spec.failover.promotionPolicy == Manual. When
	// set, Decide never returns ActionAcquire off an expired or absent lease
	// on its own — only an explicit ForcePromote may acquire. The site stays
	// Standby (AwaitingPromotion) so failover is a deliberate act for
	// warm-infra/cold-app stateful services. Renewal of a self-held lease and
	// split-brain demotion are unaffected.
	ManualPromotion bool
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
	// AwaitingPromotion is set only when ManualPromotion suppressed an
	// acquire this site would otherwise have performed (expired peer lease,
	// or absent day-1 lease). The controller surfaces it as a Warning event +
	// pending-promotion metric + ReasonAwaitingPromotion so a DR pair that
	// nobody has promoted is visible rather than silently dark. It is false
	// for an ordinary Standby waiting on a live peer.
	AwaitingPromotion bool
}

// Decide reads the snapshot and returns the next role + action for this
// reconcile pass. Pure function — no IO, no global state. The controller
// dispatches the returned Action through the best-effort DNS lease writer
// and then writes the returned NextRole / LeaseOwner / LeaseExpiresAt into
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
	// caller's lease write + read-back verification succeeds.
	if in.Observed == nil {
		// Manual promotion never auto-elects a first Primary; only an
		// explicit force-promote may create the initial lease. Without it,
		// wait and surface AwaitingPromotion so a freshly-deployed,
		// unpromoted DR pair is visible rather than silently dark.
		if in.ManualPromotion && !in.ForcePromote {
			return Decision{
				NextRole:          RoleStandby,
				Action:            ActionWait,
				Requeue:           half,
				AwaitingPromotion: true,
			}
		}
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

	// I hold the lease. Renew only once the renewal window has opened
	// (now >= renewed+half); otherwise hold without rewriting the record so a
	// Primary does not renew on every reconcile.
	if isMine {
		renewAt := obs.Renewed.Add(half)
		if in.Now.Before(renewAt) {
			wait := max(renewAt.Sub(in.Now), time.Second)
			return Decision{
				NextRole:       RolePrimary,
				Action:         ActionHoldPrimary,
				LeaseOwner:     in.SiteID,
				LeaseExpiresAt: obs.Expires,
				Requeue:        wait,
			}
		}
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

	// A peer's lease has expired. Manual promotion suppresses the automatic
	// acquire: stay Standby and surface AwaitingPromotion until a
	// force-promote token arrives (ForcePromote is handled above, before this
	// branch, so reaching here means the token is absent).
	if in.ManualPromotion {
		return Decision{
			NextRole:          RoleStandby,
			Action:            ActionWait,
			LeaseOwner:        obs.Site,
			LeaseExpiresAt:    obs.Expires,
			Requeue:           half,
			AwaitingPromotion: true,
		}
	}

	// Automatic policy: try to acquire the expired lease.
	return Decision{
		NextRole:       RoleStandby,
		Action:         ActionAcquire,
		LeaseOwner:     obs.Site,
		LeaseExpiresAt: obs.Expires,
		Requeue:        half,
	}
}
