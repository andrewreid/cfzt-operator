package cloudflare

import "testing"

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
