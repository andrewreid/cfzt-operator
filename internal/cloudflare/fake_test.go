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
