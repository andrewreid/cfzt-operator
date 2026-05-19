package cloudflare

import (
	"context"
	"strings"
)

type Zone struct {
	ID   string
	Name string
}

type Zones interface {
	List(ctx context.Context) ([]Zone, error)
	Resolve(ctx context.Context, hostname string) (*Zone, error)
}

func LongestMatchingZone(zones []Zone, hostname string) (*Zone, bool) {
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	var best *Zone
	for i := range zones {
		name := strings.TrimSuffix(strings.ToLower(zones[i].Name), ".")
		if hostname != name && !strings.HasSuffix(hostname, "."+name) {
			continue
		}
		if best == nil || len(name) > len(best.Name) {
			copy := zones[i]
			best = &copy
		}
	}
	return best, best != nil
}
