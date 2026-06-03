package dr

import (
	"math/rand"
	"time"
)

// Jitter returns a uniformly-distributed offset in the closed interval
// [-spread, +spread]. Used for two distinct timing decisions on the
// failover hot path: the per-site promotion delay (so two standbys racing
// to acquire an expired lease do not collide in the same instant) and the
// CAS-retry backoff (so two renewers that conflict do not retry in
// lockstep). Both consumers pass a *rand.Rand so tests are deterministic
// and the controller injects one seeded from process start.
//
// spread <= 0 returns 0 unconditionally; a nil rand is a programmer error
// and panics at the call site rather than silently returning 0, which would
// re-introduce thundering-herd collisions in production.
func Jitter(spread time.Duration, r *rand.Rand) time.Duration {
	if spread <= 0 {
		return 0
	}
	// r.Int63n(2*spread + 1) yields a value in [0, 2*spread]; subtracting
	// spread shifts the interval to [-spread, +spread] with both endpoints
	// reachable so the distribution is symmetric.
	n := r.Int63n(int64(2*spread) + 1)
	return time.Duration(n) - spread
}
