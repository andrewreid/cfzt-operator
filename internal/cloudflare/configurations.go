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
	Update(ctx context.Context, tunnelID string, config TunnelConfiguration) error
}
