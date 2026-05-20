package cloudflare

import (
	"encoding/json"
	"errors"
	"reflect"
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
