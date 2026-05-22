package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

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

func TestAccessAppFromListResponseCapturesAllPolicyIDs(t *testing.T) {
	app := accessAppFromListResponse(zero_trust.AccessApplicationListResponse{
		ID:     "app-1",
		Name:   "jellyfin-cfzt",
		Domain: "jellyfin.example.com",
		Policies: []zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy{
			{ID: "policy-1"},
			{ID: "policy-2"},
		},
	})

	if app.PolicyUUID != "policy-1" {
		t.Fatalf("PolicyUUID = %q, want policy-1", app.PolicyUUID)
	}
	if !reflect.DeepEqual(app.PolicyUUIDs, []string{"policy-1", "policy-2"}) {
		t.Fatalf("PolicyUUIDs = %#v", app.PolicyUUIDs)
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
