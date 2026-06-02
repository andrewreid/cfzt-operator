package dr_test

import (
	"testing"
	"time"

	"github.com/andrewreid/cfzt-operator/internal/dr"
)

func rec(id, site string, expires time.Time) dr.LeaseRecord {
	return dr.LeaseRecord{ID: id, Lease: dr.Lease{Version: 1, Site: site, Tunnel: "t-" + site, Expires: expires, Renewed: expires.Add(-time.Minute)}}
}

func TestResolveEmpty(t *testing.T) {
	r := dr.Resolve(nil, time.Unix(100, 0), "a")
	if r.WinnerID != "" || len(r.DeleteIDs) != 0 || r.IAmWinner {
		t.Fatalf("empty Resolve = %#v, want zero", r)
	}
}

func TestResolveSingleRecordNoDelete(t *testing.T) {
	now := time.Unix(100, 0)
	r := dr.Resolve([]dr.LeaseRecord{rec("r1", "site-a", now.Add(time.Minute))}, now, "site-a")
	if r.WinnerID != "r1" || !r.IAmWinner || len(r.DeleteIDs) != 0 {
		t.Fatalf("single Resolve = %#v", r)
	}
}

func TestResolveWinnerDeletesOthers(t *testing.T) {
	now := time.Unix(100, 0)
	// site-a is lowest among unexpired; site-a is me -> I delete the rest.
	records := []dr.LeaseRecord{
		rec("r2", "site-b", now.Add(time.Minute)),
		rec("r1", "site-a", now.Add(time.Minute)),
	}
	r := dr.Resolve(records, now, "site-a")
	if r.WinnerID != "r1" || !r.IAmWinner {
		t.Fatalf("winner = %#v, want r1/me", r)
	}
	if len(r.DeleteIDs) != 1 || r.DeleteIDs[0] != "r2" {
		t.Fatalf("DeleteIDs = %v, want [r2]", r.DeleteIDs)
	}
}

func TestResolveLoserDeletesOnlyOwn(t *testing.T) {
	now := time.Unix(100, 0)
	// site-a wins; I am site-b and also hold a stray duplicate r3.
	records := []dr.LeaseRecord{
		rec("r1", "site-a", now.Add(time.Minute)),
		rec("r2", "site-b", now.Add(time.Minute)),
		rec("r3", "site-b", now.Add(time.Minute)),
	}
	r := dr.Resolve(records, now, "site-b")
	if r.WinnerID != "r1" || r.IAmWinner {
		t.Fatalf("winner = %#v, want r1/not-me", r)
	}
	// Deletes only my own (r2, r3), never the winner r1.
	if len(r.DeleteIDs) != 2 {
		t.Fatalf("DeleteIDs = %v, want my two duplicates", r.DeleteIDs)
	}
	for _, id := range r.DeleteIDs {
		if id == "r1" {
			t.Fatalf("loser must not delete winner r1")
		}
	}
}

func TestResolvePrefersUnexpired(t *testing.T) {
	now := time.Unix(1000, 0)
	// site-a record is expired; site-b is live -> site-b wins despite higher site.
	records := []dr.LeaseRecord{
		rec("r1", "site-a", now.Add(-time.Minute)),
		rec("r2", "site-b", now.Add(time.Minute)),
	}
	r := dr.Resolve(records, now, "site-b")
	if r.WinnerID != "r2" || !r.IAmWinner {
		t.Fatalf("winner = %#v, want r2 (unexpired) /me", r)
	}
	if len(r.DeleteIDs) != 1 || r.DeleteIDs[0] != "r1" {
		t.Fatalf("DeleteIDs = %v, want [r1]", r.DeleteIDs)
	}
}

func TestResolveAllExpiredLowestSite(t *testing.T) {
	now := time.Unix(1000, 0)
	records := []dr.LeaseRecord{
		rec("r2", "site-b", now.Add(-time.Minute)),
		rec("r1", "site-a", now.Add(-2*time.Minute)),
	}
	r := dr.Resolve(records, now, "site-b")
	if r.WinnerID != "r1" || r.WinnerSite != "site-a" {
		t.Fatalf("winner = %#v, want r1/site-a", r)
	}
}

func TestResolveTieBreaksOnID(t *testing.T) {
	now := time.Unix(100, 0)
	records := []dr.LeaseRecord{
		rec("rB", "site-a", now.Add(time.Minute)),
		rec("rA", "site-a", now.Add(time.Minute)),
	}
	r := dr.Resolve(records, now, "site-a")
	if r.WinnerID != "rA" {
		t.Fatalf("winner = %q, want rA (lowest id on site tie)", r.WinnerID)
	}
}
