package naming

import (
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
