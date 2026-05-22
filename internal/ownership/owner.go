package ownership

import "k8s.io/apimachinery/pkg/types"

type Owner struct {
	uid types.UID
}

func From(uid types.UID) Owner {
	return Owner{uid: uid}
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
