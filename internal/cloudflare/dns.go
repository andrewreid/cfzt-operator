package cloudflare

import (
	"context"
	"errors"
)

// ErrDNSCASConflict is returned by the CAS-capable DNS writes when the
// observed live state no longer matches what the caller saw. Surfaces the
// D26 failover lease "another site won the race" outcome so the Exposure
// controller can fall back to a re-read + retry instead of treating it as a
// transient write failure. Returned by CreateCAS when a record already exists
// at the target (zone, name, type) triple, and by UpdateCAS when the live
// record at that triple no longer has the expected record_id.
var ErrDNSCASConflict = errors.New("cloudflare: DNS CAS conflict")

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

	// CreateCAS is the failover-lease acquire path. It creates a record at
	// (in.ZoneID, in.Name, in.Type) only if no record exists at that triple
	// at observation time. Returns ErrDNSCASConflict if any record already
	// matches, so the loser of a concurrent acquire can fall back to a
	// re-read.
	CreateCAS(ctx context.Context, in DNSRecordInput) (*DNSRecord, error)

	// UpdateCAS is the failover-lease renewal path. It updates the live
	// record at (in.ZoneID, in.Name, in.Type) only if its current record_id
	// still equals expectedID. Returns ErrDNSCASConflict if the record was
	// replaced (different id), removed, or no longer matches the expected
	// triple — the caller must then re-read and re-decide its role.
	UpdateCAS(ctx context.Context, expectedID string, in DNSRecordInput) (*DNSRecord, error)
}
