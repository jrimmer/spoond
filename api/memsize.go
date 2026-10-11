package api

import (
	"math"

	"github.com/jrimmer/spoond/v3/sizes"
)

// Lease size advice from measured memory (#130 R1). Owners have been
// able to pick a lease size since 3.1 without seeing what a lease
// actually uses; the memory sampler (api/memsample.go) measures it, and
// these helpers turn the measurement into API fields: the latest and
// peak usage in whole MiB and, when the measurement is trusted enough,
// the name of the smallest preset that would still hold the lease.

// mibCeil renders a byte count as whole MiB, rounding up: a lease that
// used 123.4 MiB reports 124, never less than what it used.
func mibCeil(bytes float64) int {
	if bytes <= 0 {
		return 0
	}
	return int(math.Ceil(bytes / (1024 * 1024)))
}

// sizePresets returns the service's size presets (SIZE_PRESETS as
// resolved at construction). Used by the lease row's size_hint.
func (s *Service) sizePresets() []sizes.Preset { return s.cfg.SizePresets }

// leaseSizeHints computes the fields a lease row carries from its
// measured memory: memory_used_mb, memory_peak_mb and size_hint (see
// leaseMap). A lease with no measurement gets nothing back. presets is
// the resolved preset list the hint is chosen from.
func leaseSizeHints(m map[string]any, l *Lease, ms *memSampler, presets []sizes.Preset) {
	if ms == nil || !l.live() {
		return
	}
	if used, ok := ms.latest(l.ID); ok {
		m["memory_used_mb"] = mibCeil(used)
	}
	peak, ok := ms.peak(l.ID)
	if !ok {
		return
	}
	m["memory_peak_mb"] = mibCeil(peak)
	if name, ok := sizeHint(l, ms, presets); ok {
		m["size_hint"] = name
	}
}

// sizeHint names the smallest preset whose memory still holds the
// lease: memory_mb at least 1.25 × the measured peak. It answers a
// name only when the measurement is trusted enough (at least
// sizeHintSamples ticks of the lease's current VM) and the preset is
// strictly smaller than the lease's own memory_mb — a hint that would
// not save memory is no hint. A lease with no stamped size never gets
// one.
func sizeHint(l *Lease, ms *memSampler, presets []sizes.Preset) (string, bool) {
	if l.MemoryMB <= 0 || len(presets) == 0 {
		return "", false
	}
	e, ok := ms.entry(l.ID)
	if !ok || e.samples < sizeHintSamples {
		return "", false
	}
	// The margin in whole MiB: a preset's memory_mb holds the lease
	// when it is at least the peak × 1.25, rounded up to the MiB the
	// presets are counted in.
	want := mibCeil(e.peakBytes * sizeHintMargin)
	best, name := -1, ""
	for _, p := range presets {
		if p.MemoryMB >= want && p.MemoryMB < l.MemoryMB {
			if best < 0 || p.MemoryMB < best {
				best, name = p.MemoryMB, p.Name
			}
		}
	}
	if best < 0 {
		return "", false
	}
	return name, true
}
