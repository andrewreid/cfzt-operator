package naming

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestTagRoundTrip(t *testing.T) {
	uid := types.UID("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	tag := OwnershipTag(uid)
	got, ok := ParseOwnershipTag(tag)
	if !ok {
		t.Fatalf("ParseOwnershipTag(%q) ok=false, want true", tag)
	}
	if got != uid {
		t.Errorf("ParseOwnershipTag round-trip: got %q, want %q", got, uid)
	}
}

func TestParseOwnershipTag(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantUID types.UID
		wantOK  bool
	}{
		{
			name:    "valid canonical",
			input:   "managed-by=cfzt-operator source-uid=abc-123",
			wantUID: "abc-123",
			wantOK:  true,
		},
		{
			name:    "extra whitespace tolerated",
			input:   "managed-by=cfzt-operator   source-uid=abc-123",
			wantUID: "abc-123",
			wantOK:  true,
		},
		{
			name:   "empty string rejected",
			input:  "",
			wantOK: false,
		},
		{
			name:   "wrong managed-by value rejected",
			input:  "managed-by=other source-uid=abc-123",
			wantOK: false,
		},
		{
			name:   "missing source-uid rejected",
			input:  "managed-by=cfzt-operator",
			wantOK: false,
		},
		{
			name:   "missing managed-by rejected",
			input:  "source-uid=abc-123",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseOwnershipTag(tc.input)
			if ok != tc.wantOK {
				t.Errorf("ok=%v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantUID {
				t.Errorf("uid=%q, want %q", got, tc.wantUID)
			}
		})
	}
}
