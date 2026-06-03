package ownership

import "k8s.io/apimachinery/pkg/types"

// Owner is the local-cluster authority for a Cloudflare resource. Its
// identity is rendered into the source-uid comment / tag chunks and read
// back during the mutation guard before any reconciler write or delete.
//
// For non-failover CRs the identity is metadata.uid — per-cluster, unique
// per object, so two clusters that accidentally apply identical YAML
// surface ForeignResource immediately.
//
// For D26 failover-enabled Exposures the identity is spec.failover.group
// — deliberately shared across the cluster pair so the Primary and
// Standby compute the same source-uid value and either cluster can take
// over the shared Access app / public CNAME without tripping the foreign
// guard. The wire format is identical in both modes; only the constructor
// differs.
type Owner struct {
	uid types.UID
}

func From(uid types.UID) Owner {
	return Owner{uid: uid}
}

// FromFailoverGroup constructs an Owner identified by a CloudflareExposure
// failover group ID instead of a per-CR UID. Both clusters in a failover
// pair construct identical Owners and therefore produce identical
// source-uid values, so MatchesComment and MatchesTags accept either
// cluster's writes against the shared Access application and public DNS
// CNAME. Non-failover Owners are unaffected — they still match only the
// per-CR UID they were constructed from, so the guard is no looser for
// the non-failover code path.
func FromFailoverGroup(group string) Owner {
	return Owner{uid: types.UID(group)}
}

func (o Owner) Comment() string {
	return renderComment(o.uid)
}

func (o Owner) CompactComment() string {
	return renderCompactComment(o.uid)
}

func (o Owner) Tags() []string {
	return renderAccessTags(o.uid)
}

func (o Owner) MatchesComment(s string) bool {
	uid, ok := parseComment(s)
	return ok && uid == o.uid
}

func (o Owner) MatchesTags(tags []string) bool {
	uid, ok := parseAccessTags(tags)
	return ok && uid == o.uid
}
