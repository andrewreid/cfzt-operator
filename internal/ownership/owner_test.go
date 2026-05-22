package ownership

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestOwnerCommentRoundTrip(t *testing.T) {
	uid := types.UID("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	owner := From(uid)

	if got := owner.Comment(); got != "managed-by=cfzt-operator source-uid="+string(uid) {
		t.Fatalf("Comment() = %q", got)
	}
	if !owner.MatchesComment(owner.Comment()) {
		t.Fatalf("MatchesComment(%q) = false, want true", owner.Comment())
	}
	if !owner.MatchesComment(owner.Comment() + " | user comment") {
		t.Fatalf("MatchesComment with suffix = false, want true")
	}
}

func TestOwnerCompactCommentRoundTrip(t *testing.T) {
	uid := types.UID("route-uid")
	owner := From(uid)

	if got := owner.CompactComment(); got != "managed-by=cfzt source-uid="+string(uid) {
		t.Fatalf("CompactComment() = %q", got)
	}
	if !owner.MatchesComment(owner.CompactComment()) {
		t.Fatalf("MatchesComment(%q) = false, want true", owner.CompactComment())
	}
}

func TestOwnerAccessTagChunkRoundTrip(t *testing.T) {
	uid := types.UID("c4fe2a8a-39b3-48cd-9a2a-d471cb045b4a")
	owner := From(uid)
	tags := owner.Tags()

	if len(tags) < 2 {
		t.Fatalf("Tags() = %v, want managed-by plus chunks", tags)
	}
	for _, tag := range tags {
		if len(tag) > accessTagMaxLength {
			t.Fatalf("tag %q length = %d, want <= %d", tag, len(tag), accessTagMaxLength)
		}
	}
	if !owner.MatchesTags(tags) {
		t.Fatalf("MatchesTags(%v) = false, want true", tags)
	}
}

func TestOwnerMatchesForeign(t *testing.T) {
	owner := From(types.UID("local"))
	foreign := From(types.UID("foreign"))

	if owner.MatchesComment(foreign.Comment()) {
		t.Fatalf("MatchesComment(foreign) = true, want false")
	}
	if owner.MatchesComment("managed-by=other source-uid=local") {
		t.Fatalf("MatchesComment(wrong managed-by) = true, want false")
	}
	if owner.MatchesTags(foreign.Tags()) {
		t.Fatalf("MatchesTags(foreign) = true, want false")
	}
	if owner.MatchesTags([]string{"managed-by=cfzt-operator", "source-uid=local"}) {
		t.Fatalf("MatchesTags(direct source-uid tag) = true, want false")
	}
	if owner.MatchesTags([]string{"managed-by=cfzt-operator", "source-uid-1=local"}) {
		t.Fatalf("MatchesTags(missing chunk zero) = true, want false")
	}
	if owner.MatchesComment(strings.Replace(owner.Comment(), "source-uid=", "source=", 1)) {
		t.Fatalf("MatchesComment(missing source uid) = true, want false")
	}
}
