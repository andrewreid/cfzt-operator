package dr

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// ErrLeaseConflictExhausted is returned by CASRetry when every attempt
// observed a CAS conflict and the retry budget is spent. The Exposure
// controller surfaces this as Ready=False, Reason=LeaseConflict and
// requeues per the 30s waiting-state floor in the requeue policy.
var ErrLeaseConflictExhausted = errors.New("dr: lease CAS conflict after retries")

// RetryConfig bounds the CAS retry loop. Defaults from DefaultRetryConfig
// are tuned so a transient peer-renewal collision converges in well under
// one reconcile budget; production callers override only on a strong
// reason.
type RetryConfig struct {
	// MaxAttempts is the total number of op invocations, including the
	// first. Values < 1 are clamped to 1.
	MaxAttempts int
	// BaseBackoff is the initial sleep between attempts. Doubles each
	// retry, capped at MaxBackoff.
	BaseBackoff time.Duration
	// MaxBackoff caps the doubling so backoff cannot grow without bound
	// when MaxAttempts is large. 0 disables the cap.
	MaxBackoff time.Duration
	// Jitter is the symmetric per-attempt randomisation applied on top of
	// the backoff so two racing renewers desynchronise.
	Jitter time.Duration
}

// DefaultRetryConfig returns the production CAS retry budget: 4 attempts,
// 200ms base / 2s ceiling exponential backoff, ±100ms jitter per attempt.
// At MaxAttempts=4 the worst-case wall-clock cost is bounded by roughly
// 200 + 400 + 800 ms of sleep before the final attempt, comfortably under
// the controller's 30s waiting-state requeue floor.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 4,
		BaseBackoff: 200 * time.Millisecond,
		MaxBackoff:  2 * time.Second,
		Jitter:      100 * time.Millisecond,
	}
}

// CASRetry invokes op up to cfg.MaxAttempts times, retrying only when
// isConflict reports the returned error as a CAS conflict. Any other error
// short-circuits and returns immediately, so a transient network failure
// surfaces as a controller-runtime backoff-eligible error instead of being
// masked as a lease conflict. Context cancellation is observed during
// inter-attempt sleeps.
//
// On the final attempt no further sleep occurs; if it still conflicts the
// loop returns ErrLeaseConflictExhausted wrapping the last conflict so the
// controller can match on errors.Is.
func CASRetry(ctx context.Context, cfg RetryConfig, r *rand.Rand, isConflict func(error) bool, op func() error) error {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	backoff := cfg.BaseBackoff
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	var lastConflict error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if !isConflict(err) {
			return err
		}
		lastConflict = err
		if attempt == cfg.MaxAttempts-1 {
			break
		}
		sleep := max(backoff+Jitter(cfg.Jitter, r), 0)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
		backoff *= 2
		if cfg.MaxBackoff > 0 && backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	// Join the sentinel with the last observed conflict so callers can match
	// either via errors.Is: ErrLeaseConflictExhausted for the role-gate
	// fallback path, and the underlying conflict for diagnostic logging.
	return errors.Join(ErrLeaseConflictExhausted, lastConflict)
}
