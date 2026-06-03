package cloudflare

import (
	"context"
	"strings"
)

type AccessApplication struct {
	ID          string
	Name        string
	Domain      string
	Domains     []string
	PolicyUUIDs []string
	Tags        []string
}

type AccessApplicationInput struct {
	Name        string
	Domains     []string
	PolicyUUIDs []string
	Tags        []string

	// Legacy single-host fields kept so existing callers in other slices keep
	// compiling until they are moved over to the multi-domain input.
	Domain     string
	PolicyUUID string
}

type AccessApplications interface {
	List(ctx context.Context, domain string) ([]AccessApplication, error)
	Create(ctx context.Context, in AccessApplicationInput) (*AccessApplication, error)
	Update(ctx context.Context, id string, in AccessApplicationInput) (*AccessApplication, error)
	Delete(ctx context.Context, id string) error
}

func accessApplicationMatchesHostname(app AccessApplication, hostname string) bool {
	if hostname == "" {
		return true
	}
	if accessApplicationDomainMatchesHostname(app.Domain, hostname) {
		return true
	}
	for _, domain := range app.Domains {
		if accessApplicationDomainMatchesHostname(domain, hostname) {
			return true
		}
	}
	return false
}

func accessApplicationDomainMatchesHostname(domain, hostname string) bool {
	return domain == hostname || strings.HasPrefix(domain, hostname+"/")
}

func accessApplicationInputDomains(in AccessApplicationInput) []string {
	if len(in.Domains) > 0 {
		return append([]string(nil), in.Domains...)
	}
	if in.Domain != "" {
		return []string{in.Domain}
	}
	return nil
}

func accessApplicationInputPolicyUUIDs(in AccessApplicationInput) []string {
	if len(in.PolicyUUIDs) > 0 {
		return append([]string(nil), in.PolicyUUIDs...)
	}
	if in.PolicyUUID != "" {
		return []string{in.PolicyUUID}
	}
	return nil
}

func accessApplicationPrimaryDomain(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}
