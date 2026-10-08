package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// counterValue reads a single counter's current value.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// gcOld is an updated_at comfortably older than the GC's one-hour age
// rule.
var gcOld = time.Now().Add(-2 * time.Hour)

// seedGCBuild inserts one build row with an old updated_at, so the GC's
// age rule never masks the kept-set logic under test.
func seedGCBuild(t *testing.T, db *store.DB, id, kind, parent, owner, state, templateID string) {
	t.Helper()
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: id, Kind: kind, TemplateID: templateID, Image: "py-base",
		ParentBuildID: parent, Owner: owner, State: state,
		CreatedAt: gcOld, UpdatedAt: gcOld,
	}); err != nil {
		t.Fatalf("seed build %s: %v", id, err)
	}
}

// seedGCImage writes an image row whose current build is root.
func seedGCImage(t *testing.T, db *store.DB, name, root string) {
	t.Helper()
	if err := db.UpsertImage(context.Background(), store.ImageRow{
		Name: name, TemplateID: e2b.NewTemplateID(), CurrentBuildID: root,
		VCPU: 2, MemoryMB: 2048, DiskMB: 10240, UpdatedAt: gcOld,
	}); err != nil {
		t.Fatalf("seed image %s: %v", name, err)
	}
}

// seedGCChain writes an image with a template → c1 → c2 checkpoint
// chain (all old and ready) and returns the three build ids.
func seedGCChain(t *testing.T, db *store.DB, image string) (root, c1, c2 string) {
	t.Helper()
	tid := e2b.NewTemplateID()
	root, c1, c2 = e2b.NewUUID(), e2b.NewUUID(), e2b.NewUUID()
	seedGCImage(t, db, image, root)
	seedGCBuild(t, db, root, "template", "", "consumer-a", "ready", tid)
	seedGCBuild(t, db, c1, "checkpoint", root, "consumer-a", "ready", tid)
	seedGCBuild(t, db, c2, "checkpoint", c1, "consumer-a", "ready", tid)
	return root, c1, c2
}

// seedGCLease writes a lease whose sandbox runs buildID.
func seedGCLease(t *testing.T, db *store.DB, leaseID, sandboxID, buildID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.UpsertLease(ctx, store.LeaseRow{
		ID: leaseID, Owner: "consumer-a", Image: "py-base", SandboxID: sandboxID,
		CreatedAt: gcOld, ExpiresAt: gcOld, LastActive: gcOld, State: "running",
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	if err := db.UpsertSandbox(ctx, store.SandboxRow{
		SandboxID: sandboxID, LeaseID: leaseID, BuildID: buildID, ExecutionID: "exec-" + sandboxID,
		VCPU: 2, MemoryMB: 2048, StartedAt: gcOld, EndAt: gcOld,
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
}

// gcTestService returns a service with metrics wired, GC logging in buf
// and an empty storage path dir, plus its fake substrate.
func gcTestService(t *testing.T) (*Service, *bytes.Buffer, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.SetMetrics(metrics.NewBackendMetrics())
	buf := &bytes.Buffer{}
	svc.log = log.New(buf, "", 0)
	return svc, buf, db, sub
}

// TestGCChainWithLeaseKeepsEverything: template → c1 → c2 with a lease
// running on c2 — nothing is deletable.
func TestGCChainWithLeaseKeepsEverything(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	root, c1, c2 := seedGCChain(t, db, "py-base")
	seedGCLease(t, db, "l1", "s1", c2)

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	for _, id := range []string{root, c1, c2} {
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State != "ready" {
			t.Errorf("build %s: state %q, want ready", id, b.State)
		}
	}
	if lines := buf.String(); strings.Contains(lines, "would delete") {
		t.Errorf("chain with a live lease logged deletions:\n%s", lines)
	}
}

// TestGCCandidatesAfterLeaseGone: with the lease (and its sandbox row)
// gone, the chain's tail becomes a candidate and the current template is
// not. Its parent is kept for as long as the tail's row is non-deleted
// (the parent protection), so the chain unwinds from the tail: one GC
// pass per link once nothing references the link below it.
func TestGCCandidatesAfterLeaseGone(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	root, c1, c2 := seedGCChain(t, db, "py-base")

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	lines := buf.String()
	want := fmt.Sprintf("gc: would delete %s kind=checkpoint image=py-base", c2)
	if !strings.Contains(lines, want) {
		t.Errorf("log lacks %q:\n%s", want, lines)
	}
	for _, id := range []string{root, c1} {
		if strings.Contains(lines, id) {
			t.Errorf("build %s logged as deletable while a non-deleted build references it:\n%s", id, lines)
		}
	}
	// Dry run: no state changes.
	b, err := db.GetBuild(context.Background(), c2)
	if err != nil {
		t.Fatalf("get build %s: %v", c2, err)
	}
	if b.State != "ready" {
		t.Errorf("build %s: dry run changed state to %q", c2, b.State)
	}

	// The next pass, after the tail is really deleted, reclaims the
	// parent — the deleted tail no longer protects it.
	if err := db.UpdateBuildState(context.Background(), c2, "deleted", "", nil); err != nil {
		t.Fatalf("mark tail deleted: %v", err)
	}
	buf.Reset()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if lines := buf.String(); !strings.Contains(lines, "would delete "+c1) {
		t.Errorf("log lacks the parent after the tail is deleted:\n%s", lines)
	}
}

// TestGCBuildingNeverDeleted: a fresh building build is a root and
// survives even with GC_DELETE=1 — it is inside the stale-building
// window, so the GC leaves it alone.
func TestGCBuildingNeverDeleted(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	seedGCChain(t, db, "py-base")
	building := e2b.NewUUID()
	fresh := time.Now()
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: building, Kind: "template", TemplateID: "tplb0123456789abcdef",
		Image: "py-base", Owner: "consumer-a", State: "building",
		CreatedAt: fresh, UpdatedAt: fresh,
	}); err != nil {
		t.Fatalf("seed building build: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	b, err := db.GetBuild(context.Background(), building)
	if err != nil {
		t.Fatalf("get building build: %v", err)
	}
	if b.State != "building" {
		t.Errorf("building build became %q", b.State)
	}
	if lines := buf.String(); strings.Contains(lines, building) {
		t.Errorf("building build logged as deletable:\n%s", lines)
	}
}

// TestGCStaleBuildingFailed: a build left in state building past twice
// the build timeout (a SIGKILL or reboot mid-build) is failed by the GC
// and logged; it is no longer a GC root. The row's ancestors then unwind
// as ordinary candidates in later passes.
func TestGCStaleBuildingFailed(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	svc.cfg.BuildTimeout = 30 * time.Minute
	stale := e2b.NewUUID()
	// Dated past 2x the configured timeout (and comfortably past the
	// one-hour gcAge) but not a full day, so the age arithmetic matters.
	old := time.Now().Add(-2 * time.Hour)
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: stale, Kind: "template", TemplateID: "tplb0123456789abcdef",
		Image: "py-base", Owner: "consumer-a", State: "building",
		CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatalf("seed stale build: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	b, err := db.GetBuild(context.Background(), stale)
	if err != nil {
		t.Fatalf("get stale build: %v", err)
	}
	if b.State != "failed" {
		t.Fatalf("stale building build = %q, want failed", b.State)
	}
	if !strings.Contains(b.Error, "stale building") {
		t.Errorf("stale build error = %q", b.Error)
	}
	if !strings.Contains(buf.String(), "marked stale building build failed id="+stale) {
		t.Errorf("stale build not logged:\n%s", buf.String())
	}

	// A building row younger than 2x the timeout stays building.
	recent := e2b.NewUUID()
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: recent, Kind: "template", TemplateID: "tplb0123456789abcdef",
		Image: "py-base", Owner: "consumer-a", State: "building",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed recent build: %v", err)
	}
	buf.Reset()
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc 2: %v", err)
	}
	if got, _ := db.GetBuild(context.Background(), recent); got.State != "building" {
		t.Errorf("recent building build = %q, want building", got.State)
	}
	if strings.Contains(buf.String(), recent) {
		t.Errorf("recent building build was touched:\n%s", buf.String())
	}
}

// TestGCStaleBuildingEmitsEvent: failing a stale building row emits one
// lease-less gc event naming the build, because a template build has no
// owner and never appears in /api/snapshots, so the event is where its
// failure is visible. The failure is announced even in dry-run mode.
func TestGCStaleBuildingEmitsEvent(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	svc.cfg.BuildTimeout = 30 * time.Minute
	stale := e2b.NewUUID()
	old := time.Now().Add(-2 * time.Hour)
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: stale, Kind: "template", TemplateID: "tplb0123456789abcdef",
		Image: "py-base", State: "building", CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatalf("seed stale build: %v", err)
	}

	all := svc.Subscribe(EventFilter{})
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	all.Close()

	var gc []LeaseEvent
	for _, ev := range collectEvents(all.C) {
		if ev.Type == LeaseGC {
			gc = append(gc, ev)
		}
	}
	if len(gc) != 1 {
		t.Fatalf("got %d gc events, want 1", len(gc))
	}
	if gc[0].LeaseID != "" || gc[0].Owner != "" {
		t.Fatalf("gc event has a lease or owner: %+v", gc[0])
	}
	if !strings.Contains(gc[0].Detail, "stale build") || !strings.Contains(gc[0].Detail, "build timed out") {
		t.Fatalf("gc detail = %q, want the stale build and reason", gc[0].Detail)
	}
	if !strings.Contains(gc[0].Detail, stale[:8]) {
		t.Fatalf("gc detail = %q, want the short build id %s", gc[0].Detail, stale[:8])
	}
}

// TestBuildMetricsGauge: updateBuildMetrics publishes the catalog's
// still-`building` template builds as spoond_builds_in_flight, the same
// count bakesRunning returns, so the dashboard's "builds busy" cell is
// no longer always 0.
func TestBuildMetricsGauge(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := db.InsertBuild(ctx, store.BuildRow{
			BuildID: e2b.NewUUID(), Kind: "template", TemplateID: "tplb0123456789abcdef",
			Image: "py-base", State: "building", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed building build: %v", err)
		}
	}
	// A ready template build and a building checkpoint must not count.
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID: e2b.NewUUID(), Kind: "template", TemplateID: "tplb0123456789abcdef",
		Image: "py-base", State: "ready", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed ready build: %v", err)
	}
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID: e2b.NewUUID(), Kind: "checkpoint", TemplateID: "tplb0123456789abcdef",
		Image: "py-base", State: "building", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed building checkpoint: %v", err)
	}

	svc.updateBuildMetrics(ctx)
	if got := gaugeValue(t, svc, "spoond_builds_in_flight"); got != 3 {
		t.Fatalf("spoond_builds_in_flight = %v, want 3", got)
	}

	// The same count the sweep guards on.
	n, err := svc.bakesRunning(ctx)
	if err != nil || n != 3 {
		t.Fatalf("bakesRunning = %d, %v, want 3", n, err)
	}

	// No metrics collector: the refresh is a no-op, not a panic.
	svc.metrics = nil
	svc.updateBuildMetrics(ctx)
}

// TestGCDeleteEnabledDeletesAndCounts: GC_DELETE=1 deletes candidates
// via the substrate, marks them deleted and bumps the counter; the
// leased chain and current templates stay. A chain unwinds from the
// tail — one link per pass — so the second image's two checkpoints take
// two passes.
func TestGCDeleteEnabledDeletesAndCounts(t *testing.T) {
	svc, buf, db, sub := gcTestService(t)
	t.Setenv("GC_DELETE", "1")
	root, c1, c2 := seedGCChain(t, db, "py-base")
	seedGCLease(t, db, "l1", "s1", c2)
	// A second image's chain with no lease: its checkpoints are
	// candidates, tail first.
	root2, d1, d2 := seedGCChain(t, db, "go-base")

	for i := 0; i < 2; i++ {
		if err := svc.gcOnce(context.Background()); err != nil {
			t.Fatalf("gc pass %d: %v", i, err)
		}
	}
	if n := calls(sub.Fake, "DeleteBuild"); n != 2 {
		t.Fatalf("DeleteBuild calls = %d, want 2\nlog:\n%s", n, buf.String())
	}
	for _, id := range []string{d1, d2} {
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State != "deleted" {
			t.Errorf("candidate %s: state %q, want deleted", id, b.State)
		}
	}
	for _, id := range []string{root, c1, c2, root2} {
		b, err := db.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatalf("get build %s: %v", id, err)
		}
		if b.State == "deleted" {
			t.Errorf("kept build %s was deleted", id)
		}
	}
	if n := counterValue(t, svc.metrics.GCDeleted.WithLabelValues("checkpoint")); n != 2 {
		t.Errorf("gc_deleted_total{checkpoint} = %v, want 2", n)
	}
}

// TestGCDryRunOnlyLogs: GC_DELETE unset only logs; the fake records no
// DeleteBuild call.
func TestGCDryRunOnlyLogs(t *testing.T) {
	svc, buf, db, sub := gcTestService(t)
	t.Setenv("GC_DELETE", "")
	_, _, c2 := seedGCChain(t, db, "py-base")

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n := calls(sub.Fake, "DeleteBuild"); n != 0 {
		t.Errorf("dry run recorded %d DeleteBuild call(s), want 0", n)
	}
	if !strings.Contains(buf.String(), "would delete "+c2) {
		t.Errorf("dry run did not log the candidate:\n%s", buf.String())
	}
}

// TestGCKeepsReferencedBuild: a build referenced only through
// build_refs by a kept build is not a candidate.
func TestGCKeepsReferencedBuild(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	root, _, _ := seedGCChain(t, db, "py-base")
	ref := e2b.NewUUID()
	seedGCBuild(t, db, ref, "checkpoint", "", "consumer-a", "ready", "tplr0123456789abcdef")
	if err := db.AddBuildRefs(context.Background(), root, []string{ref}); err != nil {
		t.Fatalf("add build refs: %v", err)
	}

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if lines := buf.String(); strings.Contains(lines, ref) {
		t.Errorf("referenced build logged as deletable:\n%s", lines)
	}
}

// TestGCSkipsWhileDraining: no GC pass runs while the admin drain is
// active (U10).
func TestGCSkipsWhileDraining(t *testing.T) {
	svc, buf, db, _ := gcTestService(t)
	_, _, _ = seedGCChain(t, db, "py-base")
	svc.draining.Store(true)

	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if lines := buf.String(); strings.Contains(lines, "would delete") {
		t.Errorf("GC ran while draining:\n%s", lines)
	}
}

// TestLeaseRowsExposeBuildIDs: lease list and detail rows carry
// build_id and resume_build_id (U11).
func TestLeaseRowsExposeBuildIDs(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %v", resp.StatusCode, body)
	}
	id := body["id"].(string)

	_, detail := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if detail["build_id"] == nil || detail["build_id"] == "" {
		t.Errorf("detail row lacks build_id: %v", detail)
	}

	_, list := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	rows := leaseRows(t, list)
	if len(rows) != 1 {
		t.Fatalf("expected 1 list row, got %d", len(rows))
	}
	if rows[0]["build_id"] == nil || rows[0]["build_id"] == "" {
		t.Errorf("list row lacks build_id: %v", rows[0])
	}

	resp, sbody := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("suspend status %d: %v", resp.StatusCode, sbody)
	}
	_, detail = doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	resume := detail["resume_build_id"]
	if resume == nil || resume == "" {
		t.Fatalf("suspended detail row lacks resume_build_id: %v", detail)
	}
	_, list = doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	rows = leaseRows(t, list)
	if rows[0]["resume_build_id"] != resume {
		t.Errorf("list resume_build_id = %v, want %v", rows[0]["resume_build_id"], resume)
	}
}

func leaseRows(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode list: %v", err)
	}
	var out struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("re-decode list: %v", err)
	}
	return out.Sandboxes
}
