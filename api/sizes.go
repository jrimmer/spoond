package api

import (
	"net/http"
	"runtime"

	"github.com/jrimmer/spoond/v3/sizes"
	"github.com/jrimmer/spoond/v3/store"
)

// DefaultMaxLeaseMemoryMB is MAX_LEASE_MEMORY_MB when unset: the most
// memory one lease's VM may be given (#151).
const DefaultMaxLeaseMemoryMB = 32768

// sizeLimitsOrDefault fills the size settings a ServiceConfig left
// unset: the default presets, the host's CPU count and
// DefaultMaxLeaseMemoryMB.
func sizeLimitsOrDefault(cfg ServiceConfig) ([]sizes.Preset, int, int) {
	presets, maxVCPU, maxMem := cfg.SizePresets, cfg.MaxLeaseVCPU, cfg.MaxLeaseMemoryMB
	if len(presets) == 0 {
		presets = sizes.Default()
	}
	if maxVCPU <= 0 {
		maxVCPU = runtime.NumCPU()
	}
	if maxMem <= 0 {
		maxMem = DefaultMaxLeaseMemoryMB
	}
	return presets, maxVCPU, maxMem
}

// effectiveMinMemoryMB is the smallest memory a lease of img may ask
// for: the manifest's min_memory_mb when set, else 512 MiB or the
// image's own memory when that is smaller (a small image's default size
// is always allowed). `spoond images build` applies the same rule to a
// warm size through sizes.EffectiveMinMemoryMB.
func effectiveMinMemoryMB(img store.ImageRow) int {
	return sizes.EffectiveMinMemoryMB(img.MinMemoryMB, img.MemoryMB)
}

// handleSizes lists the size presets and the per-lease limits (#151).
// Any authenticated caller may read it.
func (s *Server) handleSizes(w http.ResponseWriter, r *http.Request) {
	presets := s.svc.cfg.SizePresets
	if presets == nil {
		presets = []sizes.Preset{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"presets":       presets,
		"max_vcpu":      s.svc.cfg.MaxLeaseVCPU,
		"max_memory_mb": s.svc.cfg.MaxLeaseMemoryMB,
	})
}
