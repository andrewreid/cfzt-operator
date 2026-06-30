package tunnelconfig

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

const CatchAllService = "http_status:404"

type Route struct {
	ExposureUID types.UID
	Namespace   string
	Name        string
	Hostname    string
	Hash        string
}

type Result struct {
	Config cloudflare.TunnelConfiguration
	Routes []Route
}

type HostnameConflictError struct {
	Hostnames []string
}

func (e *HostnameConflictError) Error() string {
	return fmt.Sprintf("hostname conflict: %v", e.Hostnames)
}

func Build(exposures []cfztv1alpha1.CloudflareExposure) (*Result, error) {
	byHost := make(map[string][]cfztv1alpha1.CloudflareExposure)
	for _, exposure := range exposures {
		if exposure.Spec.Hostname == "" {
			continue
		}
		byHost[exposure.Spec.Hostname] = append(byHost[exposure.Spec.Hostname], exposure)
	}
	var conflicts []string
	for hostname, matches := range byHost {
		if len(matches) > 1 {
			conflicts = append(conflicts, hostname)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, &HostnameConflictError{Hostnames: conflicts}
	}

	// cloudflared ingress is first-match top-down, so rules must be emitted
	// most-specific-first. A lexicographic sort is wrong: '*' (0x2A) sorts before
	// letters, so '*.example.com' would shadow 'foo.example.com'. Order by, in
	// turn: (a) more labels first; (b) for equal label count, concrete before
	// wildcard; (c) the existing deterministic tie-breakers hostname, namespace,
	// name. The catch-all http_status:404 is appended last, unconditionally.
	ordered := append([]cfztv1alpha1.CloudflareExposure(nil), exposures...)
	sort.Slice(ordered, func(i, j int) bool {
		hi, hj := ordered[i].Spec.Hostname, ordered[j].Spec.Hostname
		if li, lj := labelCount(hi), labelCount(hj); li != lj {
			return li > lj
		}
		if wi, wj := isWildcardHostname(hi), isWildcardHostname(hj); wi != wj {
			return !wi
		}
		if hi != hj {
			return hi < hj
		}
		if ordered[i].Namespace != ordered[j].Namespace {
			return ordered[i].Namespace < ordered[j].Namespace
		}
		return ordered[i].Name < ordered[j].Name
	})

	result := &Result{}
	for _, exposure := range ordered {
		if exposure.Spec.Hostname == "" || exposure.Spec.Origin == nil {
			continue
		}
		service := fmt.Sprintf("%s://%s:%d", exposure.Spec.Origin.Protocol, exposure.Spec.Origin.Host, exposure.Spec.Origin.Port)
		rule := cloudflare.IngressRule{Hostname: exposure.Spec.Hostname, Service: service}
		result.Config.Ingress = append(result.Config.Ingress, rule)
		hash := HashRule(rule)
		result.Routes = append(result.Routes, Route{
			ExposureUID: exposure.UID,
			Namespace:   exposure.Namespace,
			Name:        exposure.Name,
			Hostname:    exposure.Spec.Hostname,
			Hash:        hash,
		})
	}
	result.Config.Ingress = append(result.Config.Ingress, cloudflare.IngressRule{Service: CatchAllService})
	return result, nil
}

// isWildcardHostname reports whether hostname carries a single leading wildcard
// label ('*.'), matching the CRD validation grammar.
func isWildcardHostname(hostname string) bool {
	return strings.HasPrefix(hostname, "*.")
}

// labelCount returns the number of dot-separated labels in hostname, counting a
// leading wildcard label. An empty hostname has zero labels.
func labelCount(hostname string) int {
	if hostname == "" {
		return 0
	}
	return strings.Count(hostname, ".") + 1
}

func HashRule(rule cloudflare.IngressRule) string {
	canonical := struct {
		Hostname string `json:"hostname,omitempty"`
		Service  string `json:"service"`
	}{
		Hostname: rule.Hostname,
		Service:  rule.Service,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		panic(fmt.Sprintf("marshal ingress rule hash input: %v", err))
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}
