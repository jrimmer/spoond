package api

import (
	"net/http"
	"testing"

	"github.com/jrimmer/spoond/v3/sizes"
)

// seedMemEntries records n fresh samples of usedBytes for lease id on
// sandbox sb, through the real record path.
func seedMemEntries(svc *Service, l *Lease, used []float64) {
	live := map[string]*Lease{l.ID: l}
	for _, u := range used {
		svc.memSamples.record(live, map[string]float64{l.SandboxID: u}, nil)
	}
}

// TestLeaseMapMemoryFields: a lease with measurements reports
// memory_used_mb and memory_peak_mb in whole MiB (ceil); one without
// reports neither.
func TestLeaseMapMemoryFields(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l := addSandboxedLease(svc, "meas", "consumer-a", "sb_m", 4096)
	seedMemEntries(svc, l, []float64{470 * 1024 * 1024, 560 * 1024 * 1024, 300 * 1024 * 1024})

	m := svc.leaseMap(l, svc.memSamples, 0, 0)
	if got := toF(t, m["memory_used_mb"]); got != 300 {
		t.Errorf("memory_used_mb = %v, want 300", got)
	}
	if got := toF(t, m["memory_peak_mb"]); got != 560 {
		t.Errorf("memory_peak_mb = %v, want 560 (max since VM start)", got)
	}

	// A lease with no measurement gets nothing.
	bare := addSandboxedLease(svc, "bare", "consumer-a", "sb_none", 4096)
	m2 := svc.leaseMap(bare, svc.memSamples, 0, 0)
	if _, ok := m2["memory_used_mb"]; ok {
		t.Errorf("memory_used_mb present without data: %v", m2["memory_used_mb"])
	}
	if _, ok := m2["memory_peak_mb"]; ok {
		t.Errorf("memory_peak_mb present without data: %v", m2["memory_peak_mb"])
	}
	if _, ok := m2["size_hint"]; ok {
		t.Errorf("size_hint present without data: %v", m2["size_hint"])
	}
}

// TestLeaseMapSizeHintRules: size_hint is the smallest preset whose
// memory is at least 1.25 × the peak, only with ≥ 10 samples and only
// strictly below the lease's own memory_mb.
func TestLeaseMapSizeHintRules(t *testing.T) {
	presets := sizes.Default() // small 2048, medium 4096, large 8192, xlarge 16384
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.SizePresets = presets

	// 10+ samples, peak 800 MiB: 1.25× = 1000 MiB → smallest preset ≥
	// 1000 and < 4096 is small (2048).
	lease := addSandboxedLease(svc, "hintable", "consumer-a", "sb_h", 4096)
	seedMemEntries(svc, lease, append(make([]float64, 10), 800*1024*1024))
	m := svc.leaseMap(lease, svc.memSamples, 0, 0)
	if got := m["size_hint"]; got != "small" {
		t.Fatalf("size_hint = %v, want small (1.25×800 MiB = 1000 ≤ 2048 < 4096)", got)
	}

	// Not enough samples: no hint.
	short := addSandboxedLease(svc, "short", "consumer-a", "sb_s", 4096)
	seedMemEntries(svc, short, make([]float64, 9))
	m2 := svc.leaseMap(short, svc.memSamples, 0, 0)
	if got, ok := m2["size_hint"]; ok {
		t.Fatalf("size_hint with 9 samples = %v, want omitted", got)
	}
	if got := toF(t, m2["memory_peak_mb"]); got != 0 {
		t.Fatalf("peak of the zero-value series rounds to %v, want 0", got)
	}

	// A peak needing the lease's own size: no smaller preset fits, no
	// hint.
	big := addSandboxedLease(svc, "big", "consumer-a", "sb_big", 4096)
	seedMemEntries(svc, big, make([]float64, 10))
	svc.memSamples.record(map[string]*Lease{big.ID: big},
		map[string]float64{big.SandboxID: 3600 * 1024 * 1024}, nil)
	if _, ok := svc.memSamples.peak(big.ID); !ok {
		t.Fatal("peak missing after record")
	}
	m3 := svc.leaseMap(big, svc.memSamples, 0, 0)
	if got, ok := m3["size_hint"]; ok {
		t.Fatalf("size_hint at 1.25×3600=4500 MiB = %v, want omitted (medium is larger than the lease)", got)
	}

	// A lease without a stamped size is never hinted.
	unknown := addSandboxedLease(svc, "unknown", "consumer-a", "sb_u", 0)
	seedMemEntries(svc, unknown, make([]float64, 10))
	svc.memSamples.record(map[string]*Lease{unknown.ID: unknown},
		map[string]float64{unknown.SandboxID: 100 * 1024 * 1024}, nil)
	m4 := svc.leaseMap(unknown, svc.memSamples, 0, 0)
	if got, ok := m4["size_hint"]; ok {
		t.Fatalf("size_hint without a stamped size = %v, want omitted", got)
	}
}

// TestLeaseMapSizeHintSmallestPreset: among the fitting presets the
// smallest wins, and the margin counts: a peak whose 1.25× lands below
// the preset's memory does not fit.
func TestLeaseMapSizeHintSmallestPreset(t *testing.T) {
	presets := []sizes.Preset{
		{Name: "tiny", VCPU: 1, MemoryMB: 512},
		{Name: "mid", VCPU: 1, MemoryMB: 1024},
		{Name: "roomy", VCPU: 2, MemoryMB: 4096},
	}
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.SizePresets = presets
	// Peak 500 MiB → 1.25× = 625 → tiny (512) does not fit, mid does;
	// the lease runs at 4096 so the hint saves memory.
	lease := addSandboxedLease(svc, "pick", "consumer-a", "sb_p", 4096)
	seedMemEntries(svc, lease, make([]float64, 10))
	svc.memSamples.record(map[string]*Lease{lease.ID: lease},
		map[string]float64{lease.SandboxID: 500 * 1024 * 1024}, nil)
	m := svc.leaseMap(lease, svc.memSamples, 0, 0)
	if got := m["size_hint"]; got != "mid" {
		t.Fatalf("size_hint = %v, want mid (tiny's 512 < 625)", got)
	}

	// Exactly at the margin fits: peak 409.6 MiB × 1.25 = 512 → tiny.
	exact := addSandboxedLease(svc, "exact", "consumer-a", "sb_e", 4096)
	seedMemEntries(svc, exact, make([]float64, 10))
	svc.memSamples.record(map[string]*Lease{exact.ID: exact},
		map[string]float64{exact.SandboxID: 4096 * 1024 * 1024 / 10}, nil)
	m2 := svc.leaseMap(exact, svc.memSamples, 0, 0)
	if got := m2["size_hint"]; got != "tiny" {
		t.Fatalf("size_hint = %v, want tiny (1.25×409.6 = 512)", got)
	}
}

// TestLeaseSizeHintHTTP: the fields ride the real GET routes (list and
// detail) for the lease's owner.
func TestLeaseSizeHintHTTP(t *testing.T) {
	srv, svc, _, _ := newFairShareServer(t)
	h := srv.Handler()
	rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"hint-user","fingerprints":["SHA256:fp-h"],"token":"hint-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	uid2 := body["user"].(map[string]any)["id"].(string)
	tok := "hint-tok"

	lease := addSandboxedLease(svc, "hint-http", uid2, "sb_http", 4096)
	seedMemEntries(svc, lease, make([]float64, 10))
	svc.memSamples.record(map[string]*Lease{lease.ID: lease},
		map[string]float64{lease.SandboxID: 700 * 1024 * 1024}, nil)

	rec2, got := doUsersReq(t, h, "GET", "/api/leases/hint-http", tok, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("get lease: %d %s", rec2.Code, rec2.Body.String())
	}
	if m := got["memory_used_mb"]; toF(t, m) != 700 {
		t.Errorf("detail memory_used_mb = %v, want 700", m)
	}
	if m := got["memory_peak_mb"]; toF(t, m) != 700 {
		t.Errorf("detail memory_peak_mb = %v, want 700", m)
	}
	if got["size_hint"] != "small" {
		t.Errorf("detail size_hint = %v, want small", got["size_hint"])
	}
}

// toF asserts a number (JSON float64 or a built int from the map) and
// returns it as float64.
func toF(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		t.Fatalf("wanted a number, got %T (%v)", v, v)
		return 0
	}
}
