package ownership

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	accessSourceUIDChunkPrefix = "source-uid-"
	accessTagMaxLength         = 35
)

func renderAccessTags(uid types.UID) []string {
	return append([]string{tagManagedBy}, sourceUIDTags(uid)...)
}

func parseAccessTags(tags []string) (types.UID, bool) {
	hasManagedBy := false
	chunks := map[int]string{}
	for _, tag := range tags {
		if tag == tagManagedBy {
			hasManagedBy = true
			continue
		}
		if rest, ok := strings.CutPrefix(tag, accessSourceUIDChunkPrefix); ok {
			idxText, value, ok := strings.Cut(rest, "=")
			if !ok || value == "" {
				continue
			}
			idx, err := strconv.Atoi(idxText)
			if err != nil || idx < 0 {
				continue
			}
			chunks[idx] = value
		}
	}
	if !hasManagedBy || len(chunks) == 0 {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(chunks); i++ {
		chunk, ok := chunks[i]
		if !ok {
			return "", false
		}
		b.WriteString(chunk)
	}
	return types.UID(b.String()), true
}

func sourceUIDTags(uid types.UID) []string {
	value := string(uid)
	var tags []string
	for idx := 0; value != ""; idx++ {
		prefix := fmt.Sprintf("%s%d=", accessSourceUIDChunkPrefix, idx)
		chunkSize := accessTagMaxLength - len(prefix)
		if chunkSize <= 0 {
			panic("access source UID tag prefix exceeds Cloudflare tag length limit")
		}
		if chunkSize > len(value) {
			chunkSize = len(value)
		}
		tags = append(tags, prefix+value[:chunkSize])
		value = value[chunkSize:]
	}
	return tags
}
