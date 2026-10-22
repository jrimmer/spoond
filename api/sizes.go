package api

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/jrimmer/spoond/v4/sizes"
	"github.com/jrimmer/spoond/v4/store"
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

// errInvalidSize is a create's size request that cannot be served: an
// unknown preset, a malformed or out-of-limits vcpu or memory_mb, or a
// size on a named-snapshot start. The API answers it 400 invalid_size.
type errInvalidSize struct{ msg string }

func (e *errInvalidSize) Error() string { return e.msg }

func invalidSize(format string, a ...any) error {
	return &errInvalidSize{msg: fmt.Sprintf(format, a...)}
}

// requestedSize resolves a create's size fields against the image
// (#151 Z4): size names a preset; vcpu and memory_mb set either part
// and the image default fills the other. It returns 0, 0 for the image
// default, so a default-size create still uses the warm pool. Limits:
// vcpu 1 or even and at most MAX_LEASE_VCPU; memory_mb even, at least
// the image's minimum, at most MAX_LEASE_MEMORY_MB and the box's
// hugepage memory when known. The image default itself is always
// allowed.
func (s *Server) requestedSize(ctx context.Context, img store.ImageRow, size string, vcpu, memoryMB int) (int, int, error) {
	if size == "" && vcpu == 0 && memoryMB == 0 {
		return 0, 0, nil
	}
	if vcpu < 0 || memoryMB < 0 {
		return 0, 0, invalidSize("vcpu and memory_mb must be positive")
	}
	presets, maxVCPU, maxMem := sizeLimitsOrDefault(s.svc.cfg)
	if size != "" {
		if vcpu != 0 || memoryMB != 0 {
			return 0, 0, invalidSize("give either size or vcpu/memory_mb, not both")
		}
		p, ok := sizes.Lookup(presets, size)
		if !ok {
			names := make([]string, 0, len(presets))
			for _, p := range presets {
				names = append(names, p.Name)
			}
			return 0, 0, invalidSize("unknown size %q; presets: %s (GET /api/sizes)", size, strings.Join(names, ", "))
		}
		vcpu, memoryMB = p.VCPU, p.MemoryMB
	}
	if vcpu == 0 {
		vcpu = img.VCPU
	}
	if memoryMB == 0 {
		memoryMB = img.MemoryMB
	}
	if vcpu == img.VCPU && memoryMB == img.MemoryMB {
		return 0, 0, nil
	}
	if err := sizes.ValidVCPU(vcpu); err != nil {
		return 0, 0, invalidSize("%v", err)
	}
	if vcpu > maxVCPU {
		return 0, 0, invalidSize("vcpu %d is above this host's limit of %d", vcpu, maxVCPU)
	}
	if err := sizes.ValidMemoryMB(memoryMB); err != nil {
		return 0, 0, invalidSize("%v", err)
	}
	if min := effectiveMinMemoryMB(img); memoryMB < min {
		return 0, 0, invalidSize("memory_mb %d is below image %s's minimum of %d", memoryMB, img.Name, min)
	}
	if memoryMB > maxMem {
		return 0, 0, invalidSize("memory_mb %d is above this host's limit of %d", memoryMB, maxMem)
	}
	if snap := s.svc.fairShares(ctx); snap.capacityKnown && snap.memoryTotalMiB > 0 && memoryMB > snap.memoryTotalMiB {
		return 0, 0, invalidSize("memory_mb %d is more than this box's %d MiB of VM memory", memoryMB, snap.memoryTotalMiB)
	}
	return vcpu, memoryMB, nil
}
