package cloudflare

import "testing"

func TestLimiterForTokenShared(t *testing.T) {
	if limiterForToken("token-a") != limiterForToken("token-a") {
		t.Fatalf("same API token did not reuse limiter")
	}
	if limiterForToken("token-a") == limiterForToken("token-b") {
		t.Fatalf("different API tokens reused limiter")
	}
}
