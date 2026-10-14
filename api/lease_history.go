// Lease history (records A, owner-approved 2026-10-13): one durable row
// per ended lease, written beside the journal line at release and when a
// lease is marked lost. The dashboard's records panel reads the table;
// the journal rotates and the lease row is deleted when a lease ends, so
// the table is what survives. The same nits hold as for the journal
// lines: the owner is the identity id (never a token) and nothing
// secret-bearing rides along.
//
// The write is INSERT OR IGNORE and best effort: a row already recorded
// (a lost lease the GC later released) keeps its first outcome, and a
// failed write is logged and counted like any other store error without
// failing the release or the loss.

package api

import (
	"context"
	"errors"

	"github.com/jrimmer/spoond/v3/store"
)

// writeLeaseHistory records one ended lease in the lease_history table.
// outcome is "released" or "lost"; reason is the journal reason for the
// same op and peakMiB the release's measured peak (0 = none). The lease
// row's start_ms and max_nap_secs stamps ride along, and the row keeps
// the lease's Start (warm/cold/…) as start_kind. A lease released while
// suspended keeps that final sleep uncounted: ResumeNap's guard never
// counted it, so max_nap_secs holds only earlier completed naps.
//
// The call must run while the lease row still exists (the release
// deletes it under the store lock afterwards). A missing row — the loss
// raced a release, or the GC is releasing a lease whose row a crash
// already lost — is not an error: the history row is then written from
// the in-memory lease alone, with unknown start latency and nap. Any
// other read failure, and a failed insert, is logged (storeError) and
// never fails the release.
func (s *Service) writeLeaseHistory(l *Lease, outcome, reason string, peakMiB int) {
	var startMS, maxNapSecs int64
	if l != nil {
		s.store.mu.Lock()
		startMS, maxNapSecs = l.StartMS, l.MaxNapSecs
		s.store.mu.Unlock()
	}
	s.writeLeaseHistoryStamps(l, outcome, reason, peakMiB, startMS, maxNapSecs)
}

// writeLeaseHistoryStamps is writeLeaseHistory with the lease row's
// start_ms and max_nap_secs stamps named by the caller: the release
// captures them under the store lock in the same section that reads the
// peak, before it deletes the row a plain read would race. A lease
// released while suspended keeps its final sleep uncounted: ResumeNap's
// guard never counted it, so max_nap_secs holds only earlier completed
// naps.
func (s *Service) writeLeaseHistoryStamps(l *Lease, outcome, reason string, peakMiB int, startMS, maxNapSecs int64) {
	if s == nil || s.db == nil || l == nil {
		return
	}
	if outcome != "released" && outcome != "lost" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	row, err := s.db.GetLease(ctx, l.ID)
	switch {
	case err == nil:
		// Prefer the row's own stamps when the caller did not name any:
		// the loss paths read before their save, but a caller that raced a
		// stamp write between its read and this one still sees fresh
		// values.
		if startMS == 0 {
			startMS = row.StartMS
		}
		if maxNapSecs == 0 {
			maxNapSecs = row.MaxNapSecs
		}
	case errors.Is(err, store.ErrNotFound):
		// The lease row is already gone (a loss raced its release): the
		// stamps it carried are unknown; everything else rides the
		// in-memory lease.
		row = store.LeaseRow{}
	default:
		s.storeError("get_lease_history_source", l.ID, err)
		return
	}
	inserted, err := s.db.InsertLeaseHistory(ctx, store.LeaseHistoryRow{
		LeaseID:    l.ID,
		Owner:      l.Owner,
		Image:      l.Image,
		MemoryMB:   l.MemoryMB,
		CreatedAt:  l.CreatedAt,
		EndedAt:    s.now(),
		Outcome:    outcome,
		Reason:     reason,
		PeakMiB:    peakMiB,
		StartMS:    row.StartMS,
		StartKind:  l.Start,
		MaxNapSecs: row.MaxNapSecs,
	})
	if err != nil {
		s.storeError("insert_lease_history_"+outcome, l.ID, err)
	}
	// A skip (false, nil) is normal: a lost lease the GC later released
	// keeps its lost row (INSERT OR IGNORE).
	_ = inserted
}
