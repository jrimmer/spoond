package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jrimmer/spoond/v3/store"
)

// Usage history (#140 U4): the sampler writes one usage_samples row per
// owner every 15 minutes from the fair-share snapshot, and the read
// route serves the caller's own samples oldest first. Rows older than
// the retention window are pruned by the same pass.

const (
	// defaultUsageSampleInterval is how often the sampler snapshots each
	// owner's usage. It is a field on Service so tests can shrink it.
	defaultUsageSampleInterval = 15 * time.Minute
	// usageHistoryRetention is how long a sample is kept: 7 days, the
	// cap the read route enforces.
	usageHistoryRetention = 7 * 24 * time.Hour
	// usageHistoryDefaultHours is the read window when the request does
	// not name one.
	usageHistoryDefaultHours = 24
	// usageHistoryMaxHours is the read window cap: 7 days, matching the
	// retention. A larger hours value is clamped, never refused.
	usageHistoryMaxHours = 168
)

// usageSampleJSON is one sample as GET /api/usage/history returns it.
// The timestamps are RFC 3339; the byte and MiB counts are named so a
// size is never mistaken for a lease count.
type usageSampleJSON struct {
	At            string `json:"at"`
	MemoryUsedMiB int    `json:"memory_used_mib"`
	DiskUsedBytes int64  `json:"disk_used_bytes"`
	Running       int    `json:"running"`
	Paused        int    `json:"paused"`
}

// runUsageHistoryLoop samples every owner's usage once at startup and
// then on the sample interval until ctx is cancelled.
func (s *Service) runUsageHistoryLoop(ctx context.Context) {
	s.runSweepStage(ctx, "sampleUsage", s.sampleUsage)
	t := time.NewTicker(s.usageSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runSweepStage(ctx, "sampleUsage", s.sampleUsage)
		}
	}
}

// sampleUsage writes one usage_samples row per owner from the current
// fair-share snapshot, then prunes rows older than the retention window.
// It fails soft: a read error is logged and the pass writes nothing
// rather than persisting invalid numbers, which would be served as real
// history for the full retention window. A snapshot whose capacityKnown
// is false had a failed read (memory or disk); it carries partial usage
// with zeroed buckets, so the pass is skipped and the next tick retries.
func (s *Service) sampleUsage(ctx context.Context) {
	// One snapshot carries both the counted memory and disk usage, per
	// owner. A snapshot with capacityKnown false means at least one read
	// failed and its zeroed buckets are not real, so nothing is written.
	at := s.now()
	snap := s.fairShares(ctx)
	if !snap.capacityKnown {
		s.log.Printf("usage history: sample skipped: capacity read failed")
		return
	}
	counts := s.countLeasesByOwner()
	for _, o := range snap.owners {
		c := counts[o.Owner]
		if err := s.db.InsertUsageSample(ctx, store.UsageSampleRow{
			Owner:         o.Owner,
			At:            at,
			MemoryUsedMiB: o.Memory.UsedMiB,
			DiskUsedBytes: o.Disk.UsedBytes,
			Running:       c.Running,
			Paused:        c.Paused,
		}); err != nil {
			s.log.Printf("usage history: sample %s: %v", o.Owner, err)
			return
		}
	}
	n, err := s.db.PruneUsageSamples(ctx, at.Add(-usageHistoryRetention))
	if err != nil {
		s.log.Printf("usage history: prune: %v", err)
		return
	}
	if n > 0 {
		s.log.Printf("usage history: pruned %d sample(s) older than %s", n, usageHistoryRetention)
	}
}

// countLeasesByOwner tallies the non-released leases per owner: running
// (or recovered) ones and suspended ones. It takes the store lock, but
// in its own critical section, so a lease that changes state between the
// fair-share snapshot and this read can make one tick's count and memory
// disagree by one lease; that is at most one cosmetic sample.
func (s *Service) countLeasesByOwner() map[string]LeaseCounts {
	out := map[string]LeaseCounts{}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if l.released || l.Owner == "" {
			continue
		}
		c := out[l.Owner]
		switch {
		case l.live():
			c.Running++
		case l.Suspended:
			c.Paused++
		}
		out[l.Owner] = c
	}
	return out
}

// handleUsageHistory is GET /api/usage/history?hours=N: the caller's own
// usage samples, oldest first, within the last N hours (default 24, cap
// 168). Self-scoped: a legacy consumer token also has an owner. An empty
// history answers an empty list, not a 404.
func (s *Server) handleUsageHistory(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	hours, err := parseUsageHistoryHours(r.URL.Query().Get("hours"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	since := s.svc.now().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.svc.db.ListUsageHistory(r.Context(), owner, since)
	if err != nil {
		s.svc.log.Printf("usage history: %v", err)
		writeError(w, http.StatusInternalServerError, "usage history unavailable")
		return
	}
	out := make([]usageSampleJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, usageSampleJSON{
			At:            formatRFC3339(row.At),
			MemoryUsedMiB: row.MemoryUsedMiB,
			DiskUsedBytes: row.DiskUsedBytes,
			Running:       row.Running,
			Paused:        row.Paused,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hours": hours, "samples": out})
}

// parseUsageHistoryHours reads the hours query parameter: an absent or
// empty value is the default 24; a non-integer or a value below 1 is a
// 400; a value above the cap is clamped to 168.
func parseUsageHistoryHours(raw string) (int, error) {
	if raw == "" {
		return usageHistoryDefaultHours, nil
	}
	hours, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid hours: %s", raw)
	}
	if hours < 1 {
		return 0, fmt.Errorf("hours must be >= 1")
	}
	if hours > usageHistoryMaxHours {
		hours = usageHistoryMaxHours
	}
	return hours, nil
}
