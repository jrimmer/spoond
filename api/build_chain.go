package api

import (
	"context"
	"fmt"

	"github.com/jrimmer/spoond/v2/store"
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

// buildChainStats walks the non-deleted parent chain of head and returns
// how many builds it holds and the total recorded size_bytes. The walk
// follows parent_build_id until a missing build, a deleted build, an
// empty parent or a cycle, exactly the closure the GC keeps: a deleted
// build's files are gone, so it and its ancestors are no longer part of
// the live chain. builds is the catalog keyed by build id.
func buildChainStats(builds map[string]store.BuildRow, head string) (depth int, bytes int64) {
	seen := make(map[string]bool)
	for id := head; id != "" && !seen[id]; {
		seen[id] = true
		b, ok := builds[id]
		if !ok || b.State == "deleted" {
			return depth, bytes
		}
		depth++
		bytes += b.SizeBytes
		id = b.ParentBuildID
	}
	return depth, bytes
}

// loadBuildMap reads the whole catalog into a map keyed by build id, so
// a chain walk is one query no matter how long the chain is.
func (s *Service) loadBuildMap(ctx context.Context) (map[string]store.BuildRow, error) {
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("list builds: %w", err)
	}
	m := make(map[string]store.BuildRow, len(builds))
	for _, b := range builds {
		m[b.BuildID] = b
	}
	return m, nil
}

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
// lease currently depends on. A failed catalog read is an error; a lease
// with no build reports 0, 0.
func (s *Service) leaseChainStats(ctx context.Context, l *Lease) (int, int64, error) {
	head := leaseChainHead(l)
	if head == "" {
		return 0, 0, nil
	}
	builds, err := s.loadBuildMap(ctx)
	if err != nil {
		return 0, 0, err
	}
	depth, bytes := buildChainStats(builds, head)
	return depth, bytes, nil
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
	builds, err := s.loadBuildMap(ctx)
	if err != nil {
		s.log.Printf("pause-chain metrics: %v", err)
		return
	}
	depth, bytes := buildChainStats(builds, buildID)
	s.metrics.PauseChainDepth.Observe(float64(depth))
	s.metrics.PauseChainBytes.Observe(float64(bytes))
}
