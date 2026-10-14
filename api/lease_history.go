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
// failing the release.

package api

import (
	"context"

	"github.com/jrimmer/spoond/v3/store"
)

// writeLeaseHistory records one ended lease in the lease_history table.
// outcome is "released" or "lost"; reason is the journal reason for the
// same op and peakMiB the release's measured peak (0 = none). The lease
// row's start_ms and max_nap_secs stamps ride along, and the row keeps
// the lease's Start (warm/cold/…) as start_kind. A lease released while
// suspended keeps that final sleep uncounted: resumeNap never ran for
// it, so max_nap_secs holds only earlier completed naps. The store read
// and the insert use detached contexts: a client gone before its
// release's response still leaves its history row behind. A failed read
// or write is logged (storeError) and never fails the release.
func (s *Service) writeLeaseHistory(l *Lease, outcome, reason string, peakMiB int) {
	if s == nil || s.db == nil || l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	row, err := s.db.GetLease(ctx, l.ID)
	if err != nil {
		// Almost always store.ErrNotFound: the release already deleted
		// the lease row this record reads its stamps from. The history
		// row is then written from the in-memory lease alone, with
		// unknown start latency and nap.
		if outcome != "released" && outcome != "lost" {
			s.storeError("get_lease_history_source", l.ID, err)
			return
		}
		row = store.LeaseRow{}
	} else if row.ID == "" {
		row = store.LeaseRow{}
	}
	inserted, err := s.db.InsertLeaseHistory(ctx, store.LeaseHistoryRow{
		LeaseID:          l.ID,
		Owner:            l.Owner,
		Image:            l.Image,
		MemoryMB:         l.MemoryMB,
		CreatedAt:        l.CreatedAt,
		EndedAt:          s.now(),
		Outcome:          outcome,
		Reason:           reason,
		PeakMiB:          peakMiB,
		StartMS:          row.StartMS,
		StartKind:        l.Start,
		MaxNapSecs:       row.MaxNapSecs,
	})
	if err != nil {
		s.storeError("insert_lease_history_"+outcome, l.ID, err)
	}
	// A skip (false, nil) is normal: a lost lease the GC later released
	// keeps its lost row (INSERT OR IGNORE).
	_ = inserted
}
