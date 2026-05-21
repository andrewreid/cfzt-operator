// Package cloudflare is the sole boundary between the operator and the
// Cloudflare API. Controllers depend on Client; they never import
// cloudflare-go/v4 directly.
package cloudflare

import "errors"

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("cloudflare: resource not found")

// ErrNotImplemented is returned by stub methods not yet wired to the SDK.
var ErrNotImplemented = errors.New("cloudflare: not implemented")

// ErrUnsupportedAccessRule is returned when Cloudflare returns an Access rule
// variant outside the MVP rule set supported by this operator.
var ErrUnsupportedAccessRule = errors.New("cloudflare: unsupported access rule")

// Client is the SDK boundary. Slice 1 exposes Tunnels only; later slices add
// Configurations, Access, DNS, and Zones onto this interface (Slice 2).
type Client interface {
	Tunnels() Tunnels
	Configurations() Configurations
	AccessApplications() AccessApplications
	AccessTags() AccessTags
	AccessPolicies() AccessPolicies
	TunnelRoutes() TunnelRoutes
	DNSRecords() DNSRecords
	Zones() Zones
}
