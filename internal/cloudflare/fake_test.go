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
	config, err := fake.Configurations().Get(ctx, tunnel.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if len(config.Ingress) != 1 || config.Ingress[0].Service != "http_status:404" {
		t.Fatalf("config = %#v", config)
	}
}
