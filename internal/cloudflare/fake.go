package cloudflare

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// FakeClient is an in-memory implementation of Client for unit tests.
// No global state; each instance is independent.
type FakeClient struct {
	mu                        sync.Mutex
	tunnels                   map[string]*Tunnel
	tokens                    map[string]string
	configurations            map[string]TunnelConfiguration
	configurationUpdateCalls  map[string]int
	accessApps                map[string]*AccessApplication
	accessTags                map[string]bool
	accessPolicies            map[string]*AccessPolicy
	unsupportedAccessPolicies map[string]bool
	tunnelRoutes              map[string]*TunnelRoute
	dnsRecords                map[string]*DNSRecord
	zones                     map[string]*Zone
	zoneCache                 []Zone
	zoneCacheReady            bool
	zoneListCalls             int
}

// NewFake returns a ready-to-use FakeClient.
func NewFake() *FakeClient {
	return &FakeClient{
		tunnels:                   make(map[string]*Tunnel),
		tokens:                    make(map[string]string),
		configurations:            make(map[string]TunnelConfiguration),
		configurationUpdateCalls:  make(map[string]int),
		accessApps:                make(map[string]*AccessApplication),
		accessTags:                make(map[string]bool),
		accessPolicies:            make(map[string]*AccessPolicy),
		unsupportedAccessPolicies: make(map[string]bool),
		tunnelRoutes:              make(map[string]*TunnelRoute),
		dnsRecords:                make(map[string]*DNSRecord),
		zones:                     make(map[string]*Zone),
	}
}

// SetTunnelToken overrides the deterministic token for id.
func (f *FakeClient) SetTunnelToken(id, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[id] = token
}

func (f *FakeClient) Configuration(tunnelID string) (*TunnelConfiguration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	config, ok := f.configurations[tunnelID]
	if !ok {
		return nil, ErrNotFound
	}
	return copyConfiguration(config), nil
}

func (f *FakeClient) ConfigurationUpdateCalls(tunnelID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configurationUpdateCalls[tunnelID]
}

func (f *FakeClient) SetAccessApplicationPolicyUUIDs(id string, policyUUIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	app, ok := f.accessApps[id]
	if !ok {
		return ErrNotFound
	}
	app.PolicyUUIDs = append([]string(nil), policyUUIDs...)
	return nil
}

func (f *FakeClient) Tunnels() Tunnels {
	return &fakeTunnels{fc: f}
}

func (f *FakeClient) Configurations() Configurations {
	return &fakeConfigurations{fc: f}
}

func (f *FakeClient) AccessApplications() AccessApplications {
	return &fakeAccessApplications{fc: f}
}

func (f *FakeClient) AccessTags() AccessTags {
	return &fakeAccessTags{fc: f}
}

func (f *FakeClient) AccessPolicies() AccessPolicies {
	return &fakeAccessPolicies{fc: f}
}

func (f *FakeClient) TunnelRoutes() TunnelRoutes {
	return &fakeTunnelRoutes{fc: f}
}

func (f *FakeClient) DNSRecords() DNSRecords {
	return &fakeDNSRecords{fc: f}
}

func (f *FakeClient) Zones() Zones {
	return &fakeZones{fc: f}
}

func (f *FakeClient) AddZone(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones[id] = &Zone{ID: id, Name: name}
}

func (f *FakeClient) ZoneListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.zoneListCalls
}

// MarkAccessPolicyUnsupported makes Get return ErrUnsupportedAccessRule for id.
func (f *FakeClient) MarkAccessPolicyUnsupported(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsupportedAccessPolicies[id] = true
}

type fakeTunnels struct {
	fc *FakeClient
}

func (t *fakeTunnels) Create(_ context.Context, in CreateTunnelInput) (*Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	id := uuid.New().String()
	tun := &Tunnel{ID: id, Name: in.Name}
	t.fc.tunnels[id] = tun
	copy := *tun
	return &copy, nil
}

func (t *fakeTunnels) Rename(_ context.Context, id string, in RenameTunnelInput) (*Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	tun, ok := t.fc.tunnels[id]
	if !ok {
		return nil, ErrNotFound
	}
	tun.Name = in.Name
	copy := *tun
	return &copy, nil
}

func (t *fakeTunnels) List(_ context.Context, name string) ([]Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	var out []Tunnel
	for _, tun := range t.fc.tunnels {
		if name != "" && tun.Name != name {
			continue
		}
		out = append(out, *tun)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *fakeTunnels) Get(_ context.Context, id string) (*Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	tun, ok := t.fc.tunnels[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *tun
	return &copy, nil
}

func (t *fakeTunnels) Delete(_ context.Context, id string) error {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if _, ok := t.fc.tunnels[id]; !ok {
		return ErrNotFound
	}
	delete(t.fc.tunnels, id)
	return nil
}

// Token returns a deterministic token derived from the tunnel ID so callers get
// the same value on repeated calls and different IDs produce different tokens.
func (t *fakeTunnels) Token(_ context.Context, id string) (string, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if _, ok := t.fc.tunnels[id]; !ok {
		return "", ErrNotFound
	}
	if token, ok := t.fc.tokens[id]; ok {
		return token, nil
	}
	sum := sha256.Sum256([]byte("fake-token:" + id))
	return fmt.Sprintf("%x", sum), nil
}

type fakeConfigurations struct {
	fc *FakeClient
}

func (c *fakeConfigurations) Update(_ context.Context, tunnelID string, config TunnelConfiguration) error {
	c.fc.mu.Lock()
	defer c.fc.mu.Unlock()
	if _, ok := c.fc.tunnels[tunnelID]; !ok {
		return ErrNotFound
	}
	c.fc.configurations[tunnelID] = *copyConfiguration(config)
	c.fc.configurationUpdateCalls[tunnelID]++
	return nil
}

type fakeAccessApplications struct {
	fc *FakeClient
}

type fakeAccessTags struct {
	fc *FakeClient
}

func (t *fakeAccessTags) Ensure(_ context.Context, name string) error {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	t.fc.accessTags[name] = true
	return nil
}

func (t *fakeAccessTags) Delete(_ context.Context, name string) error {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if !t.fc.accessTags[name] {
		return ErrNotFound
	}
	delete(t.fc.accessTags, name)
	return nil
}

func (a *fakeAccessApplications) List(_ context.Context, domain string) ([]AccessApplication, error) {
	a.fc.mu.Lock()
	defer a.fc.mu.Unlock()
	var out []AccessApplication
	for _, app := range a.fc.accessApps {
		if domain != "" && app.Domain != domain {
			continue
		}
		out = append(out, copyAccessApplication(app))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a *fakeAccessApplications) Create(_ context.Context, in AccessApplicationInput) (*AccessApplication, error) {
	a.fc.mu.Lock()
	defer a.fc.mu.Unlock()
	id := uuid.New().String()
	app := &AccessApplication{ID: id}
	ensureFakeAccessTagsLocked(a.fc, in.Tags)
	applyAccessApplication(app, in)
	a.fc.accessApps[id] = app
	copy := copyAccessApplication(app)
	return &copy, nil
}

func (a *fakeAccessApplications) Update(_ context.Context, id string, in AccessApplicationInput) (*AccessApplication, error) {
	a.fc.mu.Lock()
	defer a.fc.mu.Unlock()
	app, ok := a.fc.accessApps[id]
	if !ok {
		return nil, ErrNotFound
	}
	ensureFakeAccessTagsLocked(a.fc, in.Tags)
	applyAccessApplication(app, in)
	copy := copyAccessApplication(app)
	return &copy, nil
}

func (a *fakeAccessApplications) Delete(_ context.Context, id string) error {
	a.fc.mu.Lock()
	defer a.fc.mu.Unlock()
	if _, ok := a.fc.accessApps[id]; !ok {
		return ErrNotFound
	}
	delete(a.fc.accessApps, id)
	return nil
}

type fakeDNSRecords struct {
	fc *FakeClient
}

type fakeTunnelRoutes struct {
	fc *FakeClient
}

func (r *fakeTunnelRoutes) List(_ context.Context, filter ListTunnelRoutesFilter) ([]TunnelRoute, error) {
	r.fc.mu.Lock()
	defer r.fc.mu.Unlock()
	var out []TunnelRoute
	for _, route := range r.fc.tunnelRoutes {
		if filter.Network != "" && route.Network != filter.Network {
			continue
		}
		if filter.TunnelID != "" && route.TunnelID != filter.TunnelID {
			continue
		}
		if filter.VirtualNetworkID != "" && route.VirtualNetworkID != filter.VirtualNetworkID {
			continue
		}
		out = append(out, *route)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *fakeTunnelRoutes) Create(_ context.Context, in TunnelRouteInput) (*TunnelRoute, error) {
	r.fc.mu.Lock()
	defer r.fc.mu.Unlock()
	id := uuid.New().String()
	route := &TunnelRoute{ID: id}
	applyTunnelRoute(route, in)
	r.fc.tunnelRoutes[id] = route
	copy := *route
	return &copy, nil
}

func (r *fakeTunnelRoutes) Get(_ context.Context, id string) (*TunnelRoute, error) {
	r.fc.mu.Lock()
	defer r.fc.mu.Unlock()
	route, ok := r.fc.tunnelRoutes[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *route
	return &copy, nil
}

func (r *fakeTunnelRoutes) Edit(_ context.Context, id string, in TunnelRouteInput) (*TunnelRoute, error) {
	r.fc.mu.Lock()
	defer r.fc.mu.Unlock()
	route, ok := r.fc.tunnelRoutes[id]
	if !ok {
		return nil, ErrNotFound
	}
	applyTunnelRoute(route, in)
	copy := *route
	return &copy, nil
}

func (r *fakeTunnelRoutes) Delete(_ context.Context, id string) error {
	r.fc.mu.Lock()
	defer r.fc.mu.Unlock()
	if _, ok := r.fc.tunnelRoutes[id]; !ok {
		return ErrNotFound
	}
	delete(r.fc.tunnelRoutes, id)
	return nil
}

func applyTunnelRoute(route *TunnelRoute, in TunnelRouteInput) {
	route.Network = in.Network
	route.TunnelID = in.TunnelID
	route.VirtualNetworkID = in.VirtualNetworkID
	route.Comment = in.Comment
}

func (d *fakeDNSRecords) List(_ context.Context, zoneID, name, recordType string) ([]DNSRecord, error) {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	var out []DNSRecord
	for _, record := range d.fc.dnsRecords {
		if zoneID != "" && record.ZoneID != zoneID {
			continue
		}
		if name != "" && record.Name != name {
			continue
		}
		if recordType != "" && record.Type != recordType {
			continue
		}
		out = append(out, *record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (d *fakeDNSRecords) Create(_ context.Context, in DNSRecordInput) (*DNSRecord, error) {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	id := uuid.New().String()
	record := &DNSRecord{ID: id}
	applyDNSRecord(record, in)
	d.fc.dnsRecords[id] = record
	copy := *record
	return &copy, nil
}

func (d *fakeDNSRecords) Update(_ context.Context, id string, in DNSRecordInput) (*DNSRecord, error) {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	record, ok := d.fc.dnsRecords[id]
	if !ok {
		return nil, ErrNotFound
	}
	applyDNSRecord(record, in)
	copy := *record
	return &copy, nil
}

func (d *fakeDNSRecords) Delete(_ context.Context, zoneID, id string) error {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	record, ok := d.fc.dnsRecords[id]
	if !ok || record.ZoneID != zoneID {
		return ErrNotFound
	}
	delete(d.fc.dnsRecords, id)
	return nil
}

// CreateCAS enforces the failover-lease acquire invariant: at most one record
// at a (zoneID, name, type) triple at a time. Two goroutines racing through
// fake state under one shared FakeClient will see exactly one succeed and the
// other receive ErrDNSCASConflict, mirroring the CAS-by-record_id contract of
// the live Cloudflare DNS API.
func (d *fakeDNSRecords) CreateCAS(_ context.Context, in DNSRecordInput) (*DNSRecord, error) {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	for _, existing := range d.fc.dnsRecords {
		if existing.ZoneID == in.ZoneID && existing.Name == in.Name && existing.Type == in.Type {
			return nil, ErrDNSCASConflict
		}
	}
	id := uuid.New().String()
	record := &DNSRecord{ID: id}
	applyDNSRecord(record, in)
	d.fc.dnsRecords[id] = record
	copy := *record
	return &copy, nil
}

// UpdateCAS enforces the failover-lease renewal invariant: the caller may
// only mutate the record it observed. If the live record at the target triple
// no longer carries expectedID — because a peer has acquired, the record was
// removed, or the triple itself changed — the update is rejected so the
// caller falls back to a re-read instead of clobbering the new owner's state.
func (d *fakeDNSRecords) UpdateCAS(_ context.Context, expectedID string, in DNSRecordInput) (*DNSRecord, error) {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	record, ok := d.fc.dnsRecords[expectedID]
	if !ok {
		return nil, ErrDNSCASConflict
	}
	if record.ZoneID != in.ZoneID || record.Name != in.Name || record.Type != in.Type {
		return nil, ErrDNSCASConflict
	}
	applyDNSRecord(record, in)
	copy := *record
	return &copy, nil
}

type fakeZones struct {
	fc *FakeClient
}

func (z *fakeZones) List(_ context.Context) ([]Zone, error) {
	z.fc.mu.Lock()
	defer z.fc.mu.Unlock()
	z.fc.zoneListCalls++
	out := make([]Zone, 0, len(z.fc.zones))
	for _, zone := range z.fc.zones {
		out = append(out, *zone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (z *fakeZones) Resolve(ctx context.Context, hostname string) (*Zone, error) {
	z.fc.mu.Lock()
	if z.fc.zoneCacheReady {
		if zone, ok := LongestMatchingZone(z.fc.zoneCache, hostname); ok {
			z.fc.mu.Unlock()
			return zone, nil
		}
	}
	z.fc.mu.Unlock()

	zones, err := z.List(ctx)
	if err != nil {
		return nil, err
	}
	z.fc.mu.Lock()
	z.fc.zoneCache = append([]Zone(nil), zones...)
	z.fc.zoneCacheReady = true
	z.fc.mu.Unlock()

	zone, ok := LongestMatchingZone(zones, hostname)
	if !ok {
		return nil, ErrNotFound
	}
	return zone, nil
}

func copyConfiguration(config TunnelConfiguration) *TunnelConfiguration {
	out := TunnelConfiguration{Ingress: append([]IngressRule(nil), config.Ingress...)}
	return &out
}

func applyAccessApplication(app *AccessApplication, in AccessApplicationInput) {
	app.Name = in.Name
	app.Domain = in.Domain
	if in.PolicyUUID == "" {
		app.PolicyUUIDs = nil
	} else {
		app.PolicyUUIDs = []string{in.PolicyUUID}
	}
	app.Tags = append([]string(nil), in.Tags...)
}

func ensureFakeAccessTagsLocked(fc *FakeClient, tags []string) {
	for _, tag := range tags {
		fc.accessTags[tag] = true
	}
}

func copyAccessApplication(app *AccessApplication) AccessApplication {
	copy := *app
	copy.PolicyUUIDs = append([]string(nil), app.PolicyUUIDs...)
	copy.Tags = append([]string(nil), app.Tags...)
	return copy
}

func applyDNSRecord(record *DNSRecord, in DNSRecordInput) {
	record.ZoneID = in.ZoneID
	record.Name = in.Name
	record.Type = in.Type
	record.Content = in.Content
	record.Proxied = in.Proxied
	record.Comment = in.Comment
}

type fakeAccessPolicies struct {
	fc *FakeClient
}

func (p *fakeAccessPolicies) List(_ context.Context) ([]AccessPolicy, error) {
	p.fc.mu.Lock()
	defer p.fc.mu.Unlock()
	out := make([]AccessPolicy, 0, len(p.fc.accessPolicies))
	for _, pol := range p.fc.accessPolicies {
		out = append(out, accessPolicyMetadata(pol))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *fakeAccessPolicies) GetMetadata(_ context.Context, id string) (*AccessPolicy, error) {
	p.fc.mu.Lock()
	defer p.fc.mu.Unlock()
	pol, ok := p.fc.accessPolicies[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := accessPolicyMetadata(pol)
	return &c, nil
}

func (p *fakeAccessPolicies) Get(_ context.Context, id string) (*AccessPolicy, error) {
	p.fc.mu.Lock()
	defer p.fc.mu.Unlock()
	pol, ok := p.fc.accessPolicies[id]
	if !ok {
		return nil, ErrNotFound
	}
	if p.fc.unsupportedAccessPolicies[id] {
		return nil, ErrUnsupportedAccessRule
	}
	c := copyAccessPolicy(pol)
	return &c, nil
}

func (p *fakeAccessPolicies) Create(_ context.Context, in AccessPolicyInput) (*AccessPolicy, error) {
	p.fc.mu.Lock()
	defer p.fc.mu.Unlock()
	id := uuid.New().String()
	pol := &AccessPolicy{ID: id}
	applyAccessPolicy(pol, in)
	p.fc.accessPolicies[id] = pol
	c := copyAccessPolicy(pol)
	return &c, nil
}

func (p *fakeAccessPolicies) Update(_ context.Context, id string, in AccessPolicyInput) (*AccessPolicy, error) {
	p.fc.mu.Lock()
	defer p.fc.mu.Unlock()
	pol, ok := p.fc.accessPolicies[id]
	if !ok {
		return nil, ErrNotFound
	}
	applyAccessPolicy(pol, in)
	c := copyAccessPolicy(pol)
	return &c, nil
}

func (p *fakeAccessPolicies) Delete(_ context.Context, id string) error {
	p.fc.mu.Lock()
	defer p.fc.mu.Unlock()
	if _, ok := p.fc.accessPolicies[id]; !ok {
		return ErrNotFound
	}
	delete(p.fc.accessPolicies, id)
	return nil
}

func applyAccessPolicy(pol *AccessPolicy, in AccessPolicyInput) {
	pol.Name = in.Name
	pol.Decision = in.Decision
	pol.Include = append([]AccessRule(nil), in.Include...)
	pol.Exclude = append([]AccessRule(nil), in.Exclude...)
	pol.Require = append([]AccessRule(nil), in.Require...)
	pol.SessionDuration = in.SessionDuration
	pol.PurposeJustificationRequired = in.PurposeJustificationRequired
	pol.PurposeJustificationPrompt = in.PurposeJustificationPrompt
}

func copyAccessPolicy(pol *AccessPolicy) AccessPolicy {
	c := *pol
	c.Include = append([]AccessRule(nil), pol.Include...)
	c.Exclude = append([]AccessRule(nil), pol.Exclude...)
	c.Require = append([]AccessRule(nil), pol.Require...)
	return c
}

func accessPolicyMetadata(pol *AccessPolicy) AccessPolicy {
	return AccessPolicy{
		ID:                           pol.ID,
		Name:                         pol.Name,
		Decision:                     pol.Decision,
		SessionDuration:              pol.SessionDuration,
		PurposeJustificationRequired: pol.PurposeJustificationRequired,
		PurposeJustificationPrompt:   pol.PurposeJustificationPrompt,
	}
}
