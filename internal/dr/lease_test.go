package dr_test

import (
	"errors"
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/dr"
)

func TestLeaseParseSerializeRoundTrip(t *testing.T) {
	in := dr.Lease{
		Version: 1,
		Site:    "homelab-primary",
		Tunnel:  "00000000-0000-4000-8000-000000000001",
		Expires: time.Unix(1700000060, 0).UTC(),
		Renewed: time.Unix(1700000000, 0).UTC(),
	}
	wire := in.Serialize()
	want := "v=1 site=homelab-primary tunnel=00000000-0000-4000-8000-000000000001 exp=1700000060 renewed=1700000000"
	if wire != want {
		t.Fatalf("Serialize = %q, want %q", wire, want)
	}
	out, err := dr.ParseLease(wire)
	if err != nil {
		t.Fatalf("ParseLease returned error: %v", err)
	}
	if !out.Expires.Equal(in.Expires) || !out.Renewed.Equal(in.Renewed) {
		t.Fatalf("times round-tripped wrong: got exp=%v renewed=%v want exp=%v renewed=%v", out.Expires, out.Renewed, in.Expires, in.Renewed)
	}
	out.Expires = in.Expires
	out.Renewed = in.Renewed
	if out != in {
		t.Fatalf("round-trip = %#v, want %#v", out, in)
	}
}

func TestLeaseParseOrderAgnostic(t *testing.T) {
	wire := "renewed=1700000000 tunnel=t-1 site=primary exp=1700000060 v=1"
	out, err := dr.ParseLease(wire)
	if err != nil {
		t.Fatalf("ParseLease returned error: %v", err)
	}
	if out.Site != "primary" || out.Tunnel != "t-1" {
		t.Fatalf("parsed = %#v", out)
	}
}

func TestLeaseParseRejects(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"missing version":  "site=primary tunnel=t-1 exp=1700000060 renewed=1700000000",
		"unsupported v":    "v=2 site=primary tunnel=t-1 exp=1700000060 renewed=1700000000",
		"unknown key":      "v=1 site=primary tunnel=t-1 exp=1700000060 renewed=1700000000 mode=primary",
		"duplicate key":    "v=1 site=primary site=peer tunnel=t-1 exp=1700000060 renewed=1700000000",
		"missing required": "v=1 site=primary tunnel=t-1 exp=1700000060",
		"non-numeric exp":  "v=1 site=primary tunnel=t-1 exp=soon renewed=1700000000",
		"bad token shape":  "v=1 site=primary tunnelt-1 exp=1700000060 renewed=1700000000",
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := dr.ParseLease(wire); !errors.Is(err, dr.ErrLeaseMalformed) {
				t.Fatalf("ParseLease(%q) err = %v, want ErrLeaseMalformed", wire, err)
			}
		})
	}
}

func TestLeaseExpired(t *testing.T) {
	l := dr.Lease{Expires: time.Unix(1000, 0).UTC()}
	if l.Expired(time.Unix(999, 0).UTC()) {
		t.Fatalf("Expired(now=999) = true, want false (lease still live)")
	}
	if !l.Expired(time.Unix(1000, 0).UTC()) {
		t.Fatalf("Expired(now=expiry) = false, want true (deterministic race resolution)")
	}
	if !l.Expired(time.Unix(1001, 0).UTC()) {
		t.Fatalf("Expired(now=1001) = false, want true")
	}
}
