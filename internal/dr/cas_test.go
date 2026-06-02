package dr_test

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/dr"
)

var errFakeConflict = errors.New("fake CAS conflict")

func isFakeConflict(err error) bool { return errors.Is(err, errFakeConflict) }

func newTestRand() *rand.Rand { return rand.New(rand.NewSource(42)) }

func TestFailoverCASConflictRetries(t *testing.T) {
	attempts := 0
	op := func() error {
		attempts++
		if attempts < 3 {
			return errFakeConflict
		}
		return nil
	}
	cfg := dr.RetryConfig{MaxAttempts: 5, BaseBackoff: time.Microsecond, MaxBackoff: time.Microsecond, Jitter: 0}
	if err := dr.CASRetry(context.Background(), cfg, newTestRand(), isFakeConflict, op); err != nil {
		t.Fatalf("CASRetry err = %v, want nil after 3rd attempt success", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two conflicts then success)", attempts)
	}
}

func TestCASRetryExhausts(t *testing.T) {
	attempts := 0
	op := func() error { attempts++; return errFakeConflict }
	cfg := dr.RetryConfig{MaxAttempts: 4, BaseBackoff: time.Microsecond, MaxBackoff: time.Microsecond}
	err := dr.CASRetry(context.Background(), cfg, newTestRand(), isFakeConflict, op)
	if !errors.Is(err, dr.ErrLeaseConflictExhausted) {
		t.Fatalf("err = %v, want ErrLeaseConflictExhausted", err)
	}
	if !errors.Is(err, errFakeConflict) {
		t.Fatalf("err = %v, want wrapped fake conflict for diagnostics", err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4 (MaxAttempts)", attempts)
	}
}

func TestCASRetryNonConflictShortCircuits(t *testing.T) {
	wantErr := errors.New("network down")
	attempts := 0
	op := func() error { attempts++; return wantErr }
	cfg := dr.RetryConfig{MaxAttempts: 5, BaseBackoff: time.Microsecond}
	err := dr.CASRetry(context.Background(), cfg, newTestRand(), isFakeConflict, op)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v (non-conflict must short-circuit)", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (non-conflict must not retry)", attempts)
	}
}

func TestCASRetryRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := func() error { return errFakeConflict }
	cfg := dr.RetryConfig{MaxAttempts: 4, BaseBackoff: time.Second}
	err := dr.CASRetry(ctx, cfg, newTestRand(), isFakeConflict, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled propagated through sleep", err)
	}
}

func TestJitterBounded(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	spread := 5 * time.Second
	for i := range 2000 {
		j := dr.Jitter(spread, r)
		if j < -spread || j > spread {
			t.Fatalf("Jitter[%d] = %v, outside [-%v, +%v]", i, j, spread, spread)
		}
	}
	if dr.Jitter(0, r) != 0 {
		t.Fatalf("Jitter(0) = nonzero")
	}
	if dr.Jitter(-1*time.Second, r) != 0 {
		t.Fatalf("Jitter(-1s) = nonzero")
	}
}
