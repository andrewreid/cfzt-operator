package dr

import (
	"sort"
	"time"
)

// LeaseRecord pairs a parsed lease with the DNS record ID it was read from, so
// the resolver can name the records the caller should delete.
type LeaseRecord struct {
	ID    string
	Lease Lease
}

// Resolution is the deterministic verdict over the set of group-owned lease
// records found at a single lease name. Because both sites compute it from the
// same record set, they converge without coordination.
type Resolution struct {
	// WinnerID is the record that should survive ("" when records is empty).
	WinnerID string
	// WinnerSite is the site that owns the surviving record.
	WinnerSite string
	// IAmWinner reports whether the resolving site owns the winner.
	IAmWinner bool
	// DeleteIDs are the records the resolving site should delete this pass:
	// when it is the winner, every other record; otherwise only its own
	// duplicates. Deletes are idempotent (tolerate ErrNotFound).
	DeleteIDs []string
}

// Resolve picks the single surviving lease deterministically. Among unexpired
// records the lowest Site wins; if every record is expired, the lowest Site
// across all wins; ties on Site break on lowest record ID. The winner deletes
// all other records; a non-winner deletes only its own duplicates and leaves
// the winner's record untouched. Both rules converge the set to one record.
//
// Callers pass only records they have already confirmed are group-owned;
// records whose payload fails to parse must not be included (the caller treats
// an unparseable record at the lease name as fail-closed LeaseConflict).
func Resolve(records []LeaseRecord, now time.Time, mySite string) Resolution {
	if len(records) == 0 {
		return Resolution{}
	}

	candidates := make([]LeaseRecord, 0, len(records))
	for _, r := range records {
		if !r.Lease.Expired(now) {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		candidates = append(candidates, records...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Lease.Site != candidates[j].Lease.Site {
			return candidates[i].Lease.Site < candidates[j].Lease.Site
		}
		return candidates[i].ID < candidates[j].ID
	})
	winner := candidates[0]

	res := Resolution{
		WinnerID:   winner.ID,
		WinnerSite: winner.Lease.Site,
		IAmWinner:  winner.Lease.Site == mySite,
	}
	for _, r := range records {
		if r.ID == winner.ID {
			continue
		}
		if res.IAmWinner || r.Lease.Site == mySite {
			res.DeleteIDs = append(res.DeleteIDs, r.ID)
		}
	}
	return res
}
