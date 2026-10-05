package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// keptMetricsTestServer builds a server whose checkpoints leave real
// build files (so recorded sizes are nonzero) under a temp storage root.
func keptMetricsTestServer(t *testing.T) (*httptest.Server, *Service, *store.DB, *testSub) {
	t.Helper()
	ts, svc, db, sub := newTestServerWithService(t)
	root := t.TempDir()
	svc.cfg.TemplateStoragePath = root
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		if _, err := writeBuildDir(filepath.Join(root, id), 4096, 8192); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		return id, substrate.BuildRefs{}, nil
	}
	return ts, svc, db, sub
}

// metricSample scrapes /metrics as token and returns the value of the
// gauge family's (only) sample.
func metricSample(t *testing.T, ts *httptest.Server, token, name string) (float64, bool) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	prefix := name + " "
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed sample line: %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err != nil {
			t.Fatalf("sample %q: %v", fields[1], err)
		}
		return v, true
	}
	return 0, false
}

// TestKeptMetricsGauges: a keep bumps spoond_kept_builds and
// spoond_kept_builds_bytes (#126); the release drops the pins and the
// next GC pass's gauge refresh returns the gauges to zero.
func TestKeptMetricsGauges(t *testing.T) {
	ts, svc, db, _ := keptMetricsTestServer(t)
	seedImage(t, db, "py-base", 2048)

	if n, ok := metricSample(t, ts, "token-a", "spoond_kept_builds_bytes"); !ok || n != 0 {
		t.Fatalf("fresh server: spoond_kept_builds_bytes = %v (present %v), want a 0 gauge (updateKeptMetrics runs on every scrape)", n, ok)
	}

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep: status %d (%v), want 200", resp.StatusCode, body)
	}
	buildID := body["build_id"].(string)

	n, ok := metricSample(t, ts, "token-a", "spoond_kept_builds")
	if !ok || n != 1 {
		t.Fatalf("spoond_kept_builds = %v (present %v), want 1", n, ok)
	}
	sz, ok := metricSample(t, ts, "token-a", "spoond_kept_builds_bytes")
	b, err := db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || sz != float64(b.SizeBytes) || b.SizeBytes == 0 {
		t.Fatalf("spoond_kept_builds_bytes = %v (present %v), want the build's recorded %d", sz, ok, b.SizeBytes)
	}

	// The scrape itself refreshes the kept gauges (collectServiceMetrics),
	// so the release is visible without the GC pass: the gauges read zero
	// right away, and stay zero after a pass.
	svc.release(context.Background(), svc.lookup("consumer-a", id))
	if n, _ := metricSample(t, ts, "token-a", "spoond_kept_builds"); n != 0 {
		t.Fatalf("spoond_kept_builds after the release = %v, want 0 (refreshed on scrape)", n)
	}
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n, _ := metricSample(t, ts, "token-a", "spoond_kept_builds"); n != 0 {
		t.Fatalf("spoond_kept_builds after release+pass = %v, want 0", n)
	}
	if n, _ := metricSample(t, ts, "token-a", "spoond_kept_builds_bytes"); n != 0 {
		t.Fatalf("spoond_kept_builds_bytes after release+pass = %v, want 0", n)
	}
}

// TestLeaseDetailListsKeptBuilds: GET /api/leases/{id} carries
// kept_builds — build_id, size_bytes and kept_at each, oldest first
// (#126). A lease with no keeps lists an empty array.
func TestLeaseDetailListsKeptBuilds(t *testing.T) {
	ts, _, db, _ := keptMetricsTestServer(t)
	seedImage(t, db, "py-base", 2048)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	// No keeps yet: an empty list, present.
	_, detail := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if kb, ok := detail["kept_builds"].([]any); !ok || len(kb) != 0 {
		t.Fatalf("kept_builds without keeps = %v, want an empty list", detail["kept_builds"])
	}

	var ids []string
	for i := 0; i < 2; i++ {
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
			map[string]any{"keep": true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keep %d: status %d (%v)", i+1, resp.StatusCode, body)
		}
		ids = append(ids, body["build_id"].(string))
	}
	_, detail = doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	raw, err := json.Marshal(detail["kept_builds"])
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		BuildID   string `json:"build_id"`
		SizeBytes int64  `json:"size_bytes"`
		KeptAt    string `json:"kept_at"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode kept_builds %s: %v", raw, err)
	}
	if len(rows) != 2 {
		t.Fatalf("kept_builds = %s, want 2 rows", raw)
	}
	for i, r := range rows {
		if r.BuildID != ids[i] {
			t.Errorf("kept_builds[%d].build_id = %s, want %s (kept order)", i, r.BuildID, ids[i])
		}
		if r.SizeBytes <= 0 {
			t.Errorf("kept_builds[%d].size_bytes = %d, want the recorded build size", i, r.SizeBytes)
		}
		if r.KeptAt == "" || !strings.Contains(r.KeptAt, "T") {
			t.Errorf("kept_builds[%d].kept_at = %q, want RFC 3339", i, r.KeptAt)
		}
	}

	// Another owner sees nothing: the detail route is owner-scoped and
	// kept_builds rides it.
	other := e2b.NewUUID()
	seedSnapshotBuild(t, db, other, "checkpoint", ids[0], "consumer-b", "ready")
	_, list := doReq(t, "GET", ts.URL+"/api/snapshots", "token-b", nil)
	if got, ok := snapshotSizeBytes(t, list, ids[0]); ok {
		t.Errorf("token-a's kept build %s visible to token-b at size %d", ids[0], got)
	}
}

// TestKeptBudgetIsPerOwner: the budget reads the caller's own identity
// row; a legacy consumer owner (no row) has no budget and a keep is
// never refused for one (#126).
func TestKeptBudgetIsPerOwner(t *testing.T) {
	ts, svc, db, _ := keptMetricsTestServer(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("solo", identity.KindPerson, []string{"SHA256:fp-s"}, "solo-tok")
	if err != nil {
		t.Fatal(err)
	}
	// A 1-byte budget: nothing fits, every keep is over budget.
	if err := ids.SetQuota(u.ID, 0, 0, 0, 0, 1); err != nil {
		t.Fatal(err)
	}

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "solo-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "solo-tok",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("keep under a 1-byte budget: status %d (%v), want 409", resp.StatusCode, body)
	}
	buildID := body["build_id"].(string)
	keeps, err := db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 0 {
		t.Fatalf("kept rows = %v (%v), want none (not pinned)", keeps, err)
	}
	if b, err := db.GetBuild(context.Background(), buildID); err != nil || b.State != "ready" {
		t.Fatalf("build %s = %v (%v), want a written, unpinned checkpoint", buildID, b, err)
	}

	// A retry without keep: 200, the checkpoint stands, still unpinned.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "solo-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry without keep: status %d (%v), want 200", resp.StatusCode, body)
	}
	keeps, err = db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 0 {
		t.Fatalf("kept rows after the plain retry = %v (%v), want none", keeps, err)
	}
}

// TestKeptDiskProbeFeedsNotify: the service's KeptDiskProbe reports the
// live leases' kept bytes and the snapshot disk's total, so the
// notifier's disk.kept check sees them (#126).
func TestKeptDiskProbeFeedsNotify(t *testing.T) {
	ts, svc, db, _ := keptMetricsTestServer(t)
	seedImage(t, db, "py-base", 2048)
	// A tiny "disk": 100 bytes total. One kept build (≥ 8192 allocated
	// bytes) is far past the 40 % warn level.
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 50, nil }

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep: status %d (%v)", resp.StatusCode, body)
	}

	probe := svc.KeptDiskProbe(t.TempDir())
	kept, total, err := probe()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if total != 100 {
		t.Fatalf("probe total = %d, want 100 (from diskCapacity)", total)
	}
	if kept == 0 {
		t.Fatal("probe kept = 0, want the kept build's recorded size")
	}
	b, err := db.GetBuild(context.Background(), body["build_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if kept != uint64(b.SizeBytes) {
		t.Fatalf("probe kept = %d, want the build's recorded %d", kept, b.SizeBytes)
	}
	// Released: the pins are gone, the probe reads zero.
	svc.release(context.Background(), svc.lookup("consumer-a", id))
	kept, _, err = probe()
	if err != nil {
		t.Fatalf("probe after release: %v", err)
	}
	if kept != 0 {
		t.Fatalf("probe kept after release = %d, want 0", kept)
	}
}
