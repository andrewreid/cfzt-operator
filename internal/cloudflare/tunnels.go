package cloudflare

import "context"

// cloudflare-go/v4 v4.6.0 does not surface a comment field on tunnel responses.
// Tunnel ownership therefore stays on status.tunnelId; other resources carry
// the source-uid tag markers.
type Tunnel struct {
	ID   string
	Name string
}

type CreateTunnelInput struct {
	Name      string
	ConfigSrc string // "cloudflare" for remotely-managed tunnels (D1)
}

// ListTunnelsFilter narrows the List query. Zero value = no filter.
type ListTunnelsFilter struct {
	Name string
}

// Tunnels is the sub-interface for tunnel lifecycle operations.
type Tunnels interface {
	Create(ctx context.Context, in CreateTunnelInput) (*Tunnel, error)
	List(ctx context.Context, filter ListTunnelsFilter) ([]Tunnel, error)
	Get(ctx context.Context, id string) (*Tunnel, error)
	Delete(ctx context.Context, id string) error
	Token(ctx context.Context, id string) (string, error)
}
