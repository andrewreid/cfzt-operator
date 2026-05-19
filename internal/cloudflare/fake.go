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
	mu             sync.Mutex
	tunnels        map[string]*Tunnel
	tokens         map[string]string
	configurations map[string]TunnelConfiguration
	accessApps     map[string]*AccessApplication
	dnsRecords     map[string]*DNSRecord
	zones          map[string]*Zone
}

// NewFake returns a ready-to-use FakeClient.
func NewFake() *FakeClient {
	return &FakeClient{
		tunnels:        make(map[string]*Tunnel),
		tokens:         make(map[string]string),
		configurations: make(map[string]TunnelConfiguration),
		accessApps:     make(map[string]*AccessApplication),
		dnsRecords:     make(map[string]*DNSRecord),
		zones:          make(map[string]*Zone),
	}
}

// SetTunnelToken overrides the deterministic token for id.
func (f *FakeClient) SetTunnelToken(id, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[id] = token
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

func (t *fakeTunnels) List(_ context.Context, filter ListTunnelsFilter) ([]Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	var out []Tunnel
	for _, tun := range t.fc.tunnels {
		if filter.Name != "" && tun.Name != filter.Name {
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

func (c *fakeConfigurations) Get(_ context.Context, tunnelID string) (*TunnelConfiguration, error) {
	c.fc.mu.Lock()
	defer c.fc.mu.Unlock()
	config, ok := c.fc.configurations[tunnelID]
	if !ok {
		return nil, ErrNotFound
	}
	return copyConfiguration(config), nil
}

func (c *fakeConfigurations) Update(_ context.Context, tunnelID string, config TunnelConfiguration) error {
	c.fc.mu.Lock()
	defer c.fc.mu.Unlock()
	if _, ok := c.fc.tunnels[tunnelID]; !ok {
		return ErrNotFound
	}
	c.fc.configurations[tunnelID] = *copyConfiguration(config)
	return nil
}

type fakeAccessApplications struct {
	fc *FakeClient
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

func (d *fakeDNSRecords) Delete(_ context.Context, _ string, id string) error {
	d.fc.mu.Lock()
	defer d.fc.mu.Unlock()
	if _, ok := d.fc.dnsRecords[id]; !ok {
		return ErrNotFound
	}
	delete(d.fc.dnsRecords, id)
	return nil
}

type fakeZones struct {
	fc *FakeClient
}

func (z *fakeZones) List(_ context.Context) ([]Zone, error) {
	z.fc.mu.Lock()
	defer z.fc.mu.Unlock()
	var out []Zone
	for _, zone := range z.fc.zones {
		out = append(out, *zone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (z *fakeZones) Resolve(ctx context.Context, hostname string) (*Zone, error) {
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

func copyConfiguration(config TunnelConfiguration) *TunnelConfiguration {
	out := TunnelConfiguration{Ingress: append([]IngressRule(nil), config.Ingress...)}
	return &out
}

func applyAccessApplication(app *AccessApplication, in AccessApplicationInput) {
	app.Name = in.Name
	app.Domain = in.Domain
	app.PolicyUUID = in.PolicyUUID
	app.Tags = append([]string(nil), in.Tags...)
}

func copyAccessApplication(app *AccessApplication) AccessApplication {
	copy := *app
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
