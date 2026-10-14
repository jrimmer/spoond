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
// Locking: the callers hold s.store.mu (both mark the lease ended under
// it), so these helpers must not take it — s.store.mu is a plain
// sync.Mutex and a nested Lock would deadlock. The lease's start_ms and
// max_nap_secs stamps are therefore named by the caller, read from the
// in-memory lease inside its own locked section; the only store calls
// here (GetLease, InsertLeaseHistory) run with the lock released, so a
// release can still race in between. The GetLease then answers
// ErrNotFound — the row is gone because the release deleted it — and the
// write goes ahead on the captured copies alone: that is exactly the
// race the captured copies exist for. A GetLease failure other than
// ErrNotFound skips the write: it is logged, and the next lease must not
// be blocked by this one.

package api

import (
	"context"
	"errors"

	"github.com/jrimmer/spoond/v3/store"
)

// writeLeaseHistoryStamps records one ended lease in the lease_history
// table. outcome is "released" or "lost"; reason is the journal reason
// for the same op; peakMiB is the release's measured peak (0 = none,
// the only value a loss passes). startMS and maxNapSecs are the stamps
// the caller captured from the in-memory lease under the store lock —
// the same section that read the release's peak, before it deletes the
// row — so a release that races this read of the row still writes the
// real values. The row keeps the lease's Start (warm/cold/…) as
// start_kind. A lease released while suspended keeps that final sleep
// uncounted: ResumeNap's guard never counted it, so maxNapSecs holds
// only earlier completed naps.
//
// The write must run while the lease row still exists (the release
// deletes it under the store lock afterwards). A missing row — this
// write raced the release that beat it, or the GC is releasing a lease
// whose row a crash already lost — is not an error: the captured stamps
// stand in for the row's. Any other read failure, and a failed insert,
// is logged (storeError) and never fails the release or the loss.
//
// Call with s.store.mu NOT held: the store calls below need the writer.
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
		// The row is still there and can carry fresher stamps than the
		// caller's copies (a caller that read before a stamp write
		// landed): prefer whichever names a value.
		if startMS == 0 {
			startMS = row.StartMS
		}
		if maxNapSecs == 0 {
			maxNapSecs = row.MaxNapSecs
		}
	case errors.Is(err, store.ErrNotFound):
		// The lease row is already gone (a loss raced its release): the
		// caller's captured copies are all there is.
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
		StartMS:    startMS,
		StartKind:  l.Start,
		MaxNapSecs: maxNapSecs,
	})
	if err != nil {
		s.storeError("insert_lease_history_"+outcome, l.ID, err)
	}
	// A skip (false, nil) is normal: a lost lease the GC later released
	// keeps its lost row (INSERT OR IGNORE).
	_ = inserted
}
