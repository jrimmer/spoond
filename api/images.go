package api

import (
	"context"
	"errors"

	"github.com/jrimmer/spoond/v2/store"
)

// ImageRegistry validates requested image names against the SQLite
// catalog (U08): an image is available when it has a row with a
// non-empty current_build_id.
type ImageRegistry struct {
	db *store.DB
}

// NewImageRegistry returns a registry backed by the image catalog.
func NewImageRegistry(db *store.DB) *ImageRegistry {
	return &ImageRegistry{db: db}
}

// Has reports whether name is a buildable image: a catalog row with a
// current build.
func (r *ImageRegistry) Has(ctx context.Context, name string) (bool, error) {
	img, err := r.db.GetImage(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return img.CurrentBuildID != "", nil
}
