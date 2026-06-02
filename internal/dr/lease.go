// Package dr contains the cross-cluster active-passive failover primitives
// shared by the Exposure reconciler and its envtest/live-smoke harnesses.
// The package is pure — no Kubernetes, no Cloudflare API — so the lease
// serde, CAS retry loop, and role state machine are unit-testable in
// isolation. See spec.md ## DR failover (D26) for the contract.
package dr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LeaseSchemaVersion is the only schema version ParseLease currently accepts.
// A future schema change bumps this and Parse explicitly rejects mismatches
// so a half-upgraded cluster pair never silently mis-interprets a peer's
// lease.
const LeaseSchemaVersion = 1

// ErrLeaseMalformed wraps every payload-shape rejection from ParseLease so
// callers can distinguish "the peer wrote garbage" from upstream IO errors.
var ErrLeaseMalformed = errors.New("dr: malformed lease payload")

// Lease is the parsed payload of one failover lease TXT record. The wire
// form is a single TXT string of whitespace-separated key=value tokens,
// per spec.md ## DR failover ### Lease record:
//
//	v=1 site=<site-id> tunnel=<tunnelId> exp=<unix-epoch> renewed=<unix-epoch>
//
// Times are unix seconds in UTC. Site is the holder's --site-id. Tunnel is
// the holder's Cloudflare tunnel ID; it lets a Standby observer correlate
// the public CNAME's target with the lease for split-brain diagnostics.
type Lease struct {
	Version int
	Site    string
	Tunnel  string
	Expires time.Time
	Renewed time.Time
}

// ParseLease decodes a TXT payload into a Lease. Tokens may appear in any
// order; unknown keys are rejected so a corrupted or future-version record
// does not silently drop data the operator's arbitration depends on.
// Duplicate keys are rejected for the same reason.
func ParseLease(s string) (Lease, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return Lease{}, fmt.Errorf("%w: empty payload", ErrLeaseMalformed)
	}
	var out Lease
	seen := map[string]struct{}{}
	for _, tok := range fields {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || k == "" || v == "" {
			return Lease{}, fmt.Errorf("%w: token %q is not key=value", ErrLeaseMalformed, tok)
		}
		if _, dup := seen[k]; dup {
			return Lease{}, fmt.Errorf("%w: duplicate key %q", ErrLeaseMalformed, k)
		}
		seen[k] = struct{}{}
		switch k {
		case "v":
			n, err := strconv.Atoi(v)
			if err != nil {
				return Lease{}, fmt.Errorf("%w: v=%q: %v", ErrLeaseMalformed, v, err)
			}
			out.Version = n
		case "site":
			out.Site = v
		case "tunnel":
			out.Tunnel = v
		case "exp":
			ts, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return Lease{}, fmt.Errorf("%w: exp=%q: %v", ErrLeaseMalformed, v, err)
			}
			out.Expires = time.Unix(ts, 0).UTC()
		case "renewed":
			ts, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return Lease{}, fmt.Errorf("%w: renewed=%q: %v", ErrLeaseMalformed, v, err)
			}
			out.Renewed = time.Unix(ts, 0).UTC()
		default:
			return Lease{}, fmt.Errorf("%w: unknown key %q", ErrLeaseMalformed, k)
		}
	}
	if out.Version != LeaseSchemaVersion {
		return Lease{}, fmt.Errorf("%w: v=%d unsupported (want %d)", ErrLeaseMalformed, out.Version, LeaseSchemaVersion)
	}
	if out.Site == "" || out.Tunnel == "" || out.Expires.IsZero() || out.Renewed.IsZero() {
		return Lease{}, fmt.Errorf("%w: missing required fields", ErrLeaseMalformed)
	}
	return out, nil
}

// Serialize encodes a Lease into the canonical TXT payload form. Field order
// is fixed (v, site, tunnel, exp, renewed) so identical leases hash and
// `dig`-dump identically — eases split-brain diagnostics in the field.
func (l Lease) Serialize() string {
	return fmt.Sprintf("v=%d site=%s tunnel=%s exp=%d renewed=%d",
		l.Version, l.Site, l.Tunnel, l.Expires.UTC().Unix(), l.Renewed.UTC().Unix())
}

// Expired reports whether the lease has expired at the supplied wall-clock
// instant. Equality counts as expired so two clusters with identical clocks
// race deterministically at the moment of expiry rather than oscillating
// around it.
func (l Lease) Expired(now time.Time) bool {
	return !now.Before(l.Expires)
}
