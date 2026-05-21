package cloudflare

import "context"

// TunnelRoute mirrors a Cloudflare Tunnel private-network route.
type TunnelRoute struct {
	ID               string
	Network          string
	TunnelID         string
	VirtualNetworkID string
	Comment          string
}

// TunnelRouteInput is the desired-state shape for create/update.
type TunnelRouteInput struct {
	Network          string
	TunnelID         string
	VirtualNetworkID string
	Comment          string
}

// ListTunnelRoutesFilter narrows active cfd_tunnel routes. Network is matched
// exactly by the wrapper after the SDK returns superset/subset candidates.
type ListTunnelRoutesFilter struct {
	Network          string
	TunnelID         string
	VirtualNetworkID string
}

// TunnelRoutes manages Cloudflare Tunnel private-network routes.
type TunnelRoutes interface {
	List(ctx context.Context, filter ListTunnelRoutesFilter) ([]TunnelRoute, error)
	Create(ctx context.Context, in TunnelRouteInput) (*TunnelRoute, error)
	Get(ctx context.Context, id string) (*TunnelRoute, error)
	Edit(ctx context.Context, id string, in TunnelRouteInput) (*TunnelRoute, error)
	Delete(ctx context.Context, id string) error
}
