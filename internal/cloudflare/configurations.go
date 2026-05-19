package cloudflare

import "context"

type IngressRule struct {
	Hostname string
	Service  string
}

type TunnelConfiguration struct {
	Ingress []IngressRule
}

type Configurations interface {
	Get(ctx context.Context, tunnelID string) (*TunnelConfiguration, error)
	Update(ctx context.Context, tunnelID string, config TunnelConfiguration) error
}
