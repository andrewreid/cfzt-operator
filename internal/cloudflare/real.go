package cloudflare

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	cfdns "github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
	cfzones "github.com/cloudflare/cloudflare-go/v4/zones"
	"golang.org/x/time/rate"
)

const (
	defaultRateLimit = 4.0 // requests/second per API token
	defaultBurst     = 8
	maxRetries       = 5
)

var limiterByToken sync.Map

// RealClient wraps the cloudflare-go/v4 SDK behind the Client interface.
// All SDK imports are confined to this file; controllers never see cloudflare-go.
type RealClient struct {
	api       *cf.Client
	accountID string
	limiter   *rate.Limiter
}

// New constructs a RealClient authenticated with apiToken for accountID.
func New(accountID, apiToken string) (*RealClient, error) {
	if accountID == "" {
		return nil, errors.New("cloudflare: accountID required")
	}
	if apiToken == "" {
		return nil, errors.New("cloudflare: apiToken required")
	}
	api := cf.NewClient(option.WithAPIToken(apiToken))
	return &RealClient{
		api:       api,
		accountID: accountID,
		limiter:   limiterForToken(apiToken),
	}, nil
}

func limiterForToken(apiToken string) *rate.Limiter {
	sum := sha256.Sum256([]byte(apiToken))
	key := fmt.Sprintf("%x", sum)
	actual, _ := limiterByToken.LoadOrStore(key, rate.NewLimiter(defaultRateLimit, defaultBurst))
	return actual.(*rate.Limiter)
}

func (c *RealClient) Tunnels() Tunnels {
	return &realTunnels{client: c}
}

func (c *RealClient) Configurations() Configurations {
	return &realConfigurations{client: c}
}

func (c *RealClient) AccessApplications() AccessApplications {
	return &realAccessApplications{client: c}
}

func (c *RealClient) AccessPolicies() AccessPolicies {
	return &realAccessPolicies{client: c}
}

func (c *RealClient) DNSRecords() DNSRecords {
	return &realDNSRecords{client: c}
}

func (c *RealClient) Zones() Zones {
	return &realZones{client: c}
}

// withRetry waits for the rate limiter then calls fn, retrying on 429 / 5xx
// with exponential backoff + jitter up to maxRetries attempts.
func (c *RealClient) withRetry(ctx context.Context, fn func() error) error {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		if attempt == maxRetries {
			return err
		}
		var apiErr *cf.Error
		if !errors.As(err, &apiErr) {
			return err // not an API error; no retry
		}
		code := apiErr.StatusCode
		if code != http.StatusTooManyRequests && (code < 500 || code > 599) {
			return err
		}
		// exponential backoff with jitter: base 500ms * 2^attempt ± 25%
		base := time.Duration(500<<uint(attempt)) * time.Millisecond
		jitter := time.Duration(rand.Int63n(int64(base) / 2))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base + jitter - base/4):
		}
	}
	return nil // unreachable
}

// realTunnels implements the Tunnels sub-interface against the SDK.
type realTunnels struct {
	client *RealClient
}

func (t *realTunnels) Create(ctx context.Context, in CreateTunnelInput) (*Tunnel, error) {
	configSrc := zero_trust.TunnelCloudflaredNewParamsConfigSrc(in.ConfigSrc)
	var result *Tunnel
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.New(ctx,
			zero_trust.TunnelCloudflaredNewParams{
				AccountID: cf.F(t.client.accountID),
				Name:      cf.F(in.Name),
				ConfigSrc: cf.F(configSrc),
			},
		)
		if err != nil {
			return err
		}
		result = &Tunnel{ID: resp.ID, Name: resp.Name}
		return nil
	})
	return result, err
}

func (t *realTunnels) List(ctx context.Context, filter ListTunnelsFilter) ([]Tunnel, error) {
	params := zero_trust.TunnelCloudflaredListParams{
		AccountID: cf.F(t.client.accountID),
	}
	if filter.Name != "" {
		params.Name = cf.F(filter.Name)
	}

	var results []Tunnel
	err := t.client.withRetry(ctx, func() error {
		pager := t.client.api.ZeroTrust.Tunnels.Cloudflared.ListAutoPaging(ctx, params)
		results = results[:0]
		for pager.Next() {
			item := pager.Current()
			results = append(results, Tunnel{
				ID:   item.ID,
				Name: item.Name,
			})
		}
		return pager.Err()
	})
	return results, err
}

func (t *realTunnels) Get(ctx context.Context, id string) (*Tunnel, error) {
	var result *Tunnel
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Get(ctx, id,
			zero_trust.TunnelCloudflaredGetParams{
				AccountID: cf.F(t.client.accountID),
			},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		result = &Tunnel{
			ID:   resp.ID,
			Name: resp.Name,
		}
		return nil
	})
	return result, err
}

func (t *realTunnels) Delete(ctx context.Context, id string) error {
	return t.client.withRetry(ctx, func() error {
		_, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, id,
			zero_trust.TunnelCloudflaredDeleteParams{
				AccountID: cf.F(t.client.accountID),
			},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
		}
		return err
	})
}

func (t *realTunnels) Token(ctx context.Context, id string) (string, error) {
	var tok string
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, id,
			zero_trust.TunnelCloudflaredTokenGetParams{
				AccountID: cf.F(t.client.accountID),
			},
		)
		if err != nil {
			return err
		}
		if resp != nil {
			tok = *resp
		}
		return nil
	})
	return tok, err
}

type realConfigurations struct {
	client *RealClient
}

func (c *realConfigurations) Get(ctx context.Context, tunnelID string) (*TunnelConfiguration, error) {
	var result *TunnelConfiguration
	err := c.client.withRetry(ctx, func() error {
		resp, err := c.client.api.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(ctx, tunnelID,
			zero_trust.TunnelCloudflaredConfigurationGetParams{AccountID: cf.F(c.client.accountID)},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		config := TunnelConfiguration{}
		for _, rule := range resp.Config.Ingress {
			config.Ingress = append(config.Ingress, IngressRule{Hostname: rule.Hostname, Service: rule.Service})
		}
		result = &config
		return nil
	})
	return result, err
}

func (c *realConfigurations) Update(ctx context.Context, tunnelID string, config TunnelConfiguration) error {
	return c.client.withRetry(ctx, func() error {
		ingress := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(config.Ingress))
		for _, rule := range config.Ingress {
			param := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				Service: cf.F(rule.Service),
			}
			if rule.Hostname != "" {
				param.Hostname = cf.F(rule.Hostname)
			}
			ingress = append(ingress, param)
		}
		_, err := c.client.api.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, tunnelID,
			zero_trust.TunnelCloudflaredConfigurationUpdateParams{
				AccountID: cf.F(c.client.accountID),
				Config: cf.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
					Ingress: cf.F(ingress),
				}),
			},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
		}
		return err
	})
}

type realAccessApplications struct {
	client *RealClient
}

func (a *realAccessApplications) List(ctx context.Context, domain string) ([]AccessApplication, error) {
	var out []AccessApplication
	err := a.client.withRetry(ctx, func() error {
		params := zero_trust.AccessApplicationListParams{
			AccountID: cf.F(a.client.accountID),
			Domain:    cf.F(domain),
			Exact:     cf.F(true),
		}
		pager := a.client.api.ZeroTrust.Access.Applications.ListAutoPaging(ctx, params)
		out = out[:0]
		for pager.Next() {
			item := pager.Current()
			out = append(out, AccessApplication{
				ID:         item.ID,
				Name:       item.Name,
				Domain:     item.Domain,
				Tags:       stringSlice(item.Tags),
				PolicyUUID: firstPolicyID(item.Policies),
			})
		}
		return pager.Err()
	})
	return out, err
}

func (a *realAccessApplications) Create(ctx context.Context, in AccessApplicationInput) (*AccessApplication, error) {
	var result *AccessApplication
	err := a.client.withRetry(ctx, func() error {
		resp, err := a.client.api.ZeroTrust.Access.Applications.New(ctx, zero_trust.AccessApplicationNewParams{
			AccountID: cf.F(a.client.accountID),
			Body:      accessNewBody(in),
		})
		if err != nil {
			return err
		}
		result = accessAppFromResponse(resp.ID, resp.Name, resp.Domain, in.PolicyUUID, stringSlice(resp.Tags))
		return nil
	})
	return result, err
}

func (a *realAccessApplications) Update(ctx context.Context, id string, in AccessApplicationInput) (*AccessApplication, error) {
	var result *AccessApplication
	err := a.client.withRetry(ctx, func() error {
		resp, err := a.client.api.ZeroTrust.Access.Applications.Update(ctx, id, zero_trust.AccessApplicationUpdateParams{
			AccountID: cf.F(a.client.accountID),
			Body:      accessUpdateBody(in),
		})
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		result = accessAppFromResponse(resp.ID, resp.Name, resp.Domain, in.PolicyUUID, stringSlice(resp.Tags))
		return nil
	})
	return result, err
}

func (a *realAccessApplications) Delete(ctx context.Context, id string) error {
	return a.client.withRetry(ctx, func() error {
		_, err := a.client.api.ZeroTrust.Access.Applications.Delete(ctx, id,
			zero_trust.AccessApplicationDeleteParams{AccountID: cf.F(a.client.accountID)},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
		}
		return err
	})
}

type realDNSRecords struct {
	client *RealClient
}

func (d *realDNSRecords) List(ctx context.Context, zoneID, name, recordType string) ([]DNSRecord, error) {
	var out []DNSRecord
	err := d.client.withRetry(ctx, func() error {
		params := cfdns.RecordListParams{
			ZoneID: cf.F(zoneID),
			Name:   cf.F(cfdns.RecordListParamsName{Exact: cf.F(name)}),
			Type:   cf.F(cfdns.RecordListParamsType(recordType)),
		}
		pager := d.client.api.DNS.Records.ListAutoPaging(ctx, params)
		out = out[:0]
		for pager.Next() {
			item := pager.Current()
			out = append(out, DNSRecord{
				ID:      item.ID,
				ZoneID:  zoneID,
				Name:    item.Name,
				Type:    string(item.Type),
				Content: item.Content,
				Proxied: item.Proxied,
				Comment: item.Comment,
			})
		}
		return pager.Err()
	})
	return out, err
}

func (d *realDNSRecords) Create(ctx context.Context, in DNSRecordInput) (*DNSRecord, error) {
	var result *DNSRecord
	err := d.client.withRetry(ctx, func() error {
		resp, err := d.client.api.DNS.Records.New(ctx, cfdns.RecordNewParams{
			ZoneID: cf.F(in.ZoneID),
			Body: cfdns.CNAMERecordParam{
				Name:    cf.F(in.Name),
				TTL:     cf.F(cfdns.TTL1),
				Type:    cf.F(cfdns.CNAMERecordTypeCNAME),
				Content: cf.F(in.Content),
				Proxied: cf.F(in.Proxied),
				Comment: cf.F(in.Comment),
			},
		})
		if err != nil {
			return err
		}
		result = dnsRecordFromResponse(in.ZoneID, resp)
		return nil
	})
	return result, err
}

func (d *realDNSRecords) Update(ctx context.Context, id string, in DNSRecordInput) (*DNSRecord, error) {
	var result *DNSRecord
	err := d.client.withRetry(ctx, func() error {
		resp, err := d.client.api.DNS.Records.Update(ctx, id, cfdns.RecordUpdateParams{
			ZoneID: cf.F(in.ZoneID),
			Body: cfdns.CNAMERecordParam{
				Name:    cf.F(in.Name),
				TTL:     cf.F(cfdns.TTL1),
				Type:    cf.F(cfdns.CNAMERecordTypeCNAME),
				Content: cf.F(in.Content),
				Proxied: cf.F(in.Proxied),
				Comment: cf.F(in.Comment),
			},
		})
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		result = dnsRecordFromResponse(in.ZoneID, resp)
		return nil
	})
	return result, err
}

func (d *realDNSRecords) Delete(ctx context.Context, zoneID, id string) error {
	return d.client.withRetry(ctx, func() error {
		_, err := d.client.api.DNS.Records.Delete(ctx, id, cfdns.RecordDeleteParams{ZoneID: cf.F(zoneID)})
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
		}
		return err
	})
}

type realZones struct {
	client *RealClient
}

func (z *realZones) List(ctx context.Context) ([]Zone, error) {
	var out []Zone
	err := z.client.withRetry(ctx, func() error {
		pager := z.client.api.Zones.ListAutoPaging(ctx, cfzones.ZoneListParams{})
		out = out[:0]
		for pager.Next() {
			item := pager.Current()
			out = append(out, Zone{ID: item.ID, Name: item.Name})
		}
		return pager.Err()
	})
	return out, err
}

func (z *realZones) Resolve(ctx context.Context, hostname string) (*Zone, error) {
	zones, err := z.List(ctx)
	if err != nil {
		return nil, err
	}
	zone, ok := LongestMatchingZone(zones, hostname)
	if !ok {
		return nil, ErrNotFound
	}
	return zone, nil
}

func accessNewBody(in AccessApplicationInput) zero_trust.AccessApplicationNewParamsBodyUnion {
	return zero_trust.AccessApplicationNewParamsBodySelfHostedApplication{
		Domain: cf.F(in.Domain),
		Type:   cf.F(zero_trust.ApplicationTypeSelfHosted),
		Name:   cf.F(in.Name),
		Policies: cf.F([]zero_trust.AccessApplicationNewParamsBodySelfHostedApplicationPolicyUnion{
			zero_trust.AccessApplicationNewParamsBodySelfHostedApplicationPoliciesAccessAppPolicyLink{
				ID:         cf.F(in.PolicyUUID),
				Precedence: cf.F(int64(0)),
			},
		}),
		SelfHostedDomains: cf.F([]zero_trust.SelfHostedDomainsParam{in.Domain}),
		Tags:              cf.F(in.Tags),
	}
}

func accessUpdateBody(in AccessApplicationInput) zero_trust.AccessApplicationUpdateParamsBodyUnion {
	return zero_trust.AccessApplicationUpdateParamsBodySelfHostedApplication{
		Domain: cf.F(in.Domain),
		Type:   cf.F(zero_trust.ApplicationTypeSelfHosted),
		Name:   cf.F(in.Name),
		Policies: cf.F([]zero_trust.AccessApplicationUpdateParamsBodySelfHostedApplicationPolicyUnion{
			zero_trust.AccessApplicationUpdateParamsBodySelfHostedApplicationPoliciesAccessAppPolicyLink{
				ID:         cf.F(in.PolicyUUID),
				Precedence: cf.F(int64(0)),
			},
		}),
		SelfHostedDomains: cf.F([]zero_trust.SelfHostedDomainsParam{in.Domain}),
		Tags:              cf.F(in.Tags),
	}
}

func accessAppFromResponse(id, name, domain, policyUUID string, tags []string) *AccessApplication {
	return &AccessApplication{ID: id, Name: name, Domain: domain, PolicyUUID: policyUUID, Tags: tags}
}

func dnsRecordFromResponse(zoneID string, resp *cfdns.RecordResponse) *DNSRecord {
	return &DNSRecord{
		ID:      resp.ID,
		ZoneID:  zoneID,
		Name:    resp.Name,
		Type:    string(resp.Type),
		Content: resp.Content,
		Proxied: resp.Proxied,
		Comment: resp.Comment,
	}
}

func stringSlice(value any) []string {
	switch v := value.(type) {
	case []string:
		return append([]string(nil), v...)
	default:
		return nil
	}
}

type realAccessPolicies struct {
	client *RealClient
}

func (p *realAccessPolicies) List(ctx context.Context) ([]AccessPolicy, error) {
	var out []AccessPolicy
	err := p.client.withRetry(ctx, func() error {
		params := zero_trust.AccessPolicyListParams{AccountID: cf.F(p.client.accountID)}
		pager := p.client.api.ZeroTrust.Access.Policies.ListAutoPaging(ctx, params)
		out = out[:0]
		for pager.Next() {
			item := pager.Current()
			out = append(out, AccessPolicy{
				ID:                           item.ID,
				Name:                         item.Name,
				Decision:                     string(item.Decision),
				Include:                      fromAccessRules(item.Include),
				Exclude:                      fromAccessRules(item.Exclude),
				Require:                      fromAccessRules(item.Require),
				SessionDuration:              item.SessionDuration,
				PurposeJustificationRequired: item.PurposeJustificationRequired,
				PurposeJustificationPrompt:   item.PurposeJustificationPrompt,
			})
		}
		return pager.Err()
	})
	return out, err
}

func (p *realAccessPolicies) Get(ctx context.Context, id string) (*AccessPolicy, error) {
	var result *AccessPolicy
	err := p.client.withRetry(ctx, func() error {
		resp, err := p.client.api.ZeroTrust.Access.Policies.Get(ctx, id,
			zero_trust.AccessPolicyGetParams{AccountID: cf.F(p.client.accountID)},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			Include:                      fromAccessRules(resp.Include),
			Exclude:                      fromAccessRules(resp.Exclude),
			Require:                      fromAccessRules(resp.Require),
			SessionDuration:              resp.SessionDuration,
			PurposeJustificationRequired: resp.PurposeJustificationRequired,
			PurposeJustificationPrompt:   resp.PurposeJustificationPrompt,
		}
		return nil
	})
	return result, err
}

func (p *realAccessPolicies) Create(ctx context.Context, in AccessPolicyInput) (*AccessPolicy, error) {
	decision, err := toDecision(in.Decision)
	if err != nil {
		return nil, err
	}
	var result *AccessPolicy
	cerr := p.client.withRetry(ctx, func() error {
		resp, err := p.client.api.ZeroTrust.Access.Policies.New(ctx, zero_trust.AccessPolicyNewParams{
			AccountID:                    cf.F(p.client.accountID),
			Name:                         cf.F(in.Name),
			Decision:                     cf.F(decision),
			Include:                      cf.F(toAccessRuleParams(in.Include)),
			Exclude:                      cf.F(toAccessRuleParams(in.Exclude)),
			Require:                      cf.F(toAccessRuleParams(in.Require)),
			SessionDuration:              cf.F(in.SessionDuration),
			PurposeJustificationRequired: cf.F(in.PurposeJustificationRequired),
			PurposeJustificationPrompt:   cf.F(in.PurposeJustificationPrompt),
		})
		if err != nil {
			return err
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			Include:                      fromAccessRules(resp.Include),
			Exclude:                      fromAccessRules(resp.Exclude),
			Require:                      fromAccessRules(resp.Require),
			SessionDuration:              resp.SessionDuration,
			PurposeJustificationRequired: resp.PurposeJustificationRequired,
			PurposeJustificationPrompt:   resp.PurposeJustificationPrompt,
		}
		return nil
	})
	return result, cerr
}

func (p *realAccessPolicies) Update(ctx context.Context, id string, in AccessPolicyInput) (*AccessPolicy, error) {
	decision, err := toDecision(in.Decision)
	if err != nil {
		return nil, err
	}
	var result *AccessPolicy
	cerr := p.client.withRetry(ctx, func() error {
		resp, err := p.client.api.ZeroTrust.Access.Policies.Update(ctx, id, zero_trust.AccessPolicyUpdateParams{
			AccountID:                    cf.F(p.client.accountID),
			Name:                         cf.F(in.Name),
			Decision:                     cf.F(decision),
			Include:                      cf.F(toAccessRuleParams(in.Include)),
			Exclude:                      cf.F(toAccessRuleParams(in.Exclude)),
			Require:                      cf.F(toAccessRuleParams(in.Require)),
			SessionDuration:              cf.F(in.SessionDuration),
			PurposeJustificationRequired: cf.F(in.PurposeJustificationRequired),
			PurposeJustificationPrompt:   cf.F(in.PurposeJustificationPrompt),
		})
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			Include:                      fromAccessRules(resp.Include),
			Exclude:                      fromAccessRules(resp.Exclude),
			Require:                      fromAccessRules(resp.Require),
			SessionDuration:              resp.SessionDuration,
			PurposeJustificationRequired: resp.PurposeJustificationRequired,
			PurposeJustificationPrompt:   resp.PurposeJustificationPrompt,
		}
		return nil
	})
	return result, cerr
}

func (p *realAccessPolicies) Delete(ctx context.Context, id string) error {
	return p.client.withRetry(ctx, func() error {
		_, err := p.client.api.ZeroTrust.Access.Policies.Delete(ctx, id,
			zero_trust.AccessPolicyDeleteParams{AccountID: cf.F(p.client.accountID)},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
		}
		return err
	})
}

// toDecision validates and casts a string Decision to the SDK type.
// Decision strings beyond the four MVP values are rejected at this boundary so
// invalid input never reaches Cloudflare.
func toDecision(d string) (zero_trust.Decision, error) {
	switch zero_trust.Decision(d) {
	case zero_trust.DecisionAllow, zero_trust.DecisionDeny,
		zero_trust.DecisionBypass, zero_trust.DecisionNonIdentity:
		return zero_trust.Decision(d), nil
	}
	return "", fmt.Errorf("cloudflare: invalid decision %q", d)
}

// toAccessRuleParams maps the package-local AccessRule slice to the SDK union
// param slice. The first non-zero field on each item picks the variant; order
// matches the discriminator order in api/v1alpha1.AccessRule.
func toAccessRuleParams(rules []AccessRule) []zero_trust.AccessRuleUnionParam {
	if len(rules) == 0 {
		return nil
	}
	out := make([]zero_trust.AccessRuleUnionParam, 0, len(rules))
	for _, r := range rules {
		switch {
		case r.Email != "":
			out = append(out, zero_trust.EmailRuleParam{
				Email: cf.F(zero_trust.EmailRuleEmailParam{Email: cf.F(r.Email)}),
			})
		case r.EmailDomain != "":
			out = append(out, zero_trust.DomainRuleParam{
				EmailDomain: cf.F(zero_trust.DomainRuleEmailDomainParam{Domain: cf.F(r.EmailDomain)}),
			})
		case r.IP != "":
			out = append(out, zero_trust.IPRuleParam{
				IP: cf.F(zero_trust.IPRuleIPParam{IP: cf.F(r.IP)}),
			})
		case r.Everyone:
			out = append(out, zero_trust.EveryoneRuleParam{
				Everyone: cf.F(zero_trust.EveryoneRuleEveryoneParam{}),
			})
		case r.ServiceToken != "":
			out = append(out, zero_trust.ServiceTokenRuleParam{
				ServiceToken: cf.F(zero_trust.ServiceTokenRuleServiceTokenParam{TokenID: cf.F(r.ServiceToken)}),
			})
		case r.GeoCountryCode != "":
			out = append(out, zero_trust.CountryRuleParam{
				Geo: cf.F(zero_trust.CountryRuleGeoParam{CountryCode: cf.F(r.GeoCountryCode)}),
			})
		}
	}
	return out
}

// fromAccessRules maps an SDK response AccessRule slice back into the local
// shape using the SDK's union accessor. Unknown variants (e.g. group, okta,
// saml — beyond MVP) are skipped so they cannot crash the controller.
func fromAccessRules(rules []zero_trust.AccessRule) []AccessRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]AccessRule, 0, len(rules))
	for _, r := range rules {
		switch u := r.AsUnion().(type) {
		case zero_trust.EmailRule:
			out = append(out, AccessRule{Email: u.Email.Email})
		case zero_trust.DomainRule:
			out = append(out, AccessRule{EmailDomain: u.EmailDomain.Domain})
		case zero_trust.IPRule:
			out = append(out, AccessRule{IP: u.IP.IP})
		case zero_trust.EveryoneRule:
			out = append(out, AccessRule{Everyone: true})
		case zero_trust.ServiceTokenRule:
			out = append(out, AccessRule{ServiceToken: u.ServiceToken.TokenID})
		case zero_trust.CountryRule:
			out = append(out, AccessRule{GeoCountryCode: u.Geo.CountryCode})
		}
	}
	return out
}

func firstPolicyID(value any) string {
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice || rv.Len() == 0 {
		return ""
	}
	first := rv.Index(0)
	if first.Kind() == reflect.Pointer {
		first = first.Elem()
	}
	if first.Kind() != reflect.Struct {
		return ""
	}
	field := first.FieldByName("ID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return ""
	}
	return field.String()
}
