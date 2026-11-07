package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
)

func TestLimiterForTokenShared(t *testing.T) {
	first := limiterForToken("token-a")
	second := limiterForToken("token-a")
	other := limiterForToken("token-b")
	if first != second {
		t.Fatalf("same API token did not reuse limiter")
	}
	if first == other {
		t.Fatalf("different API tokens reused limiter")
	}
}

func TestRealClientReuse(t *testing.T) {
	clientCacheByCred = sync.Map{}

	first, err := New("account-1", "token-1")
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	second, err := New("account-1", "token-1")
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	other, err := New("account-1", "token-2")
	if err != nil {
		t.Fatalf("New other: %v", err)
	}

	if first != second {
		t.Fatalf("same credentials did not reuse RealClient")
	}
	if first == other {
		t.Fatalf("different credentials reused RealClient")
	}
}

func TestRealClientZoneCacheServesAcrossInstances(t *testing.T) {
	zoneCacheByCred = sync.Map{}
	key := newCacheKey("account-1", "token-1")
	cache := zoneCacheForCred(key)
	cache.mu.Lock()
	cache.zones = []Zone{{ID: testZoneID, Name: "example.com"}}
	cache.ready = true
	cache.mu.Unlock()

	first := &RealClient{cacheKey: key}
	second := &RealClient{cacheKey: key}
	if zone, err := first.Zones().Resolve(context.Background(), "app.example.com"); err != nil || zone.ID != testZoneID {
		t.Fatalf("first Resolve = (%+v, %v), want zone-1", zone, err)
	}
	if zone, err := second.Zones().Resolve(context.Background(), "other.example.com"); err != nil || zone.ID != testZoneID {
		t.Fatalf("second Resolve = (%+v, %v), want cached zone-1", zone, err)
	}
}

func TestMapAPIErrorNotFound(t *testing.T) {
	if err := mapAPIError(&cf.Error{StatusCode: http.StatusNotFound}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mapAPIError(404) = %v, want ErrNotFound", err)
	}
	sentinel := errors.New("boom")
	if err := mapAPIError(sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("mapAPIError(non-API) = %v, want sentinel", err)
	}
	if err := mapAPIError(nil); err != nil {
		t.Fatalf("mapAPIError(nil) = %v, want nil", err)
	}
}

func TestAccessApplicationListParamsUsesBroadDomainFilter(t *testing.T) {
	params := accessApplicationListParams("account-1", testHostname)
	if params.AccountID.Value != "account-1" {
		t.Fatalf("AccountID = %q, want account-1", params.AccountID.Value)
	}
	if params.Domain.Value != testHostname {
		t.Fatalf("Domain = %q, want broad hostname filter", params.Domain.Value)
	}
	if params.Exact.Value {
		t.Fatal("Exact = true, want broad domain filter without exact matching")
	}
}

func TestAccessAppFromListResponseCapturesDomainsAndPolicyIDs(t *testing.T) {
	app := accessAppFromListResponse(zero_trust.AccessApplicationListResponse{
		ID:     testAppID,
		Name:   testAppName,
		Domain: testHostname,
		SelfHostedDomains: []string{
			testHostname,
			testAdminDomain,
		},
		Policies: []zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		},
	})

	if !reflect.DeepEqual(app.Domains, []string{testHostname, testAdminDomain}) {
		t.Fatalf("Domains = %#v", app.Domains)
	}
	if !reflect.DeepEqual(app.PolicyUUIDs, []string{testPolicy1ID, testPolicy2ID}) {
		t.Fatalf("PolicyUUIDs = %#v", app.PolicyUUIDs)
	}
}

func TestAccessAppFromNewAndUpdateResponseRoundTrip(t *testing.T) {
	newResp := &zero_trust.AccessApplicationNewResponse{
		ID:     testAppID,
		Name:   testAppName,
		Domain: testHostname,
		SelfHostedDomains: []string{
			testHostname,
			testAdminDomain,
		},
		Policies: []zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		},
		Tags: []string{testManagedTag},
	}
	newApp := accessAppFromNewResponse(newResp)
	if newApp.Domain != testHostname {
		t.Fatalf("new Domain = %q", newApp.Domain)
	}
	if !reflect.DeepEqual(newApp.Domains, []string{testHostname, testAdminDomain}) {
		t.Fatalf("new Domains = %#v", newApp.Domains)
	}
	if !reflect.DeepEqual(newApp.PolicyUUIDs, []string{testPolicy1ID, testPolicy2ID}) {
		t.Fatalf("new PolicyUUIDs = %#v", newApp.PolicyUUIDs)
	}

	updateResp := &zero_trust.AccessApplicationUpdateResponse{
		ID:     testAppID,
		Name:   testAppName,
		Domain: testHostname,
		SelfHostedDomains: []string{
			testHostname,
			testAdminDomain,
		},
		Policies: []zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		},
		Tags: []string{testManagedTag},
	}
	updateApp := accessAppFromUpdateResponse(updateResp)
	if !reflect.DeepEqual(updateApp.Domains, newApp.Domains) {
		t.Fatalf("update Domains = %#v, want %#v", updateApp.Domains, newApp.Domains)
	}
	if !reflect.DeepEqual(updateApp.PolicyUUIDs, newApp.PolicyUUIDs) {
		t.Fatalf("update PolicyUUIDs = %#v, want %#v", updateApp.PolicyUUIDs, newApp.PolicyUUIDs)
	}
}

func TestSelfHostedPolicyIDsSortByPrecedence(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		policies := []zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		}
		original := append([]zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy(nil), policies...)

		if got := selfHostedListPolicyIDs(policies); !reflect.DeepEqual(got, []string{testPolicy1ID, testPolicy2ID}) {
			t.Fatalf("selfHostedListPolicyIDs = %#v", got)
		}
		if !reflect.DeepEqual(policies, original) {
			t.Fatalf("selfHostedListPolicyIDs mutated input: got %#v want %#v", policies, original)
		}
	})

	t.Run("new", func(t *testing.T) {
		policies := []zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		}
		original := append([]zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy(nil), policies...)

		if got := selfHostedNewPolicyIDs(policies); !reflect.DeepEqual(got, []string{testPolicy1ID, testPolicy2ID}) {
			t.Fatalf("selfHostedNewPolicyIDs = %#v", got)
		}
		if !reflect.DeepEqual(policies, original) {
			t.Fatalf("selfHostedNewPolicyIDs mutated input: got %#v want %#v", policies, original)
		}
	})

	t.Run("update", func(t *testing.T) {
		policies := []zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		}
		original := append([]zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy(nil), policies...)

		if got := selfHostedUpdatePolicyIDs(policies); !reflect.DeepEqual(got, []string{testPolicy1ID, testPolicy2ID}) {
			t.Fatalf("selfHostedUpdatePolicyIDs = %#v", got)
		}
		if !reflect.DeepEqual(policies, original) {
			t.Fatalf("selfHostedUpdatePolicyIDs mutated input: got %#v want %#v", policies, original)
		}
	})

	t.Run("get", func(t *testing.T) {
		policies := []zero_trust.AccessApplicationGetResponseSelfHostedApplicationPolicy{
			{ID: testPolicy2ID, Precedence: 20},
			{ID: testPolicy1ID, Precedence: 10},
		}
		original := append([]zero_trust.AccessApplicationGetResponseSelfHostedApplicationPolicy(nil), policies...)

		if got := selfHostedGetPolicyIDs(policies); !reflect.DeepEqual(got, []string{testPolicy1ID, testPolicy2ID}) {
			t.Fatalf("selfHostedGetPolicyIDs = %#v", got)
		}
		if !reflect.DeepEqual(policies, original) {
			t.Fatalf("selfHostedGetPolicyIDs mutated input: got %#v want %#v", policies, original)
		}
	})
}

func TestFromAccessRulesRejectsUnsupportedVariants(t *testing.T) {
	var rule zero_trust.AccessRule
	if err := json.Unmarshal([]byte(`{"group":{"id":"group-1"}}`), &rule); err != nil {
		t.Fatalf("Unmarshal AccessRule: %v", err)
	}

	_, err := fromAccessRules([]zero_trust.AccessRule{rule})
	if !errors.Is(err, ErrUnsupportedAccessRule) {
		t.Fatalf("fromAccessRules error = %v, want ErrUnsupportedAccessRule", err)
	}
}

func TestAccessBodiesSendPublicDestinations(t *testing.T) {
	in := AccessApplicationInput{
		Name:        testAppName,
		Domains:     []string{testHostname, testAdminDomain},
		PolicyUUIDs: []string{testPolicy1ID},
	}
	bodies := map[string]any{
		"new":    accessNewBody(in),
		"update": accessUpdateBody(in),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Domain       string              `json:"domain"`
				Destinations []map[string]string `json:"destinations"`
				Legacy       []string            `json:"self_hosted_domains"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			want := []map[string]string{
				{testKeyType: testDestinationPublic, testKeyURI: testHostname},
				{testKeyType: testDestinationPublic, testKeyURI: testAdminDomain},
			}
			if !reflect.DeepEqual(got.Destinations, want) {
				t.Fatalf("destinations = %#v, want %#v", got.Destinations, want)
			}
			if got.Legacy != nil {
				t.Fatalf("self_hosted_domains = %#v, want omitted", got.Legacy)
			}
			if got.Domain != testHostname {
				t.Fatalf("domain = %q, want %q", got.Domain, testHostname)
			}
		})
	}
}

func publicDestinations(uris ...string) []any {
	out := make([]any, 0, len(uris))
	for _, uri := range uris {
		out = append(out, map[string]any{testKeyType: testDestinationPublic, testKeyURI: uri})
	}
	return out
}

func TestAccessResponseDomainsRoundTrip(t *testing.T) {
	multi := []string{testHostname, testAdminDomain, "*.example.com/app"}
	legacyOnly := []string{testHostname, testAdminDomain}
	cases := []struct {
		name         string
		destinations any
		selfHosted   any
		domain       string
		want         []string
	}{
		{"destinations only", publicDestinations(multi...), nil, testHostname, multi},
		{"legacy only", nil, legacyOnly, testHostname, legacyOnly},
		{"legacy only decoded as []any", nil, []any{testHostname, testAdminDomain}, testHostname, legacyOnly},
		{"both prefers destinations", publicDestinations(multi...), legacyOnly, testHostname, multi},
		{"neither falls back to domain", nil, nil, testHostname, []string{testHostname}},
		{"unknown and non-public entries ignored",
			[]any{
				"junk",
				map[string]any{testKeyType: "private", "cidr": "10.0.0.0/8"},
				map[string]any{testKeyType: testDestinationPublic},
				map[string]any{testKeyType: testDestinationPublic, testKeyURI: testAdminDomain},
			}, legacyOnly, testHostname, []string{testAdminDomain}},
		{"only non-public entries falls back to legacy",
			[]any{map[string]any{testKeyType: "private", testKeyURI: "x"}}, legacyOnly, testHostname, legacyOnly},
		{"malformed destinations value", "nope", nil, testHostname, []string{testHostname}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list := accessAppFromListResponse(zero_trust.AccessApplicationListResponse{
				ID: testAppID, Name: testAppName, Domain: tc.domain,
				Destinations: tc.destinations, SelfHostedDomains: tc.selfHosted,
			})
			created := accessAppFromNewResponse(&zero_trust.AccessApplicationNewResponse{
				ID: testAppID, Name: testAppName, Domain: tc.domain,
				Destinations: tc.destinations, SelfHostedDomains: tc.selfHosted,
			})
			updated := accessAppFromUpdateResponse(&zero_trust.AccessApplicationUpdateResponse{
				ID: testAppID, Name: testAppName, Domain: tc.domain,
				Destinations: tc.destinations, SelfHostedDomains: tc.selfHosted,
			})
			for label, got := range map[string][]string{"list": list.Domains, "new": created.Domains, "update": updated.Domains} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("%s Domains = %#v, want %#v", label, got, tc.want)
				}
			}
		})
	}
}

// A request built from desired domains must read back identically from a
// destinations-only response, otherwise multi-path apps would drift forever.
func TestAccessDestinationsRequestResponseNoDrift(t *testing.T) {
	desired := []string{testHostname, testAdminDomain, "*.example.com"}
	raw, err := json.Marshal(accessNewBody(AccessApplicationInput{Name: testAppName, Domains: desired}))
	if err != nil {
		t.Fatal(err)
	}
	var echoed struct {
		Destinations any `json:"destinations"`
	}
	if err := json.Unmarshal(raw, &echoed); err != nil {
		t.Fatal(err)
	}
	app := accessAppFromNewResponse(&zero_trust.AccessApplicationNewResponse{
		ID: testAppID, Name: testAppName, Domain: testHostname, Destinations: echoed.Destinations,
	})
	if !reflect.DeepEqual(app.Domains, desired) {
		t.Fatalf("Domains = %#v, want %#v", app.Domains, desired)
	}
}
