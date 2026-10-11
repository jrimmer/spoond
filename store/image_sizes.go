package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ImageSizeRow is one row of image_sizes (#151 Z3): the current warm
// template build of one image at one size preset, and the shape it was
// built at. The image's default size is not here; it is the images row's
// current build.
type ImageSizeRow struct {
	Image, Size, BuildID string
	VCPU, MemoryMB       int
	UpdatedAt            time.Time
}

// UpsertImageSize records build as image's current warm build for its
// size preset, replacing the previous one.
func (db *DB) UpsertImageSize(ctx context.Context, r ImageSizeRow) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO image_sizes
		(image, size, build_id, vcpu, memory_mb, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(image, size) DO UPDATE SET
		  build_id = excluded.build_id,
		  vcpu = excluded.vcpu,
		  memory_mb = excluded.memory_mb,
		  updated_at = excluded.updated_at`,
		r.Image, r.Size, r.BuildID, r.VCPU, r.MemoryMB, formatTime(r.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert image size %s/%s: %w", r.Image, r.Size, err)
	}
	return nil
}

// ListImageSizes returns image's warm size rows, ordered by size name.
func (db *DB) ListImageSizes(ctx context.Context, image string) ([]ImageSizeRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT image, size, build_id, vcpu, memory_mb, updated_at
		FROM image_sizes WHERE image = ? ORDER BY size`, image)
	if err != nil {
		return nil, fmt.Errorf("store: list image sizes %s: %w", image, err)
	}
	defer rows.Close()
	var out []ImageSizeRow
	for rows.Next() {
		r, err := scanImageSize(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list image sizes %s: %w", image, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list image sizes %s: %w", image, err)
	}
	return out, nil
}

// ImageSizeFor returns image's warm size row built at exactly vcpu and
// memoryMB, or ErrNotFound. It matches the shape, not the preset name,
// so a preset whose definition changed since the build is not served at
// its old shape. When two presets share a shape the first by name wins.
func (db *DB) ImageSizeFor(ctx context.Context, image string, vcpu, memoryMB int) (ImageSizeRow, error) {
	row := db.r.QueryRowContext(ctx, `SELECT image, size, build_id, vcpu, memory_mb, updated_at
		FROM image_sizes WHERE image = ? AND vcpu = ? AND memory_mb = ?
		ORDER BY size LIMIT 1`, image, vcpu, memoryMB)
	r, err := scanImageSize(row.Scan)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ImageSizeRow{}, fmt.Errorf("store: image size %s %d/%d: %w", image, vcpu, memoryMB, err)
	}
	return r, err
}

// DeleteImageSizesExcept drops image's warm size rows whose preset is
// not in keep: a preset taken out of the manifest's warm_sizes stops
// being served warm, and its build becomes an ordinary GC candidate.
// Images are never deleted, so there is no whole-image delete.
func (db *DB) DeleteImageSizesExcept(ctx context.Context, image string, keep []string) error {
	rows, err := db.ListImageSizes(ctx, image)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, r := range rows {
		if kept[r.Size] {
			continue
		}
		if _, err := db.w.ExecContext(ctx, `DELETE FROM image_sizes WHERE image = ? AND size = ?`,
			image, r.Size); err != nil {
			return fmt.Errorf("store: delete image size %s/%s: %w", image, r.Size, err)
		}
	}
	return nil
}

// ImageSizeBuilds returns the build id of every warm size row: GC roots
// like each image's current build.
func (db *DB) ImageSizeBuilds(ctx context.Context) (map[string]bool, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT build_id FROM image_sizes`)
	if err != nil {
		return nil, fmt.Errorf("store: image size builds: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: image size builds: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: image size builds: %w", err)
	}
	return out, nil
}

func scanImageSize(scan func(dest ...any) error) (ImageSizeRow, error) {
	var r ImageSizeRow
	var updatedAt string
	err := scan(&r.Image, &r.Size, &r.BuildID, &r.VCPU, &r.MemoryMB, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ImageSizeRow{}, ErrNotFound
	}
	if err != nil {
		return ImageSizeRow{}, err
	}
	r.UpdatedAt = parseTime(updatedAt)
	return r, nil
}
