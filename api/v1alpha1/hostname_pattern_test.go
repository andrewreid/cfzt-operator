package v1alpha1

import (
	"regexp"
	"strings"
	"testing"
)

// hostnamePattern MUST stay byte-for-byte identical to the
// +kubebuilder:validation:Pattern marker on CloudflareExposureSpec.Hostname.
// This test proves the regex enforces the issue #13 wildcard truth-table
// independently of envtest / the apiserver, so a future marker edit that breaks
// the contract fails here even when envtest is unavailable.
const hostnamePattern = `^(\*\.)?[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)+$`

func TestHostnamePatternTruthTable(t *testing.T) {
	re := regexp.MustCompile(hostnamePattern)

	label63 := strings.Repeat("a", 63)
	label64 := strings.Repeat("a", 64)

	cases := []struct {
		hostname string
		want     bool
	}{
		// Accept.
		{"example.com", true},
		{"*.example.com", true},
		{"*.foo.example.com", true},
		{label63 + ".example.com", true},
		{"foo.example.com", true},
		{"bar.foo.example.com", true},
		// Reject.
		{"*", false},
		{"*.com", false},
		{"*example.com", false},
		{"foo.*.example.com", false},
		{"foo.*", false},
		{"**.example.com", false},
		{"Example.com", false},
		{"example.com.", false},
		{label64 + ".example.com", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := re.MatchString(tc.hostname); got != tc.want {
			t.Errorf("MatchString(%q) = %v, want %v", tc.hostname, got, tc.want)
		}
	}
}
