package api

import (
	"context"
	"fmt"
	"time"
)

// Pause-chain measurement (spoond-p9j). A pause build's parent is the
// build the lease was running from, so a lease that suspends repeatedly
// accumulates a chain: pause → pause → … → template. The GC keeps every
// ancestor of a live lease's resume build (gc.go, keptBuilds), so a
// persistent lease that suspends daily holds every pause build until a
// cold restart (or the lease's release) breaks the chain.
//
// This is measurement only: the chain's depth and summed recorded bytes
// are reported in GET /api/leases/{id} and observed once per pause as
// the spoond_pause_chain_depth / spoond_pause_chain_bytes histograms.
// No compaction happens here.

// leaseChainHead is the build a lease's chain is measured from: the
// pause build it will resume from while suspended, otherwise the build
// it runs from, falling back to its newest checkpoint. Empty when the
// lease has no build (a fresh suspended lease before its first pause).
func leaseChainHead(l *Lease) string {
	if l.Suspended && l.ResumeBuildID != "" {
		return l.ResumeBuildID
	}
	if l.BuildID != "" {
		return l.BuildID
	}
	return l.LastCheckpointBuildID
}

// leaseChainStats returns the depth and recorded bytes of the chain the
// lease currently depends on, one row per ancestor. A failed catalog
// read is an error; a lease with no build reports 0, 0.
func (s *Service) leaseChainStats(ctx context.Context, l *Lease) (int, int64, error) {
	head := leaseChainHead(l)
	if head == "" {
		return 0, 0, nil
	}
	depth, bytes, err := s.db.BuildChain(ctx, head)
	if err != nil {
		return 0, 0, fmt.Errorf("build chain %s: %w", head, err)
	}
	return depth, bytes, nil
}

// pauseChainObserver returns the callback settleBuildSize runs once the
// pause build has settled: it records the chain the pause extends in the
// pause-chain histograms (spoond-p9j). Running it from the settle path,
// not right after insert, is what makes spoond_pause_chain_bytes include
// the newest pause build: its memfile lands (and its blocks commit on
// ZFS) after Pause returns, so a reading taken at insert time would miss
// most of the chain's largest snapshot. A failed read is logged and the
// observation skipped, never failing the pause. The callback runs on the
// settle goroutine that owns its build, so it adds no snapshot-slot
// contention.
func (s *Service) pauseChainObserver(buildID string) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.observePauseChain(ctx, buildID)
	}
}

// observePauseChain records a pause build's chain depth and bytes in the
// pause-chain histograms (spoond-p9j). The histogram labels are fixed,
// so the cardinality stays bounded no matter how many leases pause; one
// lease's exact figures are in GET /api/leases/{id}. A failed read is
// logged and the observation skipped, never failing the pause.
func (s *Service) observePauseChain(ctx context.Context, buildID string) {
	if s.metrics == nil {
		return
	}
	depth, bytes, err := s.db.BuildChain(ctx, buildID)
	if err != nil {
		s.log.Printf("pause-chain metrics: %v", err)
		return
	}
	if depth == 0 {
		// The pause build was deleted before the settle finished: the
		// lease was released during the pause, or the GC ran inside the
		// settle window. There is no chain to measure, so skip the
		// observation rather than recording a depth-0/bytes-0 sample in
		// the lowest histogram bucket.
		return
	}
	s.metrics.PauseChainDepth.Observe(float64(depth))
	s.metrics.PauseChainBytes.Observe(float64(bytes))
}
