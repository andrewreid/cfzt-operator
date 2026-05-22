package ownership

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

func renderComment(uid types.UID) string {
	return fmt.Sprintf("%s %s%s", tagManagedBy, tagSourceUID, uid)
}

func renderCompactComment(uid types.UID) string {
	return fmt.Sprintf("%s %s%s", tagManagedByCompact, tagSourceUID, uid)
}

func parseComment(s string) (types.UID, bool) {
	prefix, _, _ := strings.Cut(s, " | ")
	fields := strings.Fields(prefix)

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
