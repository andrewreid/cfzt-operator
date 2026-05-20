package cloudflare

import "context"

// AccessTags is the sub-interface for Cloudflare Access application tags.
// Cloudflare requires tag names to exist before they can be assigned to an
// Access application.
type AccessTags interface {
	Ensure(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
}
