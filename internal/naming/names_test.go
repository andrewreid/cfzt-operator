package naming

import (
	"regexp"
	"strings"
	"testing"
)

func TestTokenSecretName(t *testing.T) {
	tests := []struct {
		tunnelName string
		want       string
	}{
		{"homelab", "homelab-token"},
		{"my-tunnel", "my-tunnel-token"},
		{"", "-token"},
	}
	for _, tc := range tests {
		if got := TokenSecretName(tc.tunnelName); got != tc.want {
			t.Errorf("TokenSecretName(%q) = %q, want %q", tc.tunnelName, got, tc.want)
		}
	}
}

func TestDaemonSetName(t *testing.T) {
	tests := []struct {
		tunnelName string
		want       string
	}{
		{"homelab", "cloudflared-homelab"},
		{"prod", "cloudflared-prod"},
	}
	for _, tc := range tests {
		if got := DaemonSetName(tc.tunnelName); got != tc.want {
			t.Errorf("DaemonSetName(%q) = %q, want %q", tc.tunnelName, got, tc.want)
		}
	}
}

func TestFailoverLeaseTXTName(t *testing.T) {
	const zone = "example.com"

	// Stable: the same group + zone always hashes to the same record.
	first := FailoverLeaseTXTName("jellyfin-dr", zone)
	second := FailoverLeaseTXTName("jellyfin-dr", zone)
	if first != second {
		t.Fatalf("FailoverLeaseTXTName not stable: %q vs %q", first, second)
	}

	// Shape: _cfzt-lease.<8 hex>.<zone>, zone suffix preserved.
	shape := regexp.MustCompile(`^_cfzt-lease\.[0-9a-f]{8}\.example\.com$`)
	if !shape.MatchString(first) {
		t.Fatalf("FailoverLeaseTXTName = %q, want match %v", first, shape)
	}
	if !strings.HasSuffix(first, "."+zone) {
		t.Fatalf("FailoverLeaseTXTName = %q, want zone suffix %q", first, zone)
	}

	// Distinct groups hash to distinct hash8 labels (no leak of the
	// group string into DNS, but still collision-distinguishable).
	other := FailoverLeaseTXTName("plex-dr", zone)
	if other == first {
		t.Fatalf("distinct groups produced same record name %q", first)
	}
	if strings.Contains(first, "jellyfin") {
		t.Fatalf("group string leaked into DNS name %q", first)
	}

	// Bounded: hash8 + fixed label keep the record well under the 63-char
	// DNS label / 253-char FQDN limits regardless of group length.
	longGroup := strings.Repeat("a", 63)
	long := FailoverLeaseTXTName(longGroup, zone)
	for label := range strings.SplitSeq(long, ".") {
		if len(label) > 63 {
			t.Fatalf("label %q exceeds 63 chars in %q", label, long)
		}
	}
}

func TestAccessAppName(t *testing.T) {
	tests := []struct {
		displayName  string
		metadataName string
		want         string
	}{
		{"My App", "my-app", "My App-cfzt"},
		{"", "my-app", "my-app-cfzt"},
		{"override", "ignored", "override-cfzt"},
	}
	for _, tc := range tests {
		if got := AccessAppName(tc.displayName, tc.metadataName); got != tc.want {
			t.Errorf("AccessAppName(%q, %q) = %q, want %q", tc.displayName, tc.metadataName, got, tc.want)
		}
	}
}
