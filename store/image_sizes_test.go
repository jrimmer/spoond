package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestImageSizes: a warm size row round-trips, a rebuild replaces it,
// ImageSizeFor matches the shape (not the name), and
// DeleteImageSizesExcept drops only the presets left out of warm_sizes.
func TestImageSizes(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)

	// go-base (default 2 vCPU / 2048 MiB) with warm_sizes [medium, large].
	medium := ImageSizeRow{Image: "go-base", Size: "medium", BuildID: "5f0c3e9a-4b1d-4c2e-9a77-1d2e3f405162",
		VCPU: 2, MemoryMB: 4096, UpdatedAt: at}
	large := ImageSizeRow{Image: "go-base", Size: "large", BuildID: "8a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
		VCPU: 4, MemoryMB: 8192, UpdatedAt: at}
	other := ImageSizeRow{Image: "py-base", Size: "large", BuildID: "0d9c8b7a-6f5e-4d3c-2b1a-0f9e8d7c6b5a",
		VCPU: 4, MemoryMB: 8192, UpdatedAt: at}
	for _, r := range []ImageSizeRow{medium, large, other} {
		if err := db.UpsertImageSize(ctx, r); err != nil {
			t.Fatalf("upsert %s/%s: %v", r.Image, r.Size, err)
		}
	}
	got, err := db.ListImageSizes(ctx, "go-base")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []ImageSizeRow{large, medium}; !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}

	// A rebuild of medium replaces its build.
	medium2 := medium
	medium2.BuildID = "c4b3a291-8f7e-4d6c-b5a4-93827160f5e4"
	medium2.UpdatedAt = at.Add(time.Hour)
	if err := db.UpsertImageSize(ctx, medium2); err != nil {
		t.Fatalf("upsert rebuild: %v", err)
	}
	r, err := db.ImageSizeFor(ctx, "go-base", 2, 4096)
	if err != nil || !reflect.DeepEqual(r, medium2) {
		t.Fatalf("ImageSizeFor 2/4096 = %+v, %v; want %+v", r, err, medium2)
	}
	// Shape, not name: 4/4096 is no warm build, nor is another image's.
	if _, err := db.ImageSizeFor(ctx, "go-base", 4, 4096); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ImageSizeFor 4/4096 err = %v, want ErrNotFound", err)
	}
	if _, err := db.ImageSizeFor(ctx, "dev-base", 4, 8192); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ImageSizeFor dev-base err = %v, want ErrNotFound", err)
	}

	builds, err := db.ImageSizeBuilds(ctx)
	if err != nil {
		t.Fatalf("builds: %v", err)
	}
	if want := map[string]bool{medium2.BuildID: true, large.BuildID: true, other.BuildID: true}; !reflect.DeepEqual(builds, want) {
		t.Fatalf("builds = %v, want %v", builds, want)
	}

	// warm_sizes now [large]: medium goes, py-base is untouched.
	if err := db.DeleteImageSizesExcept(ctx, "go-base", []string{"large"}); err != nil {
		t.Fatalf("delete except: %v", err)
	}
	got, err = db.ListImageSizes(ctx, "go-base")
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if want := []ImageSizeRow{large}; !reflect.DeepEqual(got, want) {
		t.Fatalf("list after delete = %+v, want %+v", got, want)
	}
	if got, _ := db.ListImageSizes(ctx, "py-base"); len(got) != 1 {
		t.Fatalf("py-base rows = %+v, want its large row kept", got)
	}
}
