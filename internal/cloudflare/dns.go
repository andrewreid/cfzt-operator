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

type DNSRecords interface {
	List(ctx context.Context, zoneID, name, recordType string) ([]DNSRecord, error)
	Create(ctx context.Context, in DNSRecordInput) (*DNSRecord, error)
	Update(ctx context.Context, id string, in DNSRecordInput) (*DNSRecord, error)
	Delete(ctx context.Context, zoneID, id string) error
}
