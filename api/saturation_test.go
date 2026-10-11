package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// writePSI writes /proc/pressure-shaped io and cpu files with the given
// 10 s averages, the format the kernel produces.
func writePSI(t *testing.T, dir string, ioFull, cpuSome float64) {
	t.Helper()
	// "some" is always at least "full"; 40 points more tells them apart.
	io := fmt.Sprintf("some avg10=%.2f avg60=0.00 avg300=0.00 total=1\nfull avg10=%.2f avg60=0.00 avg300=0.00 total=1\n", ioFull+40, ioFull)
	cpu := fmt.Sprintf("some avg10=%.2f avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", cpuSome)
	if err := os.WriteFile(filepath.Join(dir, "io"), []byte(io), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu"), []byte(cpu), 0o644); err != nil {
		t.Fatal(err)
	}
}

// expirePressure drops the gate's cached reading.
func expirePressure(svc *Service) {
	svc.pressure.mu.Lock()
	svc.pressure.at = time.Time{}
	svc.pressure.mu.Unlock()
}

// TestSaturationGate (#136 stage 2): a create, clone or fork waits with
// a capacity refusal (503 capacity_wait over HTTP) while host I/O full
// or CPU some pressure is at or over its target; a resume does not; an
// unreadable pressure file or a target of 0 leaves the gate open.
func TestSaturationGate(t *testing.T) {
	_, h, svc, sub, ids := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	one := uid(t, ids, "tok-1")
	ctx := context.Background()
	dir := t.TempDir()
	svc.pressure.root = dir
	svc.cfg.SaturationIOFullPct, svc.cfg.SaturationCPUSomePct = 20, 75

	writePSI(t, dir, 1.06, 3)
	src, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("calm create: %v", err)
	}

	refused := func(what string, err error, resource string) {
		t.Helper()
		var sat *errHostSaturated
		if !errors.As(err, &sat) || !errors.Is(err, substrate.ErrCapacity) || sat.resource != resource {
			t.Fatalf("%s = %v, want a capacity refusal for %s", what, err, resource)
		}
	}
	writePSI(t, dir, 25, 3)
	expirePressure(svc)
	_, err = svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour})
	refused("create under io pressure", err, "io")
	_, _, err = svc.clone(ctx, one, src.ID)
	refused("clone under io pressure", err, "io")
	_, _, err = svc.fork(ctx, one, src.ID, 1, false, time.Hour, "", "")
	refused("fork under io pressure", err, "io")
	// A bad request is still answered as itself, not as a wait.
	if _, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "no-such-image", ttl: time.Hour}); !errors.Is(err, errUnknownImage) {
		t.Fatalf("unknown image under pressure = %v, want errUnknownImage", err)
	}
	if _, _, err := svc.fork(ctx, one, src.ID, 0, false, time.Hour, "", ""); !errors.Is(err, errBadForkCount) {
		t.Fatalf("bad fork count under pressure = %v, want errBadForkCount", err)
	}
	if _, _, err := svc.clone(ctx, one, "no-such-lease"); !errors.Is(err, errNotFound) {
		t.Fatalf("clone of an unknown lease under pressure = %v, want errNotFound", err)
	}
	rec, body := createSandboxBody(t, h, "tok-1", `{"image":"mid","ttl":60}`)
	if rec.Code != http.StatusServiceUnavailable || body["code"] != "capacity_wait" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("HTTP create under io pressure: %d %v", rec.Code, body)
	}

	// Existing leases keep working: a suspend and resume pass.
	if _, err := svc.suspend(ctx, one, src.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, one, src.ID); err != nil {
		t.Fatalf("resume under io pressure: %v", err)
	}

	writePSI(t, dir, 1, 80)
	expirePressure(svc)
	_, err = svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour})
	refused("create under cpu pressure", err, "cpu")

	// A target of 0 turns that check off.
	svc.cfg.SaturationCPUSomePct = 0
	if _, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("cpu check off: %v", err)
	}
	svc.cfg.SaturationCPUSomePct = 75

	// Unreadable pressure: the gate stays open.
	svc.pressure.root = filepath.Join(dir, "missing")
	expirePressure(svc)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("unreadable pressure: %v", err)
	}
}

// TestSaturationQueues: a create with "wait" under pressure is queued
// with the saturation named in its queued event, and is admitted once
// the pressure falls.
func TestSaturationQueues(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	dir := t.TempDir()
	svc.pressure.root = dir
	svc.cfg.SaturationIOFullPct, svc.cfg.SaturationCPUSomePct = 20, 75
	svc.admitQ.tick = 20 * time.Millisecond
	events := svc.Subscribe(EventFilter{})
	defer events.Close()

	writePSI(t, dir, 30, 0)
	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":30}`)
	waitDepth(t, svc, 1)
	deadline := time.After(5 * time.Second)
	for queued := false; !queued; {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseQueued {
				if !strings.Contains(ev.Detail, "host saturated (io 30%)") {
					t.Fatalf("queued detail %q, want the saturation named", ev.Detail)
				}
				queued = true
			}
		case <-deadline:
			t.Fatal("no queued event")
		}
	}
	writePSI(t, dir, 2, 0)
	expirePressure(svc)
	svc.wakeAdmissionQueue()
	select {
	case r := <-res:
		if r.code != http.StatusCreated {
			t.Fatalf("queued create: %d %v", r.code, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("queued create was not admitted after the pressure fell")
	}
}

// TestSaturationPool: a create the warm pool can serve passes the gate
// (it boots nothing), and the pool is not refilled while the host is
// saturated.
func TestSaturationPool(t *testing.T) {
	_, _, svc, sub, ids := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	one := uid(t, ids, "tok-1")
	ctx := context.Background()
	dir := t.TempDir()
	svc.pressure.root = dir
	svc.cfg.SaturationIOFullPct, svc.cfg.SaturationCPUSomePct = 20, 75
	svc.cfg.PoolSize = 1
	poolLen := func() int {
		svc.store.mu.Lock()
		defer svc.store.mu.Unlock()
		return len(svc.store.pool["mid"])
	}

	writePSI(t, dir, 1, 1)
	svc.refillPool(ctx)
	if poolLen() != 1 {
		t.Fatalf("calm refill: pool %d, want 1", poolLen())
	}
	writePSI(t, dir, 30, 1)
	expirePressure(svc)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("pooled create under pressure: %v", err)
	}
	svc.refillPool(ctx)
	if poolLen() != 0 {
		t.Fatalf("saturated refill: pool %d, want 0", poolLen())
	}
	// With the pool empty, the next create waits.
	if _, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Hour}); !errors.Is(err, substrate.ErrCapacity) {
		t.Fatalf("unpooled create under pressure = %v, want a wait", err)
	}
}
