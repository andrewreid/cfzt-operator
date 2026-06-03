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

func TestFailoverOwnershipTagAcceptsGroupID(t *testing.T) {
	// Two clusters in a failover pair both reconcile the same logical
	// exposure with spec.failover.group="jellyfin-dr". They each construct
	// an Owner from that group and must produce identical comment / tag
	// renders, so either cluster can mutate the shared Access app and DNS
	// CNAME without surfacing ForeignResource.
	const group = "jellyfin-dr"
	primary := FromFailoverGroup(group)
	standby := FromFailoverGroup(group)

	if primary.Comment() != standby.Comment() {
		t.Fatalf("two clusters disagree on Comment: %q vs %q", primary.Comment(), standby.Comment())
	}
	if !standby.MatchesComment(primary.Comment()) {
		t.Fatalf("standby does not accept primary's failover-group comment %q", primary.Comment())
	}
	if !primary.MatchesTags(standby.Tags()) {
		t.Fatalf("primary does not accept standby's failover-group tags %v", standby.Tags())
	}
	// Compact form (used on DNS records owned by Routes / Tunnels) must
	// also round-trip across the pair.
	if !standby.MatchesComment(primary.CompactComment()) {
		t.Fatalf("standby does not accept primary's compact comment %q", primary.CompactComment())
	}

	// Negative direction: a non-failover Owner constructed from a per-CR
	// UID that happens to differ from the group MUST NOT match the
	// failover-group renders. The relaxation is opt-in via the
	// FromFailoverGroup constructor; the From(uid) guard stays tight.
	other := From(types.UID("some-cr-uid"))
	if other.MatchesComment(primary.Comment()) {
		t.Fatalf("non-failover Owner matches failover-group comment")
	}
	if other.MatchesTags(primary.Tags()) {
		t.Fatalf("non-failover Owner matches failover-group tags")
	}

	// Symmetric: a failover-group Owner MUST NOT match an unrelated
	// per-CR Owner's writes — the group ID must literally equal the
	// stamped source-uid for the match to succeed.
	if primary.MatchesComment(other.Comment()) {
		t.Fatalf("failover-group Owner matches unrelated per-CR comment")
	}
	if primary.MatchesTags(other.Tags()) {
		t.Fatalf("failover-group Owner matches unrelated per-CR tags")
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
