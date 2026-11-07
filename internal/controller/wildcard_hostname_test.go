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
		{"wildcard bare host", testWildcardHostname, testWildcardHostname, testWildcardHostname, false},
		{"wildcard path", testWildcardHostname, "*.example.com/admin", "*.example.com/admin", false},
		{"wildcard star path canonicalizes to host", testWildcardHostname, "*.example.com/*", testWildcardHostname, false},
		{"domain outside wildcard host rejected", testWildcardHostname, "other.example.com", "", true},
		{"scheme rejected under wildcard host", testWildcardHostname, "https://*.example.com", "", true},
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
		{testWildcardHostname, testFooHostname, true},
		{testWildcardHostname, "bar.example.com", true},
		{testWildcardHostname, "foo.bar.example.com", false}, // single-leading-label only
		{testWildcardHostname, testZoneName, false},          // apex not covered
		{"*.foo.example.com", "bar.foo.example.com", true},
		{"*.foo.example.com", testFooHostname, false},
		{testWildcardHostname, testWildcardHostname, false}, // host is itself a wildcard
		{testZoneName, testFooHostname, false},              // not a wildcard
		{testWildcardHostname, "fooexample.com", false},
	}
	for _, tc := range cases {
		if got := wildcardCoversHost(tc.wildcard, tc.host); got != tc.want {
			t.Errorf("wildcardCoversHost(%q,%q) = %v, want %v", tc.wildcard, tc.host, got, tc.want)
		}
	}
}
