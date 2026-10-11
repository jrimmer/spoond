package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/sizes"
	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
	"github.com/jrimmer/spoond/v3/substrate/e2b"
)

// TestCreateAtSize (#151 Z4): a create picks its size by preset or by
// vcpu/memory_mb; a size with a warm build starts warm from it, any
// other cold; bad sizes are 400 invalid_size before anything starts.
func TestCreateAtSize(t *testing.T) {
	srv, h, tok, _ := newMemQuotaServer(t, map[string]int{"mid": 1024})
	svc := srv.svc
	svc.cfg.SizePresets = sizes.Default() // small 1/2048, medium 2/4096, large 4/8192, xlarge 8/16384
	svc.cfg.MaxLeaseVCPU = 8
	svc.cfg.MaxLeaseMemoryMB = 8192
	sub := svc.sub.(*testSub)

	// A warm "medium" build for mid, as spoond images build records it.
	ctx := context.Background()
	img, err := svc.db.GetImage(ctx, "mid")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	now := time.Now()
	warm := store.BuildRow{
		BuildID: e2b.NewUUID(), Kind: "template", TemplateID: img.TemplateID, Image: "mid",
		State: "ready", VCPU: 2, MemoryMB: 4096, DiskMB: 10240, CreatedAt: now, UpdatedAt: now,
	}
	if err := svc.db.InsertBuild(ctx, warm); err != nil {
		t.Fatalf("warm build: %v", err)
	}
	if err := svc.db.UpsertImageSize(ctx, store.ImageSizeRow{
		Image: "mid", Size: "medium", BuildID: warm.BuildID, VCPU: 2, MemoryMB: 4096, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("image size: %v", err)
	}

	create := func(body string) (int, map[string]any) {
		t.Helper()
		rec, b := createSandboxBody(t, h, tok, body)
		return rec.Code, b
	}
	ok := func(body string, vcpu, mem int, start string) {
		t.Helper()
		code, b := create(body)
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %v", body, code, b)
		}
		if b["vcpu"] != float64(vcpu) || b["memory_mb"] != float64(mem) || b["start"] != start {
			t.Fatalf("%s: lease %v/%v start %v, want %d/%d %s", body, b["vcpu"], b["memory_mb"], b["start"], vcpu, mem, start)
		}
		req := sub.LastCreate()
		if req.FilesystemBoot != (start == "cold") || int(req.VCPU) != vcpu || int(req.MemoryMB) != mem {
			t.Fatalf("%s: create request filesystem_boot %v %d/%d", body, req.FilesystemBoot, req.VCPU, req.MemoryMB)
		}
	}
	ok(`{"image":"mid","ttl":60}`, 2, 1024, "warm")
	ok(`{"image":"mid","ttl":60,"size":"large"}`, 4, 8192, "cold")
	ok(`{"image":"mid","ttl":60,"memory_mb":2048}`, 2, 2048, "cold")
	ok(`{"image":"mid","ttl":60,"memory_mib":2048}`, 2, 2048, "cold")
	ok(`{"image":"mid","ttl":60,"size":"medium"}`, 2, 4096, "warm")
	if got := sub.LastCreate().BuildID; got != warm.BuildID {
		t.Fatalf("medium started from build %s, want the warm build %s", got, warm.BuildID)
	}
	ok(`{"image":"mid","ttl":60,"vcpu":2,"memory_mb":4096}`, 2, 4096, "warm")

	// While a create starts from the warm build, the GC sees it as in
	// use (an image rebuild may drop its warm row mid-start).
	held := false
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		held = svc.startingBuild(warm.BuildID)
		return sub.Fake.Create(c, req)
	}
	ok(`{"image":"mid","ttl":60,"size":"medium"}`, 2, 4096, "warm")
	sub.createFn = nil
	if !held || svc.startingBuild(warm.BuildID) {
		t.Fatalf("warm build held during start = %v, still held after = %v; want true, false", held, svc.startingBuild(warm.BuildID))
	}

	// A clone of a sized lease is sized too: it keeps the size on restart.
	code, b := create(`{"image":"mid","ttl":600,"persistent":true,"memory_mb":1536}`)
	if code != http.StatusCreated {
		t.Fatalf("sized create: %d %v", code, b)
	}
	owner := ownerIDFor(t, h, tok)
	c, _, err := svc.clone(ctx, owner, b["id"].(string))
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if !c.Sized || c.MemoryMB != 1536 {
		t.Fatalf("clone sized %v memory %d, want a sized 1536 clone", c.Sized, c.MemoryMB)
	}

	bad := func(body, want string) {
		t.Helper()
		code, b := create(body)
		if code != http.StatusBadRequest || b["code"] != "invalid_size" || !strings.Contains(b["error"].(string), want) {
			t.Fatalf("%s: %d %v, want 400 invalid_size naming %q", body, code, b, want)
		}
	}
	bad(`{"image":"mid","ttl":60,"vcpu":3}`, "1 or an even number")
	bad(`{"image":"mid","ttl":60,"vcpu":16}`, "limit of 8")
	bad(`{"image":"mid","ttl":60,"memory_mb":16384}`, "limit of 8192")
	bad(`{"image":"mid","ttl":60,"memory_mb":256}`, "minimum of 512")
	bad(`{"image":"mid","ttl":60,"memory_mb":1025}`, "must be even")
	bad(`{"image":"mid","ttl":60,"size":"huge"}`, "presets: small, medium, large, xlarge")
	bad(`{"image":"mid","ttl":60,"size":"large","memory_mb":2048}`, "not both")
	bad(`{"image":"mid","ttl":60,"memory_mb":2048,"memory_mib":4096}`, "disagree")
}
