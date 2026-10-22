package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/sizes"
	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate/e2b"
)

// TestEffectiveMinMemoryMB: the manifest's min_memory_mb wins; unset,
// the minimum is 512 MiB, or the image's own memory when smaller.
// Mutation: drop the min() with the image's memory and the small
// image's row fails.
func TestEffectiveMinMemoryMB(t *testing.T) {
	for _, tc := range []struct {
		name          string
		memory, minMB int
		want          int
	}{
		{"set", 2048, 1024, 1024},
		{"unset", 2048, 0, 512},
		{"unset, small image", 256, 0, 256},
	} {
		img := store.ImageRow{Name: "py-base", VCPU: 2, MemoryMB: tc.memory, MinMemoryMB: tc.minMB}
		if got := effectiveMinMemoryMB(img); got != tc.want {
			t.Errorf("%s: effectiveMinMemoryMB = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSizesRoute: GET /api/sizes lists the configured presets and the
// per-lease limits to any authenticated caller; an unset config reads
// as the defaults. Mutation: drop sizeLimitsOrDefault from NewService
// and the default case reports no presets and zero limits.
func TestSizesRoute(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cfg         ServiceConfig
		wantPresets []sizes.Preset
		wantVCPU    int
		wantMem     int
	}{
		{name: "defaults", wantPresets: sizes.Default(), wantVCPU: runtime.NumCPU(), wantMem: DefaultMaxLeaseMemoryMB},
		{name: "configured",
			cfg:         ServiceConfig{SizePresets: []sizes.Preset{{Name: "tiny", VCPU: 1, MemoryMB: 512}}, MaxLeaseVCPU: 4, MaxLeaseMemoryMB: 8192},
			wantPresets: []sizes.Preset{{Name: "tiny", VCPU: 1, MemoryMB: 512}}, wantVCPU: 4, wantMem: 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.DefaultTTL, cfg.MaxTTL, cfg.ProxyURL = 60*time.Second, 10*time.Minute, "http://127.0.0.1:1"
			svc := NewService(newTestSub(), newTestDB(t), map[string]string{"token-a": "consumer-a"}, cfg)
			svc.log = log.New(io.Discard, "", 0)
			ts := httptest.NewServer(NewServer(svc, NewImageRegistry(svc.db)).Handler())
			t.Cleanup(ts.Close)

			if resp, _ := doReq(t, "GET", ts.URL+"/api/sizes", "", nil); resp.StatusCode != 401 {
				t.Fatalf("unauthenticated status %d, want 401", resp.StatusCode)
			}
			resp, body := doReq(t, "GET", ts.URL+"/api/sizes", "token-a", nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			var got []sizes.Preset
			for _, p := range body["presets"].([]any) {
				m := p.(map[string]any)
				got = append(got, sizes.Preset{Name: m["name"].(string), VCPU: int(m["vcpu"].(float64)), MemoryMB: int(m["memory_mb"].(float64))})
			}
			if !reflect.DeepEqual(got, tc.wantPresets) {
				t.Fatalf("presets = %+v, want %+v", got, tc.wantPresets)
			}
			if body["max_vcpu"] != float64(tc.wantVCPU) || body["max_memory_mb"] != float64(tc.wantMem) {
				t.Fatalf("limits = %v/%v, want %d/%d", body["max_vcpu"], body["max_memory_mb"], tc.wantVCPU, tc.wantMem)
			}
		})
	}
}

// TestImagesDetailSizes: GET /api/images?detail=1 reports each image's
// effective min_memory_mb and the presets it has a warm build for.
func TestImagesDetailSizes(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	seedImage(t, db, "py-base", 2048) // no min_memory_mb, no warm sizes
	goBase := seedImage(t, db, "go-base", 2048)
	goBase.MinMemoryMB = 1024
	if err := db.UpsertImage(ctx, goBase); err != nil {
		t.Fatalf("set min_memory_mb: %v", err)
	}
	for _, p := range []sizes.Preset{{Name: "medium", VCPU: 2, MemoryMB: 4096}, {Name: "large", VCPU: 4, MemoryMB: 8192}} {
		if err := db.UpsertImageSize(ctx, store.ImageSizeRow{
			Image: "go-base", Size: p.Name, BuildID: e2b.NewUUID(), VCPU: p.VCPU, MemoryMB: p.MemoryMB, UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed image size: %v", err)
		}
	}
	ts := httptest.NewServer(NewServer(svc, NewImageRegistry(db)).Handler())
	t.Cleanup(ts.Close)

	resp, body := doReq(t, "GET", ts.URL+"/api/images?detail=1", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := map[string]map[string]any{}
	for _, r := range body["images"].([]any) {
		m := r.(map[string]any)
		rows[m["name"].(string)] = m
	}
	if got := rows["go-base"]["min_memory_mb"]; got != float64(1024) {
		t.Errorf("go-base min_memory_mb = %v, want 1024", got)
	}
	if got := rows["go-base"]["warm_sizes"]; !reflect.DeepEqual(got, []any{"large", "medium"}) {
		t.Errorf("go-base warm_sizes = %v, want [large medium]", got)
	}
	if got := rows["py-base"]["min_memory_mb"]; got != float64(512) {
		t.Errorf("py-base min_memory_mb = %v, want 512", got)
	}
	if got := rows["py-base"]["warm_sizes"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("py-base warm_sizes = %v, want []", got)
	}
}
