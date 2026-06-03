package dr_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/dr"
)

func TestJitterBounded(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	spread := 5 * time.Second
	for range 2000 {
		j := dr.Jitter(spread, r)
		if j < -spread || j > spread {
			t.Fatalf("Jitter = %v, outside [-%v, +%v]", j, spread, spread)
		}
	}
	if dr.Jitter(0, r) != 0 {
		t.Fatalf("Jitter(0) = nonzero")
	}
	if dr.Jitter(-1*time.Second, r) != 0 {
		t.Fatalf("Jitter(-1s) = nonzero")
	}
}
