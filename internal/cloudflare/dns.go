package cloudflare

import "context"

type DNSRecord struct {
	ID      string
	ZoneID  string
	Name    string
	Type    string
	Content string
	Proxied bool
	Comment string
}

type DNSRecordInput struct {
	ZoneID  string
	Name    string
	Type    string
	Content string
	Proxied bool
	Comment string
}

// DNSRecords is a thin wrapper over the Cloudflare DNS records API. It models
// real Cloudflare semantics exactly: there is no conditional-write precondition
// and no TXT-uniqueness guarantee, so Create is non-atomic (two callers racing
// from an absent record can both create one, yielding duplicates) and Update is
// unconditional by record_id. The D26 failover lease builds best-effort
// coordination on top of these primitives in the controller + internal/dr; the
// client provides no atomicity itself.
type DNSRecords interface {
	List(ctx context.Context, zoneID, name, recordType string) ([]DNSRecord, error)
	Create(ctx context.Context, in DNSRecordInput) (*DNSRecord, error)
	Update(ctx context.Context, id string, in DNSRecordInput) (*DNSRecord, error)
	Delete(ctx context.Context, zoneID, id string) error
}
