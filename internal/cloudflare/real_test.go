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
	cache.zones = []Zone{{ID: "zone-1", Name: "example.com"}}
	cache.ready = true
	cache.mu.Unlock()

	first := &RealClient{cacheKey: key}
	second := &RealClient{cacheKey: key}
	if zone, err := first.Zones().Resolve(context.Background(), "app.example.com"); err != nil || zone.ID != "zone-1" {
		t.Fatalf("first Resolve = (%+v, %v), want zone-1", zone, err)
	}
	if zone, err := second.Zones().Resolve(context.Background(), "other.example.com"); err != nil || zone.ID != "zone-1" {
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

func TestAccessAppFromListResponseCapturesDomainsAndPolicyIDs(t *testing.T) {
	app := accessAppFromListResponse(zero_trust.AccessApplicationListResponse{
		ID:     "app-1",
		Name:   "jellyfin-cfzt",
		Domain: "jellyfin.example.com",
		SelfHostedDomains: []string{
			"jellyfin.example.com",
			"jellyfin.example.com/admin",
		},
		Policies: []zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy{
			{ID: "policy-1"},
			{ID: "policy-2"},
		},
	})

	if !reflect.DeepEqual(app.Domains, []string{"jellyfin.example.com", "jellyfin.example.com/admin"}) {
		t.Fatalf("Domains = %#v", app.Domains)
	}
	if !reflect.DeepEqual(app.PolicyUUIDs, []string{"policy-1", "policy-2"}) {
		t.Fatalf("PolicyUUIDs = %#v", app.PolicyUUIDs)
	}
}

func TestAccessAppFromNewAndUpdateResponseRoundTrip(t *testing.T) {
	newResp := &zero_trust.AccessApplicationNewResponse{
		ID:     "app-1",
		Name:   "jellyfin-cfzt",
		Domain: "jellyfin.example.com",
		SelfHostedDomains: []string{
			"jellyfin.example.com",
			"jellyfin.example.com/admin",
		},
		Policies: []zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy{
			{ID: "policy-2"},
			{ID: "policy-1"},
		},
		Tags: []string{"managed-by=cfzt-operator"},
	}
	newApp := accessAppFromNewResponse(newResp)
	if newApp.Domain != "jellyfin.example.com" {
		t.Fatalf("new Domain = %q", newApp.Domain)
	}
	if !reflect.DeepEqual(newApp.Domains, []string{"jellyfin.example.com", "jellyfin.example.com/admin"}) {
		t.Fatalf("new Domains = %#v", newApp.Domains)
	}
	if !reflect.DeepEqual(newApp.PolicyUUIDs, []string{"policy-2", "policy-1"}) {
		t.Fatalf("new PolicyUUIDs = %#v", newApp.PolicyUUIDs)
	}

	updateResp := &zero_trust.AccessApplicationUpdateResponse{
		ID:     "app-1",
		Name:   "jellyfin-cfzt",
		Domain: "jellyfin.example.com",
		SelfHostedDomains: []string{
			"jellyfin.example.com",
			"jellyfin.example.com/admin",
		},
		Policies: []zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy{
			{ID: "policy-2"},
			{ID: "policy-1"},
		},
		Tags: []string{"managed-by=cfzt-operator"},
	}
	updateApp := accessAppFromUpdateResponse(updateResp)
	if !reflect.DeepEqual(updateApp.Domains, newApp.Domains) {
		t.Fatalf("update Domains = %#v, want %#v", updateApp.Domains, newApp.Domains)
	}
	if !reflect.DeepEqual(updateApp.PolicyUUIDs, newApp.PolicyUUIDs) {
		t.Fatalf("update PolicyUUIDs = %#v, want %#v", updateApp.PolicyUUIDs, newApp.PolicyUUIDs)
	}
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
