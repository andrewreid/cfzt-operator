package cloudflare_test

import (
	"context"
	"errors"
	"testing"

	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

func TestFakeRouteCreateGetDelete(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	route, err := fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{
		Network:  testNetwork1,
		TunnelID: testTunnelID,
		Comment:  "managed-by=cfzt source-uid=uid-1",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if route.ID == "" || route.Network != testNetwork1 {
		t.Fatalf("route = %#v", route)
	}
	got, err := fake.TunnelRoutes().Get(ctx, route.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.ID != route.ID || got.Comment != route.Comment {
		t.Fatalf("got = %#v, want %#v", got, route)
	}
	if err := fake.TunnelRoutes().Delete(ctx, route.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	_, err = fake.TunnelRoutes().Get(ctx, route.ID)
	if !errors.Is(err, cloudflare.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestFakeRouteListByCanonicalCIDRAndVNet(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	want, _ := fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: testNetwork1, TunnelID: testTunnelID, VirtualNetworkID: testVnetID})
	_, _ = fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: testNetwork2, TunnelID: testTunnelID, VirtualNetworkID: testVnetID})
	_, _ = fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: testNetwork1, TunnelID: testTunnelID, VirtualNetworkID: "vnet-2"})

	routes, err := fake.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{Network: testNetwork1, VirtualNetworkID: testVnetID})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(routes) != 1 || routes[0].ID != want.ID {
		t.Fatalf("routes = %#v, want only %s", routes, want.ID)
	}
}

func TestFakeRouteListOmitsVNetWhenUnset(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	_, _ = fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: testNetwork1, TunnelID: testTunnelID, VirtualNetworkID: testVnetID})
	_, _ = fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: testNetwork1, TunnelID: "tunnel-2", VirtualNetworkID: "vnet-2"})

	routes, err := fake.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{Network: testNetwork1})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes length = %d, want 2", len(routes))
	}
}

func TestFakeRouteEditIdempotent(t *testing.T) {
	ctx := context.Background()
	fake := cloudflare.NewFake()
	route, _ := fake.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: testNetwork1, TunnelID: testTunnelID, Comment: "old"})

	updated, err := fake.TunnelRoutes().Edit(ctx, route.ID, cloudflare.TunnelRouteInput{Network: testNetwork2, TunnelID: testTunnelID, Comment: "new"})
	if err != nil {
		t.Fatalf("Edit returned error: %v", err)
	}
	if updated.ID != route.ID || updated.Network != testNetwork2 || updated.Comment != "new" {
		t.Fatalf("updated = %#v", updated)
	}
}
