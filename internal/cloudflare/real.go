package cloudflare

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
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

var (
	limiterByToken    sync.Map
	zoneCacheByCred   sync.Map
	clientCacheByCred sync.Map
)

type cacheKey struct {
	accountID string
	tokenHash [32]byte
}

type zoneCache struct {
	mu    sync.Mutex
	zones []Zone
	ready bool
}

// RealClient wraps the cloudflare-go/v4 SDK behind the Client interface.
// All SDK imports are confined to this file; controllers never see cloudflare-go.
type RealClient struct {
	api       *cf.Client
	accountID string
	limiter   *rate.Limiter
	cacheKey  cacheKey
}

// New constructs a RealClient authenticated with apiToken for accountID.
func New(accountID, apiToken string) (*RealClient, error) {
	if accountID == "" {
		return nil, errors.New("cloudflare: accountID required")
	}
	if apiToken == "" {
		return nil, errors.New("cloudflare: apiToken required")
	}
	key := newCacheKey(accountID, apiToken)
	if cached, ok := clientCacheByCred.Load(key); ok {
		return cached.(*RealClient), nil
	}
	api := cf.NewClient(option.WithAPIToken(apiToken))
	client := &RealClient{
		api:       api,
		accountID: accountID,
		limiter:   limiterForTokenHash(key.tokenHash),
		cacheKey:  key,
	}
	actual, _ := clientCacheByCred.LoadOrStore(key, client)
	return actual.(*RealClient), nil
}

func newCacheKey(accountID, apiToken string) cacheKey {
	return cacheKey{accountID: accountID, tokenHash: sha256.Sum256([]byte(apiToken))}
}

func limiterForToken(apiToken string) *rate.Limiter {
	return limiterForTokenHash(sha256.Sum256([]byte(apiToken)))
}

func limiterForTokenHash(tokenHash [32]byte) *rate.Limiter {
	key := fmt.Sprintf("%x", tokenHash)
	actual, _ := limiterByToken.LoadOrStore(key, rate.NewLimiter(defaultRateLimit, defaultBurst))
	return actual.(*rate.Limiter)
}

func zoneCacheForCred(key cacheKey) *zoneCache {
	actual, _ := zoneCacheByCred.LoadOrStore(key, &zoneCache{})
	return actual.(*zoneCache)
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

func (c *RealClient) AccessTags() AccessTags {
	return &realAccessTags{client: c}
}

func (c *RealClient) AccessPolicies() AccessPolicies {
	return &realAccessPolicies{client: c}
}

func (c *RealClient) TunnelRoutes() TunnelRoutes {
	return &realTunnelRoutes{client: c}
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

func mapAPIError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *cf.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return err
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

func (t *realTunnels) Rename(ctx context.Context, id string, in RenameTunnelInput) (*Tunnel, error) {
	var result *Tunnel
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Edit(ctx, id,
			zero_trust.TunnelCloudflaredEditParams{
				AccountID: cf.F(t.client.accountID),
				Name:      cf.F(in.Name),
			},
		)
		if err != nil {
			return mapAPIError(err)
		}
		result = &Tunnel{ID: resp.ID, Name: resp.Name}
		return nil
	})
	return result, err
}

func (t *realTunnels) List(ctx context.Context, name string) ([]Tunnel, error) {
	params := zero_trust.TunnelCloudflaredListParams{
		AccountID: cf.F(t.client.accountID),
	}
	if name != "" {
		params.Name = cf.F(name)
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
			return mapAPIError(err)
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
			return mapAPIError(err)
		}
		return nil
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
			return mapAPIError(err)
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

type realTunnelRoutes struct {
	client *RealClient
}

func (r *realTunnelRoutes) List(ctx context.Context, filter ListTunnelRoutesFilter) ([]TunnelRoute, error) {
	params := zero_trust.NetworkRouteListParams{
		AccountID: cf.F(r.client.accountID),
		IsDeleted: cf.F(false),
		TunTypes: cf.F([]zero_trust.NetworkRouteListParamsTunType{
			zero_trust.NetworkRouteListParamsTunTypeCfdTunnel,
		}),
	}
	if filter.Network != "" {
		// The SDK exposes subset/superset CIDR filters, not exact matching.
		// This wrapper asks for both candidate sets and enforces exact Network
		// equality below so callers get one stable filter semantic.
		params.NetworkSubset = cf.F(filter.Network)
		params.NetworkSuperset = cf.F(filter.Network)
	}
	if filter.TunnelID != "" {
		params.TunnelID = cf.F(filter.TunnelID)
	}
	if filter.VirtualNetworkID != "" {
		params.VirtualNetworkID = cf.F(filter.VirtualNetworkID)
	}

	var results []TunnelRoute
	err := r.client.withRetry(ctx, func() error {
		pager := r.client.api.ZeroTrust.Networks.Routes.ListAutoPaging(ctx, params)
		results = results[:0]
		for pager.Next() {
			item := pager.Current()
			if filter.Network != "" && item.Network != filter.Network {
				continue
			}
			results = append(results, TunnelRoute{
				ID:               item.ID,
				Network:          item.Network,
				TunnelID:         item.TunnelID,
				VirtualNetworkID: item.VirtualNetworkID,
				Comment:          item.Comment,
			})
		}
		return pager.Err()
	})
	return results, err
}

func (r *realTunnelRoutes) Create(ctx context.Context, in TunnelRouteInput) (*TunnelRoute, error) {
	params := zero_trust.NetworkRouteNewParams{
		AccountID: cf.F(r.client.accountID),
		Network:   cf.F(in.Network),
		TunnelID:  cf.F(in.TunnelID),
		Comment:   cf.F(in.Comment),
	}
	if in.VirtualNetworkID != "" {
		params.VirtualNetworkID = cf.F(in.VirtualNetworkID)
	}
	var result *TunnelRoute
	err := r.client.withRetry(ctx, func() error {
		resp, err := r.client.api.ZeroTrust.Networks.Routes.New(ctx, params)
		if err != nil {
			return err
		}
		result = routeFromSDK(resp)
		return nil
	})
	return result, err
}

func (r *realTunnelRoutes) Get(ctx context.Context, id string) (*TunnelRoute, error) {
	var result *TunnelRoute
	err := r.client.withRetry(ctx, func() error {
		resp, err := r.client.api.ZeroTrust.Networks.Routes.Get(ctx, id, zero_trust.NetworkRouteGetParams{
			AccountID: cf.F(r.client.accountID),
		})
		if err != nil {
			return mapAPIError(err)
		}
		result = routeFromSDK(resp)
		return nil
	})
	return result, err
}

func (r *realTunnelRoutes) Edit(ctx context.Context, id string, in TunnelRouteInput) (*TunnelRoute, error) {
	params := zero_trust.NetworkRouteEditParams{
		AccountID: cf.F(r.client.accountID),
		Network:   cf.F(in.Network),
		TunnelID:  cf.F(in.TunnelID),
		Comment:   cf.F(in.Comment),
	}
	if in.VirtualNetworkID != "" {
		params.VirtualNetworkID = cf.F(in.VirtualNetworkID)
	}
	var result *TunnelRoute
	err := r.client.withRetry(ctx, func() error {
		resp, err := r.client.api.ZeroTrust.Networks.Routes.Edit(ctx, id, params)
		if err != nil {
			return mapAPIError(err)
		}
		result = routeFromSDK(resp)
		return nil
	})
	return result, err
}

func (r *realTunnelRoutes) Delete(ctx context.Context, id string) error {
	return r.client.withRetry(ctx, func() error {
		_, err := r.client.api.ZeroTrust.Networks.Routes.Delete(ctx, id, zero_trust.NetworkRouteDeleteParams{
			AccountID: cf.F(r.client.accountID),
		})
		if err != nil {
			return mapAPIError(err)
		}
		return nil
	})
}

func routeFromSDK(route *zero_trust.Route) *TunnelRoute {
	if route == nil {
		return nil
	}
	return &TunnelRoute{
		ID:               route.ID,
		Network:          route.Network,
		TunnelID:         route.TunnelID,
		VirtualNetworkID: route.VirtualNetworkID,
		Comment:          route.Comment,
	}
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
			return mapAPIError(err)
		}
		return nil
	})
}

type realAccessApplications struct {
	client *RealClient
}

type realAccessTags struct {
	client *RealClient
}

func (t *realAccessTags) Ensure(ctx context.Context, name string) error {
	err := t.client.withRetry(ctx, func() error {
		_, err := t.client.api.ZeroTrust.Access.Tags.Get(ctx, name, zero_trust.AccessTagGetParams{
			AccountID: cf.F(t.client.accountID),
		})
		if err != nil {
			return mapAPIError(err)
		}
		return nil
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return t.client.withRetry(ctx, func() error {
		_, err := t.client.api.ZeroTrust.Access.Tags.New(ctx, zero_trust.AccessTagNewParams{
			AccountID: cf.F(t.client.accountID),
			Name:      cf.F(name),
		})
		return err
	})
}

func (t *realAccessTags) Delete(ctx context.Context, name string) error {
	return t.client.withRetry(ctx, func() error {
		_, err := t.client.api.ZeroTrust.Access.Tags.Delete(ctx, name, zero_trust.AccessTagDeleteParams{
			AccountID: cf.F(t.client.accountID),
		})
		if err != nil {
			return mapAPIError(err)
		}
		return nil
	})
}

func (a *realAccessApplications) List(ctx context.Context, domain string) ([]AccessApplication, error) {
	var out []AccessApplication
	err := a.client.withRetry(ctx, func() error {
		params := accessApplicationListParams(a.client.accountID, domain)
		pager := a.client.api.ZeroTrust.Access.Applications.ListAutoPaging(ctx, params)
		out = out[:0]
		for pager.Next() {
			item := pager.Current()
			app := accessAppFromListResponse(item)
			if accessApplicationMatchesHostname(app, domain) {
				out = append(out, app)
			}
		}
		return pager.Err()
	})
	return out, err
}

func accessApplicationListParams(accountID, domain string) zero_trust.AccessApplicationListParams {
	params := zero_trust.AccessApplicationListParams{AccountID: cf.F(accountID)}
	if domain != "" {
		params.Domain = cf.F(domain)
	}
	return params
}

func (a *realAccessApplications) Create(ctx context.Context, in AccessApplicationInput) (*AccessApplication, error) {
	if err := a.ensureTags(ctx, in.Tags); err != nil {
		return nil, err
	}
	var result *AccessApplication
	err := a.client.withRetry(ctx, func() error {
		resp, err := a.client.api.ZeroTrust.Access.Applications.New(ctx, zero_trust.AccessApplicationNewParams{
			AccountID: cf.F(a.client.accountID),
			Body:      accessNewBody(in),
		})
		if err != nil {
			return err
		}
		result = accessAppFromNewResponse(resp)
		return nil
	})
	return result, err
}

func (a *realAccessApplications) Update(ctx context.Context, id string, in AccessApplicationInput) (*AccessApplication, error) {
	if err := a.ensureTags(ctx, in.Tags); err != nil {
		return nil, err
	}
	var result *AccessApplication
	err := a.client.withRetry(ctx, func() error {
		resp, err := a.client.api.ZeroTrust.Access.Applications.Update(ctx, id, zero_trust.AccessApplicationUpdateParams{
			AccountID: cf.F(a.client.accountID),
			Body:      accessUpdateBody(in),
		})
		if err != nil {
			return mapAPIError(err)
		}
		result = accessAppFromUpdateResponse(resp)
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
			return mapAPIError(err)
		}
		return nil
	})
}

func (a *realAccessApplications) ensureTags(ctx context.Context, tags []string) error {
	for _, tag := range tags {
		if err := a.client.AccessTags().Ensure(ctx, tag); err != nil {
			return err
		}
	}
	return nil
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
		body, err := dnsRecordBody(in)
		if err != nil {
			return err
		}
		resp, err := d.client.api.DNS.Records.New(ctx, cfdns.RecordNewParams{
			ZoneID: cf.F(in.ZoneID),
			Body:   body.(cfdns.RecordNewParamsBodyUnion),
		})
		if err != nil {
			return mapAPIError(err)
		}
		result = dnsRecordFromResponse(in.ZoneID, resp)
		return nil
	})
	return result, err
}

func (d *realDNSRecords) Update(ctx context.Context, id string, in DNSRecordInput) (*DNSRecord, error) {
	var result *DNSRecord
	err := d.client.withRetry(ctx, func() error {
		body, err := dnsRecordBody(in)
		if err != nil {
			return err
		}
		resp, err := d.client.api.DNS.Records.Update(ctx, id, cfdns.RecordUpdateParams{
			ZoneID: cf.F(in.ZoneID),
			Body:   body.(cfdns.RecordUpdateParamsBodyUnion),
		})
		if err != nil {
			return mapAPIError(err)
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
			return mapAPIError(err)
		}
		return nil
	})
}

// CNAMERecordParam and TXTRecordParam from cfdns satisfy both the New and
// Update body unions, so dnsRecordBody returns one concrete value and each
// call site casts to the union it needs. Compile-time asserts make the dual
// conformance load-bearing — if a future SDK version splits the param shape
// per verb, the build fails here instead of at a runtime cast.
var (
	_ cfdns.RecordNewParamsBodyUnion    = cfdns.CNAMERecordParam{}
	_ cfdns.RecordUpdateParamsBodyUnion = cfdns.CNAMERecordParam{}
	_ cfdns.RecordNewParamsBodyUnion    = cfdns.TXTRecordParam{}
	_ cfdns.RecordUpdateParamsBodyUnion = cfdns.TXTRecordParam{}
)

// dnsRecordBody dispatches on in.Type so failover lease writes (TXT) and the
// public-hostname CNAME path share one write boundary. Unknown types are a
// programmer error and reach this code only on a controller bug.
func dnsRecordBody(in DNSRecordInput) (any, error) {
	switch in.Type {
	case "CNAME":
		return cfdns.CNAMERecordParam{
			Name:    cf.F(in.Name),
			TTL:     cf.F(cfdns.TTL1),
			Type:    cf.F(cfdns.CNAMERecordTypeCNAME),
			Content: cf.F(in.Content),
			Proxied: cf.F(in.Proxied),
			Comment: cf.F(in.Comment),
		}, nil
	case "TXT":
		return cfdns.TXTRecordParam{
			Name:    cf.F(in.Name),
			TTL:     cf.F(cfdns.TTL1),
			Type:    cf.F(cfdns.TXTRecordTypeTXT),
			Content: cf.F(in.Content),
			Proxied: cf.F(in.Proxied),
			Comment: cf.F(in.Comment),
		}, nil
	default:
		return nil, fmt.Errorf("cloudflare: unsupported DNS record type %q for CAS write", in.Type)
	}
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
	cache := zoneCacheForCred(z.client.cacheKey)
	cache.mu.Lock()
	if cache.ready {
		if zone, ok := LongestMatchingZone(cache.zones, hostname); ok {
			cache.mu.Unlock()
			return zone, nil
		}
	}
	cache.mu.Unlock()

	zones, err := z.List(ctx)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	cache.zones = append([]Zone(nil), zones...)
	cache.ready = true
	cache.mu.Unlock()

	zone, ok := LongestMatchingZone(zones, hostname)
	if !ok {
		return nil, ErrNotFound
	}
	return zone, nil
}

func accessNewBody(in AccessApplicationInput) zero_trust.AccessApplicationNewParamsBodyUnion {
	domains := accessApplicationInputDomains(in)
	policyUUIDs := accessApplicationInputPolicyUUIDs(in)
	policies := make([]zero_trust.AccessApplicationNewParamsBodySelfHostedApplicationPolicyUnion, 0, len(policyUUIDs))
	for idx, policyUUID := range policyUUIDs {
		policies = append(policies, zero_trust.AccessApplicationNewParamsBodySelfHostedApplicationPoliciesAccessAppPolicyLink{
			ID:         cf.F(policyUUID),
			Precedence: cf.F(int64(idx)),
		})
	}
	return zero_trust.AccessApplicationNewParamsBodySelfHostedApplication{
		Domain:            cf.F(accessApplicationPrimaryDomain(domains)),
		Type:              cf.F(zero_trust.ApplicationTypeSelfHosted),
		Name:              cf.F(in.Name),
		Policies:          cf.F(policies),
		SelfHostedDomains: cf.F(accessApplicationSelfHostedDomains(domains)),
		Tags:              cf.F(in.Tags),
	}
}

func accessUpdateBody(in AccessApplicationInput) zero_trust.AccessApplicationUpdateParamsBodyUnion {
	domains := accessApplicationInputDomains(in)
	policyUUIDs := accessApplicationInputPolicyUUIDs(in)
	policies := make([]zero_trust.AccessApplicationUpdateParamsBodySelfHostedApplicationPolicyUnion, 0, len(policyUUIDs))
	for idx, policyUUID := range policyUUIDs {
		policies = append(policies, zero_trust.AccessApplicationUpdateParamsBodySelfHostedApplicationPoliciesAccessAppPolicyLink{
			ID:         cf.F(policyUUID),
			Precedence: cf.F(int64(idx)),
		})
	}
	return zero_trust.AccessApplicationUpdateParamsBodySelfHostedApplication{
		Domain:            cf.F(accessApplicationPrimaryDomain(domains)),
		Type:              cf.F(zero_trust.ApplicationTypeSelfHosted),
		Name:              cf.F(in.Name),
		Policies:          cf.F(policies),
		SelfHostedDomains: cf.F(accessApplicationSelfHostedDomains(domains)),
		Tags:              cf.F(in.Tags),
	}
}

func accessAppFromListResponse(item zero_trust.AccessApplicationListResponse) AccessApplication {
	domains := accessApplicationDomainsFromAny(item.SelfHostedDomains)
	policyUUIDs := accessApplicationPolicyIDs(item.Policies)
	tags := stringSlice(item.Tags)
	if selfHosted, ok := item.AsUnion().(zero_trust.AccessApplicationListResponseSelfHostedApplication); ok {
		policyUUIDs = accessApplicationPolicyIDs(selfHosted.Policies)
		tags = append([]string(nil), selfHosted.Tags...)
	}
	if len(domains) == 0 && item.Domain != "" {
		domains = []string{item.Domain}
	}
	primaryDomain := item.Domain
	if primaryDomain == "" {
		primaryDomain = accessApplicationPrimaryDomain(domains)
	}
	return AccessApplication{
		ID:          item.ID,
		Name:        item.Name,
		Domain:      primaryDomain,
		Domains:     domains,
		PolicyUUIDs: policyUUIDs,
		Tags:        tags,
	}
}

func accessAppFromNewResponse(resp *zero_trust.AccessApplicationNewResponse) *AccessApplication {
	domains := accessApplicationDomainsFromAny(resp.SelfHostedDomains)
	policyUUIDs := accessApplicationPolicyIDs(resp.Policies)
	tags := stringSlice(resp.Tags)
	if selfHosted, ok := resp.AsUnion().(zero_trust.AccessApplicationNewResponseSelfHostedApplication); ok {
		policyUUIDs = accessApplicationPolicyIDs(selfHosted.Policies)
		tags = append([]string(nil), selfHosted.Tags...)
	}
	if len(domains) == 0 && resp.Domain != "" {
		domains = []string{resp.Domain}
	}
	primaryDomain := resp.Domain
	if primaryDomain == "" {
		primaryDomain = accessApplicationPrimaryDomain(domains)
	}
	return &AccessApplication{
		ID:          resp.ID,
		Name:        resp.Name,
		Domain:      primaryDomain,
		Domains:     domains,
		PolicyUUIDs: policyUUIDs,
		Tags:        tags,
	}
}

func accessAppFromUpdateResponse(resp *zero_trust.AccessApplicationUpdateResponse) *AccessApplication {
	domains := accessApplicationDomainsFromAny(resp.SelfHostedDomains)
	policyUUIDs := accessApplicationPolicyIDs(resp.Policies)
	tags := stringSlice(resp.Tags)
	if selfHosted, ok := resp.AsUnion().(zero_trust.AccessApplicationUpdateResponseSelfHostedApplication); ok {
		policyUUIDs = accessApplicationPolicyIDs(selfHosted.Policies)
		tags = append([]string(nil), selfHosted.Tags...)
	}
	if len(domains) == 0 && resp.Domain != "" {
		domains = []string{resp.Domain}
	}
	primaryDomain := resp.Domain
	if primaryDomain == "" {
		primaryDomain = accessApplicationPrimaryDomain(domains)
	}
	return &AccessApplication{
		ID:          resp.ID,
		Name:        resp.Name,
		Domain:      primaryDomain,
		Domains:     domains,
		PolicyUUIDs: policyUUIDs,
		Tags:        tags,
	}
}

func accessApplicationPolicyIDs(value any) []string {
	switch policies := value.(type) {
	case []zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy:
		return selfHostedListPolicyIDs(policies)
	case []zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy:
		return selfHostedNewPolicyIDs(policies)
	case []zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy:
		return selfHostedUpdatePolicyIDs(policies)
	case []zero_trust.AccessApplicationGetResponseSelfHostedApplicationPolicy:
		return selfHostedGetPolicyIDs(policies)
	default:
		return nil
	}
}

func accessApplicationDomainsFromAny(value any) []string {
	switch domains := value.(type) {
	case []string:
		return append([]string(nil), domains...)
	default:
		return nil
	}
}

func accessApplicationSelfHostedDomains(domains []string) []zero_trust.SelfHostedDomainsParam {
	if len(domains) == 0 {
		return nil
	}
	out := make([]zero_trust.SelfHostedDomainsParam, 0, len(domains))
	out = append(out, domains...)
	return out
}

func selfHostedListPolicyIDs(policies []zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy) []string {
	policies = append([]zero_trust.AccessApplicationListResponseSelfHostedApplicationPolicy(nil), policies...)
	sort.SliceStable(policies, func(i, j int) bool {
		return policies[i].Precedence < policies[j].Precedence
	})
	out := make([]string, 0, len(policies))
	for _, policy := range policies {
		if policy.ID != "" {
			out = append(out, policy.ID)
		}
	}
	return out
}

func selfHostedNewPolicyIDs(policies []zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy) []string {
	policies = append([]zero_trust.AccessApplicationNewResponseSelfHostedApplicationPolicy(nil), policies...)
	sort.SliceStable(policies, func(i, j int) bool {
		return policies[i].Precedence < policies[j].Precedence
	})
	out := make([]string, 0, len(policies))
	for _, policy := range policies {
		if policy.ID != "" {
			out = append(out, policy.ID)
		}
	}
	return out
}

func selfHostedUpdatePolicyIDs(policies []zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy) []string {
	policies = append([]zero_trust.AccessApplicationUpdateResponseSelfHostedApplicationPolicy(nil), policies...)
	sort.SliceStable(policies, func(i, j int) bool {
		return policies[i].Precedence < policies[j].Precedence
	})
	out := make([]string, 0, len(policies))
	for _, policy := range policies {
		if policy.ID != "" {
			out = append(out, policy.ID)
		}
	}
	return out
}

func selfHostedGetPolicyIDs(policies []zero_trust.AccessApplicationGetResponseSelfHostedApplicationPolicy) []string {
	policies = append([]zero_trust.AccessApplicationGetResponseSelfHostedApplicationPolicy(nil), policies...)
	sort.SliceStable(policies, func(i, j int) bool {
		return policies[i].Precedence < policies[j].Precedence
	})
	out := make([]string, 0, len(policies))
	for _, policy := range policies {
		if policy.ID != "" {
			out = append(out, policy.ID)
		}
	}
	return out
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
				SessionDuration:              item.SessionDuration,
				PurposeJustificationRequired: item.PurposeJustificationRequired,
				PurposeJustificationPrompt:   item.PurposeJustificationPrompt,
			})
		}
		return pager.Err()
	})
	return out, err
}

func (p *realAccessPolicies) GetMetadata(ctx context.Context, id string) (*AccessPolicy, error) {
	var result *AccessPolicy
	err := p.client.withRetry(ctx, func() error {
		resp, err := p.client.api.ZeroTrust.Access.Policies.Get(ctx, id,
			zero_trust.AccessPolicyGetParams{AccountID: cf.F(p.client.accountID)},
		)
		if err != nil {
			return mapAPIError(err)
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			SessionDuration:              resp.SessionDuration,
			PurposeJustificationRequired: resp.PurposeJustificationRequired,
			PurposeJustificationPrompt:   resp.PurposeJustificationPrompt,
		}
		return nil
	})
	return result, err
}

func (p *realAccessPolicies) Get(ctx context.Context, id string) (*AccessPolicy, error) {
	var result *AccessPolicy
	err := p.client.withRetry(ctx, func() error {
		resp, err := p.client.api.ZeroTrust.Access.Policies.Get(ctx, id,
			zero_trust.AccessPolicyGetParams{AccountID: cf.F(p.client.accountID)},
		)
		if err != nil {
			return mapAPIError(err)
		}
		include, err := fromAccessRules(resp.Include)
		if err != nil {
			return err
		}
		exclude, err := fromAccessRules(resp.Exclude)
		if err != nil {
			return err
		}
		require, err := fromAccessRules(resp.Require)
		if err != nil {
			return err
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			Include:                      include,
			Exclude:                      exclude,
			Require:                      require,
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
		include, err := fromAccessRules(resp.Include)
		if err != nil {
			return err
		}
		exclude, err := fromAccessRules(resp.Exclude)
		if err != nil {
			return err
		}
		require, err := fromAccessRules(resp.Require)
		if err != nil {
			return err
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			Include:                      include,
			Exclude:                      exclude,
			Require:                      require,
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
			return mapAPIError(err)
		}
		include, err := fromAccessRules(resp.Include)
		if err != nil {
			return err
		}
		exclude, err := fromAccessRules(resp.Exclude)
		if err != nil {
			return err
		}
		require, err := fromAccessRules(resp.Require)
		if err != nil {
			return err
		}
		result = &AccessPolicy{
			ID:                           resp.ID,
			Name:                         resp.Name,
			Decision:                     string(resp.Decision),
			Include:                      include,
			Exclude:                      exclude,
			Require:                      require,
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
			return mapAPIError(err)
		}
		return nil
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
// saml — beyond MVP) are surfaced so controllers do not silently erase drift.
func fromAccessRules(rules []zero_trust.AccessRule) ([]AccessRule, error) {
	if len(rules) == 0 {
		return nil, nil
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
		default:
			return nil, fmt.Errorf("%w: %T", ErrUnsupportedAccessRule, u)
		}
	}
	return out, nil
}
