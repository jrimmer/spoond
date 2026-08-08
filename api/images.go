package api

import (
	"context"
	"sync"
	"time"
)

// ImageRegistry lists available snapshot tags and validates requested
// tags against them. It caches the list briefly to avoid hammering
// forkd-controller on every create.
type ImageRegistry struct {
	forkd ForkdClient

	mu    sync.Mutex
	cache map[string]bool
	at    time.Time
	ttl   time.Duration
}

// NewImageRegistry returns a registry backed by forkd-controller with
// a short cache TTL.
func NewImageRegistry(fc ForkdClient, ttl time.Duration) *ImageRegistry {
	return &ImageRegistry{
		forkd: fc,
		cache: make(map[string]bool),
		ttl:   ttl,
	}
}

// refresh reloads the tag set from forkd-controller.
func (r *ImageRegistry) refresh(ctx context.Context) error {
	snaps, err := r.forkd.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]bool, len(snaps))
	for _, s := range snaps {
		r.cache[s.Tag] = true
	}
	r.at = time.Now()
	return nil
}

// Has reports whether tag is a known, bootable snapshot. It refreshes
// the cache when stale.
func (r *ImageRegistry) Has(ctx context.Context, tag string) (bool, error) {
	r.mu.Lock()
	stale := time.Since(r.at) > r.ttl
	r.mu.Unlock()
	if stale {
		if err := r.refresh(ctx); err != nil {
			return false, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cache[tag], nil
}

// Tags returns the current set of known tags.
func (r *ImageRegistry) Tags(ctx context.Context) ([]string, error) {
	if err := r.refresh(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.cache))
	for t := range r.cache {
		out = append(out, t)
	}
	return out, nil
}
