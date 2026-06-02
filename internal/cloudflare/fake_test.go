package cloudflare_test

import (
	"context"
	"errors"
	"testing"

	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

func TestFakeTunnelsCreateGetDelete(t *testing.T) {
	ctx := context.Background()
	fc := cloudflare.NewFake()
	tuns := fc.Tunnels()

	tun, err := tuns.Create(ctx, cloudflare.CreateTunnelInput{Name: "test-tunnel", ConfigSrc: "cloudflare"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tun.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if tun.Name != "test-tunnel" {
		t.Fatalf("Name: got %q want %q", tun.Name, "test-tunnel")
	}

	got, err := tuns.Get(ctx, tun.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != tun.ID || got.Name != tun.Name {
		t.Fatalf("Get returned wrong tunnel: %+v", got)
	}

	if err := tuns.Delete(ctx, tun.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = tuns.Get(ctx, tun.ID)
	if !errors.Is(err, cloudflare.ErrNotFound) {
		t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
	}
}

func TestTunnelsListString(t *testing.T) {
	ctx := context.Background()
	fc := cloudflare.NewFake()
	tuns := fc.Tunnels()

	_, err := tuns.Create(ctx, cloudflare.CreateTunnelInput{Name: "homelab", ConfigSrc: "cloudflare"})
	if err != nil {
		t.Fatalf("Create homelab: %v", err)
	}
	_, err = tuns.Create(ctx, cloudflare.CreateTunnelInput{Name: "other", ConfigSrc: "cloudflare"})
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	got, err := tuns.List(ctx, "homelab")
	if err != nil {
		t.Fatalf("List by name: %v", err)
	}
	if len(got) != 1 || got[0].Name != "homelab" {
		t.Fatalf("List by name = %#v, want only homelab", got)
	}

	all, err := tuns.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List all length = %d, want 2", len(all))
	}
}

func TestFakeTokenIdempotent(t *testing.T) {
	ctx := context.Background()
	fc := cloudflare.NewFake()
	tuns := fc.Tunnels()

	a, _ := tuns.Create(ctx, cloudflare.CreateTunnelInput{Name: "a", ConfigSrc: "cloudflare"})
	b, _ := tuns.Create(ctx, cloudflare.CreateTunnelInput{Name: "b", ConfigSrc: "cloudflare"})

	tok1, err := tuns.Token(ctx, a.ID)
	if err != nil {
		t.Fatalf("Token(a): %v", err)
	}
	tok2, err := tuns.Token(ctx, a.ID)
	if err != nil {
		t.Fatalf("Token(a) repeat: %v", err)
	}
	if tok1 != tok2 {
		t.Fatalf("Token not idempotent: %q != %q", tok1, tok2)
	}

	tokB, err := tuns.Token(ctx, b.ID)
	if err != nil {
		t.Fatalf("Token(b): %v", err)
	}
	if tokB == tok1 {
		t.Fatalf("different IDs should produce different tokens")
	}
}

func TestZoneLongestSuffix(t *testing.T) {
	zone, ok := cloudflare.LongestMatchingZone([]cloudflare.Zone{
		{ID: "root", Name: "example.com"},
		{ID: "apps", Name: "apps.example.com"},
	}, "jellyfin.apps.example.com")
	if !ok {
		t.Fatal("expected zone match")
	}
	if zone.ID != "apps" {
		t.Fatalf("zone ID = %q, want apps", zone.ID)
	}
}

func TestFakeAccessAppRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	app, err := fake.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
		Name:       "jellyfin-cfzt",
		Domain:     "jellyfin.example.com",
		PolicyUUID: "00000000-0000-4000-8000-000000000001",
		Tags:       []string{"managed-by=cfzt-operator", "source-uid=uid-1"},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	apps, err := fake.AccessApplications().List(ctx, "jellyfin.example.com")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != app.ID {
		t.Fatalf("apps = %#v, want created app", apps)
	}
}

func TestFakeAccessApplicationsEnsuresTagsImplicitly(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	app, err := fake.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
		Name:       "jellyfin-cfzt",
		Domain:     "jellyfin.example.com",
		PolicyUUID: "00000000-0000-4000-8000-000000000001",
		Tags:       []string{"managed-by=cfzt-operator", "source-uid-0=uid-1"},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := fake.AccessTags().Delete(ctx, "managed-by=cfzt-operator"); err != nil {
		t.Fatalf("Create did not implicitly ensure managed-by tag: %v", err)
	}
	if err := fake.AccessTags().Delete(ctx, "source-uid-0=uid-1"); err != nil {
		t.Fatalf("Create did not implicitly ensure source UID tag: %v", err)
	}

	_, err = fake.AccessApplications().Update(ctx, app.ID, cloudflare.AccessApplicationInput{
		Name:       "jellyfin-cfzt",
		Domain:     "jellyfin.example.com",
		PolicyUUID: "00000000-0000-4000-8000-000000000001",
		Tags:       []string{"managed-by=cfzt-operator", "source-uid-0=uid-2"},
	})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := fake.AccessTags().Delete(ctx, "managed-by=cfzt-operator"); err != nil {
		t.Fatalf("Update did not implicitly ensure managed-by tag: %v", err)
	}
	if err := fake.AccessTags().Delete(ctx, "source-uid-0=uid-2"); err != nil {
		t.Fatalf("Update did not implicitly ensure source UID tag: %v", err)
	}
}

func TestFakeDNSRecordIdempotent(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	record, err := fake.DNSRecords().Create(ctx, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "jellyfin.example.com",
		Type:    "CNAME",
		Content: "tunnel.cfargotunnel.com",
		Proxied: true,
		Comment: "managed-by=cfzt-operator source-uid=uid-1",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	updated, err := fake.DNSRecords().Update(ctx, record.ID, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "jellyfin.example.com",
		Type:    "CNAME",
		Content: "new.cfargotunnel.com",
		Proxied: true,
		Comment: record.Comment,
	})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated.ID != record.ID || updated.Content != "new.cfargotunnel.com" {
		t.Fatalf("updated = %#v", updated)
	}
}

func TestFakeDNSRecordIDStable(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	created, err := fake.DNSRecords().CreateCAS(ctx, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "_cfzt-lease.deadbeef.example.com",
		Type:    "TXT",
		Content: "v=1 site=primary tunnel=t-1 exp=200 renewed=100",
	})
	if err != nil {
		t.Fatalf("CreateCAS returned error: %v", err)
	}
	updated, err := fake.DNSRecords().UpdateCAS(ctx, created.ID, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "_cfzt-lease.deadbeef.example.com",
		Type:    "TXT",
		Content: "v=1 site=primary tunnel=t-1 exp=260 renewed=130",
	})
	if err != nil {
		t.Fatalf("UpdateCAS returned error: %v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("UpdateCAS rotated record id: got %q, want stable %q", updated.ID, created.ID)
	}
	listed, err := fake.DNSRecords().List(ctx, "zone-1", "_cfzt-lease.deadbeef.example.com", "TXT")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("List = %#v, want single record with id %q", listed, created.ID)
	}
}

func TestFakeDNSCASCreateConflict(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	in := cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "_cfzt-lease.deadbeef.example.com",
		Type:    "TXT",
		Content: "v=1 site=primary tunnel=t-1 exp=200 renewed=100",
	}
	if _, err := fake.DNSRecords().CreateCAS(ctx, in); err != nil {
		t.Fatalf("first CreateCAS returned error: %v", err)
	}
	in.Content = "v=1 site=standby tunnel=t-2 exp=300 renewed=150"
	if _, err := fake.DNSRecords().CreateCAS(ctx, in); !errors.Is(err, cloudflare.ErrDNSCASConflict) {
		t.Fatalf("second CreateCAS = %v, want ErrDNSCASConflict", err)
	}
	listed, _ := fake.DNSRecords().List(ctx, "zone-1", "_cfzt-lease.deadbeef.example.com", "TXT")
	if len(listed) != 1 {
		t.Fatalf("List len = %d, want 1 (loser must not have written)", len(listed))
	}
}

func TestFakeDNSCASUpdateRejectsStaleRecordID(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	in := cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "_cfzt-lease.deadbeef.example.com",
		Type:    "TXT",
		Content: "v=1 site=primary tunnel=t-1 exp=200 renewed=100",
	}
	first, err := fake.DNSRecords().CreateCAS(ctx, in)
	if err != nil {
		t.Fatalf("CreateCAS returned error: %v", err)
	}
	staleID := first.ID
	if err := fake.DNSRecords().Delete(ctx, "zone-1", first.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	winner, err := fake.DNSRecords().CreateCAS(ctx, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "_cfzt-lease.deadbeef.example.com",
		Type:    "TXT",
		Content: "v=1 site=standby tunnel=t-2 exp=300 renewed=150",
	})
	if err != nil {
		t.Fatalf("second CreateCAS returned error: %v", err)
	}
	if winner.ID == staleID {
		t.Fatalf("winner reused stale id %q", staleID)
	}
	loserUpdate, err := fake.DNSRecords().UpdateCAS(ctx, staleID, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "_cfzt-lease.deadbeef.example.com",
		Type:    "TXT",
		Content: "v=1 site=primary tunnel=t-1 exp=400 renewed=200",
	})
	if !errors.Is(err, cloudflare.ErrDNSCASConflict) {
		t.Fatalf("UpdateCAS stale = %v, %v; want ErrDNSCASConflict", loserUpdate, err)
	}
	listed, _ := fake.DNSRecords().List(ctx, "zone-1", "_cfzt-lease.deadbeef.example.com", "TXT")
	if len(listed) != 1 || listed[0].ID != winner.ID || listed[0].Content != "v=1 site=standby tunnel=t-2 exp=300 renewed=150" {
		t.Fatalf("post-conflict state = %#v; want winner record untouched", listed)
	}
}

func TestFakeDNSRecordDeleteRequiresZoneID(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	record, err := fake.DNSRecords().Create(ctx, cloudflare.DNSRecordInput{
		ZoneID:  "zone-1",
		Name:    "jellyfin.example.com",
		Type:    "CNAME",
		Content: "tunnel.cfargotunnel.com",
		Proxied: true,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := fake.DNSRecords().Delete(ctx, "zone-2", record.ID); !errors.Is(err, cloudflare.ErrNotFound) {
		t.Fatalf("Delete wrong zone = %v, want ErrNotFound", err)
	}
	if err := fake.DNSRecords().Delete(ctx, "zone-1", record.ID); err != nil {
		t.Fatalf("Delete correct zone returned error: %v", err)
	}
}

func TestFakeZonesResolveCachesAndRefreshesOnMiss(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	fake.AddZone("root", "example.com")

	zone, err := fake.Zones().Resolve(ctx, "app.example.com")
	if err != nil {
		t.Fatalf("Resolve first zone returned error: %v", err)
	}
	if zone.ID != "root" {
		t.Fatalf("zone ID = %q, want root", zone.ID)
	}
	if calls := fake.ZoneListCalls(); calls != 1 {
		t.Fatalf("List calls = %d, want 1", calls)
	}

	if _, err := fake.Zones().Resolve(ctx, "other.example.com"); err != nil {
		t.Fatalf("Resolve cached zone returned error: %v", err)
	}
	if calls := fake.ZoneListCalls(); calls != 1 {
		t.Fatalf("cached resolve List calls = %d, want 1", calls)
	}

	fake.AddZone("apps", "apps.internal")
	zone, err = fake.Zones().Resolve(ctx, "jellyfin.apps.internal")
	if err != nil {
		t.Fatalf("Resolve cache miss returned error: %v", err)
	}
	if zone.ID != "apps" {
		t.Fatalf("zone ID after miss = %q, want apps", zone.ID)
	}
	if calls := fake.ZoneListCalls(); calls != 2 {
		t.Fatalf("miss refresh List calls = %d, want 2", calls)
	}
}

func TestFakeConfigurationsPutOverwrites(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	tunnel, err := fake.Tunnels().Create(ctx, cloudflare.CreateTunnelInput{Name: "homelab", ConfigSrc: "cloudflare"})
	if err != nil {
		t.Fatalf("Create tunnel returned error: %v", err)
	}
	if err := fake.Configurations().Update(ctx, tunnel.ID, cloudflare.TunnelConfiguration{Ingress: []cloudflare.IngressRule{{Hostname: "a.example.com", Service: "http://a:80"}}}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if err := fake.Configurations().Update(ctx, tunnel.ID, cloudflare.TunnelConfiguration{Ingress: []cloudflare.IngressRule{{Service: "http_status:404"}}}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	config, err := fake.Configuration(tunnel.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if len(config.Ingress) != 1 || config.Ingress[0].Service != "http_status:404" {
		t.Fatalf("config = %#v", config)
	}
}
