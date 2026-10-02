package api

import (
	"context"
	"fmt"

	"github.com/jrimmer/spoond/v2/substrate"
)

// admit checks that the node can host a sandbox of memoryMB MiB: enough
// free hugepages (D12) and a healthy node. Otherwise it returns
// substrate.ErrCapacity, which the handlers map to HTTP 503
// {"error":"capacity: <reason>"}. Every refusal is counted (U11).
func (s *Service) admit(ctx context.Context, memoryMB int) error {
	if err := s.admitCapacity(ctx, memoryMB); err != nil {
		if s.metrics != nil {
			s.metrics.CapacityRej.Inc()
		}
		return err
	}
	return nil
}

func (s *Service) admitCapacity(ctx context.Context, memoryMB int) error {
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		return fmt.Errorf("node info: %w", err)
	}
	free := (info.HugepagesTotal - info.HugepagesUsed - info.HugepagesReserved) * info.HugepageSizeBytes
	if info.Status != "healthy" {
		return fmt.Errorf("%w: node status %s", substrate.ErrCapacity, info.Status)
	}
	need := uint64(memoryMB) * 1024 * 1024
	if free < need {
		return fmt.Errorf("%w: %d bytes of hugepage memory free, need %d", substrate.ErrCapacity, free, need)
	}
	return nil
}
