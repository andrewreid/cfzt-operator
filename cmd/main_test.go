/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import "testing"

// TestFailoverSiteIDMandatoryAtBoot covers D26: empty --site-id is a fatal
// start-up error; any non-empty value passes the boot guard. Identity is a
// process-level invariant — there is no per-Exposure "SiteIDMissing" reason.
func TestFailoverSiteIDMandatoryAtBoot(t *testing.T) {
	tests := []struct {
		name    string
		siteID  string
		wantErr bool
	}{
		{name: "empty rejected", siteID: "", wantErr: true},
		{name: "whitespace rejected", siteID: "   ", wantErr: true},
		{name: "tab+newline rejected", siteID: "\t\n", wantErr: true},
		{name: "stable id accepted", siteID: "homelab-primary", wantErr: false},
		{name: "default upgrade id accepted", siteID: "cfzt-default-site", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSiteID(tc.siteID)
			if tc.wantErr && err == nil {
				t.Fatalf("validateSiteID(%q) = nil, want error", tc.siteID)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateSiteID(%q) = %v, want nil", tc.siteID, err)
			}
		})
	}
}
