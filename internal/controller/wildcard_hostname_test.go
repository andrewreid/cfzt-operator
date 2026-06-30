package controller

import "testing"

func TestCanonicalAccessApplicationDomainWildcard(t *testing.T) {
	cases := []struct {
		name     string
		hostname string
		domain   string
		want     string
		wantErr  bool
	}{
		{"wildcard bare host", "*.example.com", "*.example.com", "*.example.com", false},
		{"wildcard path", "*.example.com", "*.example.com/admin", "*.example.com/admin", false},
		{"wildcard star path canonicalizes to host", "*.example.com", "*.example.com/*", "*.example.com", false},
		{"domain outside wildcard host rejected", "*.example.com", "other.example.com", "", true},
		{"scheme rejected under wildcard host", "*.example.com", "https://*.example.com", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalAccessApplicationDomain(tc.hostname, tc.domain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("canonicalAccessApplicationDomain(%q,%q) = %q, want error", tc.hostname, tc.domain, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalAccessApplicationDomain(%q,%q) unexpected error: %v", tc.hostname, tc.domain, err)
			}
			if got != tc.want {
				t.Fatalf("canonicalAccessApplicationDomain(%q,%q) = %q, want %q", tc.hostname, tc.domain, got, tc.want)
			}
		})
	}
}

func TestWildcardCoversHost(t *testing.T) {
	cases := []struct {
		wildcard string
		host     string
		want     bool
	}{
		{"*.example.com", "foo.example.com", true},
		{"*.example.com", "bar.example.com", true},
		{"*.example.com", "foo.bar.example.com", false}, // single-leading-label only
		{"*.example.com", "example.com", false},         // apex not covered
		{"*.foo.example.com", "bar.foo.example.com", true},
		{"*.foo.example.com", "foo.example.com", false},
		{"*.example.com", "*.example.com", false}, // host is itself a wildcard
		{"example.com", "foo.example.com", false}, // not a wildcard
		{"*.example.com", "fooexample.com", false},
	}
	for _, tc := range cases {
		if got := wildcardCoversHost(tc.wildcard, tc.host); got != tc.want {
			t.Errorf("wildcardCoversHost(%q,%q) = %v, want %v", tc.wildcard, tc.host, got, tc.want)
		}
	}
}
