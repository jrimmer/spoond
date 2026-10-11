package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate/e2b"
)

// regrowImage moves an image to a new current build at another size, as
// a rebuild with new manifest sizes does.
func regrowImage(t *testing.T, db *store.DB, name string, vcpu, memoryMB, diskMB int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	img, err := db.GetImage(ctx, name)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	img.CurrentBuildID = e2b.NewUUID()
	img.VCPU, img.MemoryMB, img.DiskMB = vcpu, memoryMB, diskMB
	img.UpdatedAt = now
	if err := db.UpsertImage(ctx, img); err != nil {
		t.Fatalf("upsert image: %v", err)
	}
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID: img.CurrentBuildID, Kind: "template", TemplateID: img.TemplateID,
		Image: name, State: "ready", VCPU: vcpu, MemoryMB: memoryMB, DiskMB: diskMB,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert build: %v", err)
	}
}

// TestLeaseOwnsItsSize (#151 Z1): a lease is stamped with the size of
// the build its VM starts from, GET reports it, and a clone, a fork and
// a restore keep the lease's own size after its image is rebuilt bigger
// — the charge always matches the build the VM runs.
func TestLeaseOwnsItsSize(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 1024})
	svc := srv.svc
	ctx := context.Background()
	owner := ownerIDFor(t, h, tok)

	rec, body := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := body["id"].(string)
	rec, got := doUsersReq(t, h, "GET", "/api/leases/"+id, tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get lease: %d %s", rec.Code, rec.Body.String())
	}
	if got["vcpu"] != float64(2) || got["memory_mb"] != float64(1024) || got["disk_mb"] != float64(10240) {
		t.Fatalf("lease size = vcpu %v memory_mb %v disk_mb %v, want 2/1024/10240", got["vcpu"], got["memory_mb"], got["disk_mb"])
	}
	src := svc.lookup(uid, id)

	// The image is rebuilt at 8 vCPU / 4096 MiB / 20480 MiB disk.
	regrowImage(t, svc.db, "mid", 8, 4096, 20480)

	checkSize := func(what string, l *Lease) {
		t.Helper()
		svc.store.mu.Lock()
		v, m, d := l.VCPU, l.MemoryMB, l.DiskMB
		svc.store.mu.Unlock()
		if v != 2 || m != 1024 || d != 10240 {
			t.Fatalf("%s size = %d/%d/%d, want the source's 2/1024/10240", what, v, m, d)
		}
	}

	c, _, err := svc.clone(ctx, owner, id)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	checkSize("clone", c)

	kids, _, err := svc.fork(ctx, owner, id, 2, true, time.Hour, "", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	for _, k := range kids {
		checkSize("fork child", k)
	}

	// Restore from the source's checkpoint, suspended so restore
	// re-admits: the charge is the checkpoint build's, not the image's.
	b, err := svc.checkpointLease(ctx, src)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := svc.restore(ctx, src, b); err != nil {
		t.Fatalf("restore: %v", err)
	}
	checkSize("restored lease", src)

	// Every running lease is charged 1024: source, clone and two forks.
	if got := svc.usedMiB(owner); got != 4*1024 {
		t.Fatalf("used_mib = %d, want 4 x 1024", got)
	}
}

// TestSnapshotBuildsRecordTheLeaseSize (#151 Z1): a checkpoint and a
// pause build record the lease's own size, not its parent template
// build's — a lease cold-booted at another size snapshots at that size.
func TestSnapshotBuildsRecordTheLeaseSize(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 1024})
	svc := srv.svc
	ctx := context.Background()
	owner := ownerIDFor(t, h, tok)
	rec, body := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := body["id"].(string)
	l := svc.lookup(uid, id)
	// As if cold-booted at 6 vCPU / 3072 MiB off the 2/1024 template.
	svc.store.mu.Lock()
	l.VCPU, l.MemoryMB = 6, 3072
	svc.store.mu.Unlock()

	cp, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.VCPU != 6 || cp.MemoryMB != 3072 {
		t.Fatalf("checkpoint build size = %d/%d, want the lease's 6/3072", cp.VCPU, cp.MemoryMB)
	}
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	pauseID := l.ResumeBuildID
	svc.store.mu.Unlock()
	pb, err := svc.db.GetBuild(ctx, pauseID)
	if err != nil {
		t.Fatalf("pause build: %v", err)
	}
	if pb.VCPU != 6 || pb.MemoryMB != 3072 {
		t.Fatalf("pause build size = %d/%d, want the lease's 6/3072", pb.VCPU, pb.MemoryMB)
	}
}

// TestRestartKeepsLeaseSize (#151 Z4): after the image is rebuilt at
// another size, a warm restart of a non-persistent lease and a cold
// restart both boot the image's current build at the lease's own size —
// a cold boot (filesystem_boot), since no build of that size is current
// — and the lease reports start "cold".
func TestRestartKeepsLeaseSize(t *testing.T) {
	for _, mode := range []string{"warm", "cold"} {
		t.Run(mode, func(t *testing.T) {
			srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 1024})
			svc := srv.svc
			ctx := context.Background()
			owner := ownerIDFor(t, h, tok)
			rec, body := createSandboxBody(t, h, tok, `{"image":"mid","ttl":600}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			id := body["id"].(string)
			regrowImage(t, svc.db, "mid", 4, 2048, 10240)
			if _, err := svc.restart(ctx, owner, id, mode); err != nil {
				t.Fatalf("restart: %v", err)
			}
			l := svc.lookup(uid, id)
			svc.store.mu.Lock()
			v, m, start := l.VCPU, l.MemoryMB, l.Start
			svc.store.mu.Unlock()
			if v != 2 || m != 1024 || start != "cold" {
				t.Fatalf("restarted lease = %d/%d start %q, want its own 2/1024 started cold", v, m, start)
			}
			req := svc.sub.(*testSub).LastCreate()
			if !req.FilesystemBoot || req.VCPU != 2 || req.MemoryMB != 1024 {
				t.Fatalf("create request = filesystem_boot %v %d/%d, want a cold boot at 2/1024", req.FilesystemBoot, req.VCPU, req.MemoryMB)
			}
			if got := svc.usedMiB(owner); got != 1024 {
				t.Fatalf("used_mib = %d, want the lease's 1024", got)
			}
		})
	}
}

// TestSnapshotStartsNeverColdBoot (#151 Z4): only a template build may
// cold-boot. A restore from a checkpoint build starts with resume=false,
// yet must resume its memory snapshot at the build's size even when the
// lease's stamp says otherwise — a cold boot would lose the guest's
// memory.
func TestSnapshotStartsNeverColdBoot(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 1024})
	svc := srv.svc
	ctx := context.Background()
	rec, body := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	l := svc.lookup(uid, body["id"].(string))
	b, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	svc.store.mu.Lock()
	l.VCPU, l.MemoryMB = 6, 3072 // a stamp that disagrees with the build
	svc.store.mu.Unlock()
	if err := svc.restore(ctx, l, b); err != nil {
		t.Fatalf("restore: %v", err)
	}
	req := svc.sub.(*testSub).LastCreate()
	if req.FilesystemBoot || req.VCPU != 2 || req.MemoryMB != 1024 {
		t.Fatalf("restore create = filesystem_boot %v %d/%d, want a resume at the build's 2/1024", req.FilesystemBoot, req.VCPU, req.MemoryMB)
	}
}
