package cloudflare

import "context"

// AccessPolicy is the package-local mirror of a Cloudflare Access reusable
// policy. It is intentionally decoupled from api/v1alpha1.AccessRule so
// controllers translate between the two; cloudflare-go types never leak past
// this package (D13 / AGENTS.md ## Cloudflare Rules).
type AccessPolicy struct {
	ID                           string
	Name                         string
	Decision                     string
	Include                      []AccessRule
	Exclude                      []AccessRule
	Require                      []AccessRule
	SessionDuration              string
	PurposeJustificationRequired bool
	PurposeJustificationPrompt   string
}

// AccessPolicyInput is the desired-state shape supplied by callers.
type AccessPolicyInput struct {
	Name                         string
	Decision                     string
	Include                      []AccessRule
	Exclude                      []AccessRule
	Require                      []AccessRule
	SessionDuration              string
	PurposeJustificationRequired bool
	PurposeJustificationPrompt   string
}

// AccessRule mirrors the discriminated-union shape of
// api/v1alpha1.AccessRule. Exactly one field must be set per value. This is
// NOT the same Go type as v1alpha1.AccessRule; controllers translate.
type AccessRule struct {
	Email          string
	EmailDomain    string
	IP             string
	Everyone       bool
	ServiceToken   string
	GeoCountryCode string
}

// AccessPolicies is the sub-interface for Cloudflare Access reusable policies.
// List does NOT support a server-side name filter — callers filter client-side.
type AccessPolicies interface {
	List(ctx context.Context) ([]AccessPolicy, error)
	Get(ctx context.Context, id string) (*AccessPolicy, error)
	Create(ctx context.Context, in AccessPolicyInput) (*AccessPolicy, error)
	Update(ctx context.Context, id string, in AccessPolicyInput) (*AccessPolicy, error)
	Delete(ctx context.Context, id string) error
}
