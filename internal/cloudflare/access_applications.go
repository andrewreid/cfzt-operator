package cloudflare

import "context"

type AccessApplication struct {
	ID          string
	Name        string
	Domain      string
	PolicyUUIDs []string
	Tags        []string
}

type AccessApplicationInput struct {
	Name       string
	Domain     string
	PolicyUUID string
	Tags       []string
}

type AccessApplications interface {
	List(ctx context.Context, domain string) ([]AccessApplication, error)
	Create(ctx context.Context, in AccessApplicationInput) (*AccessApplication, error)
	Update(ctx context.Context, id string, in AccessApplicationInput) (*AccessApplication, error)
	Delete(ctx context.Context, id string) error
}
