package api

import (
	"context"
)

// ImageRegistry validates requested image tags against forkd-controller
// and lists available tags. It uses the per-tag info endpoint for
// validation (reliable even when the list endpoint is empty) and the
// list endpoint for discovery, falling back to a configured known-tags
// set when the list is empty.
type ImageRegistry struct {
	forkd ForkdClient

	// known is an optional static set of tags to surface when the
	// controller's list endpoint returns nothing.
	known map[string]bool
}

// NewImageRegistry returns a registry backed by forkd-controller.
// knownTags is an optional static set surfaced when the controller list
// is empty.
func NewImageRegistry(fc ForkdClient, knownTags ...string) *ImageRegistry {
	known := make(map[string]bool, len(knownTags))
	for _, t := range knownTags {
		known[t] = true
	}
	return &ImageRegistry{
		forkd: fc,
		known: known,
	}
}

// Has reports whether tag is a known, bootable snapshot. It checks the
// per-tag info endpoint directly so validation is always current.
func (r *ImageRegistry) Has(ctx context.Context, tag string) (bool, error) {
	return r.forkd.SnapshotExists(ctx, tag)
}

// Tags returns the current set of known tags. It prefers the
// controller's list endpoint and falls back to the configured known set
// when the list is empty.
func (r *ImageRegistry) Tags(ctx context.Context) ([]string, error) {
	snaps, err := r.forkd.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(snaps)+len(r.known))
	for _, s := range snaps {
		seen[s.Tag] = true
	}
	for t := range r.known {
		seen[t] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out, nil
}
