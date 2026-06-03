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
	return append([]string(nil), in.Domains...)
}

func accessApplicationInputPolicyUUIDs(in AccessApplicationInput) []string {
	return append([]string(nil), in.PolicyUUIDs...)
}

func accessApplicationPrimaryDomain(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}
