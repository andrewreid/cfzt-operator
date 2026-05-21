package naming

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	tagManagedBy        = "managed-by=cfzt-operator"
	tagManagedByCompact = "managed-by=cfzt"
	tagSourceUID        = "source-uid="
)

// OwnershipTag returns the canonical operator tag string for a CR UID.
// The space-separated format matches Cloudflare's multi-tag comment convention
// and is stable across versions so ParseOwnershipTag can always decode it.
func OwnershipTag(uid types.UID) string {
	return fmt.Sprintf("%s %s%s", tagManagedBy, tagSourceUID, uid)
}

// CompactOwnershipTag returns the short ownership text used where Cloudflare
// enforces very small comment limits.
func CompactOwnershipTag(uid types.UID) string {
	return fmt.Sprintf("%s %s%s", tagManagedByCompact, tagSourceUID, uid)
}

// ParseOwnershipTag returns (uid, ok). ok=false when the string is not an
// operator-owned tag: missing managed-by token or missing source-uid token.
// Extra whitespace between tokens is tolerated.
func ParseOwnershipTag(s string) (types.UID, bool) {
	fields := strings.Fields(s)

	hasManagedBy := false
	uid := ""

	for _, f := range fields {
		if f == tagManagedBy || f == tagManagedByCompact {
			hasManagedBy = true
			continue
		}
		if value, ok := strings.CutPrefix(f, tagSourceUID); ok {
			uid = value
		}
	}

	if !hasManagedBy || uid == "" {
		return "", false
	}
	return types.UID(uid), true
}
