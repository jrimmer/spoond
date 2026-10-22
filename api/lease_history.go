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
//
// Locking: the write takes no lock and reads nothing back, so it runs
// the same whether or not the caller holds s.store.mu (markLost holds
// it, the release path has dropped it). The lease's start_ms and
// max_nap_secs stamps are the in-memory values the caller captured
// under the lock; the lease row is never re-read (the release may
// already have deleted it).

package api

import (
	"context"

	"github.com/jrimmer/spoond/v4/store"
)

// writeLeaseHistoryStamps records one ended lease in the lease_history
// table. outcome is "released" or "lost"; reason is the journal reason
// for the same op; peakMiB is the release's measured peak (0 = none,
// the only value a loss passes). startMS and maxNapSecs are the stamps
// the caller captured from the in-memory lease under the store lock. The
// row keeps the lease's Start (warm/cold/…) as start_kind. A lease
// released while suspended keeps that final sleep uncounted: only a
// completed resume raises maxNapSecs. A failed insert is logged
// (storeError) and never fails the release or the loss.
func (s *Service) writeLeaseHistoryStamps(l *Lease, outcome, reason string, peakMiB int, startMS, maxNapSecs int64) {
	if s == nil || s.db == nil || l == nil {
		return
	}
	if outcome != "released" && outcome != "lost" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	// A skip (false, nil) is normal: a lost lease the GC later released
	// keeps its lost row (INSERT OR IGNORE).
	if _, err := s.db.InsertLeaseHistory(ctx, store.LeaseHistoryRow{
		LeaseID:    l.ID,
		Owner:      l.Owner,
		Image:      l.Image,
		MemoryMB:   l.MemoryMB,
		CreatedAt:  l.CreatedAt,
		EndedAt:    s.now(),
		Outcome:    outcome,
		Reason:     reason,
		PeakMiB:    peakMiB,
		StartMS:    startMS,
		StartKind:  l.Start,
		MaxNapSecs: maxNapSecs,
	}); err != nil {
		s.storeError("insert_lease_history_"+outcome, l.ID, err)
	}
}
