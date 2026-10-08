package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestImageUpsertGetList round-trips an ImageRow and verifies that a
// second upsert of the same name updates every column.
func TestImageUpsertGetList(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	first := ImageRow{
		Name: "py-base", TemplateID: "tpl0123456789abcdefgh",
		VCPU: 2, MemoryMB: 1024, DiskMB: 4096,
		StartCmd:  "/usr/local/bin/spoond-guest-init",
		ReadyCmd:  "test -f /run/spoond-guest-ready",
		Env:       map[string]string{"SPOOND_TOOL": "uv"},
		UpdatedAt: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC),
	}
	if err := db.UpsertImage(ctx, first); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("got %+v, want %+v", got, first)
	}

	// A new build: current build id, digest and updated_at move; the
	// template id is passed back unchanged.
	second := first
	second.CurrentBuildID = "b-build-2"
	second.Digest = "localhost:5000/py-base@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	second.UpdatedAt = first.UpdatedAt.Add(time.Minute)
	if err := db.UpsertImage(ctx, second); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err = db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if !reflect.DeepEqual(got, second) {
		t.Fatalf("got %+v, want %+v", got, second)
	}

	list, err := db.ListImages(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || !reflect.DeepEqual(list[0], second) {
		t.Fatalf("list = %+v, want [py-base]", list)
	}

	if _, err := db.GetImage(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v, want ErrNotFound", err)
	}
}

// TestImageEnvRoundTrip stores an image without an env map and reads it
// back empty, then with one.
func TestImageEnvRoundTrip(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	noEnv := ImageRow{
		Name: "go-base", TemplateID: "tpl0123456789abcdefgh", CurrentBuildID: "b-1",
		VCPU: 2, MemoryMB: 2048, DiskMB: 4096,
		UpdatedAt: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC),
	}
	if err := db.UpsertImage(ctx, noEnv); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := db.GetImage(ctx, "go-base")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Env) != 0 {
		t.Fatalf("nil env must read back empty, got %v", got.Env)
	}

	noEnv.Env = map[string]string{"GOPATH": "/go", "SPOOND_X": "1"}
	if err := db.UpsertImage(ctx, noEnv); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err = db.GetImage(ctx, "go-base")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if !reflect.DeepEqual(got.Env, noEnv.Env) {
		t.Fatalf("env = %v, want %v", got.Env, noEnv.Env)
	}
}

// TestBuildLifecycle inserts a build, moves it through ready with
// version fields, and lists children by parent.
func TestBuildLifecycle(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	root := BuildRow{
		BuildID: "b-root", Kind: "template", TemplateID: "tpl0123456789abcdefgh",
		Image: "py-base", State: "building",
		VCPU: 2, MemoryMB: 1024, DiskMB: 4096,
		CreatedAt: base, UpdatedAt: base,
	}
	if err := db.InsertBuild(ctx, root); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := db.GetBuild(ctx, "b-root")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != root {
		t.Fatalf("got %+v, want %+v", got, root)
	}

	// r non-nil: versions and sizes are copied with the state.
	if err := db.UpdateBuildState(ctx, "b-root", "ready", "", &BuildRow{
		KernelVersion: "vmlinux-6.1.158", FirecrackerVersion: "v1.14-0.2.0",
		EnvdVersion: "0.1.40", DiskMB: 5000, SizeBytes: 12345,
	}); err != nil {
		t.Fatalf("update ready: %v", err)
	}
	got, err = db.GetBuild(ctx, "b-root")
	if err != nil {
		t.Fatalf("get after ready: %v", err)
	}
	if got.State != "ready" || got.Error != "" ||
		got.KernelVersion != "vmlinux-6.1.158" ||
		got.FirecrackerVersion != "v1.14-0.2.0" ||
		got.EnvdVersion != "0.1.40" ||
		got.DiskMB != 5000 || got.SizeBytes != 12345 {
		t.Fatalf("after ready = %+v", got)
	}

	// r nil: only state and error move.
	child := BuildRow{
		BuildID: "b-child", Kind: "pause", TemplateID: root.TemplateID,
		Image: root.Image, ParentBuildID: root.BuildID,
		SourceSandboxID: "i0123456789abcdefghijklmnop", State: "building",
		CreatedAt: base.Add(time.Second), UpdatedAt: base.Add(time.Second),
	}
	if err := db.InsertBuild(ctx, child); err != nil {
		t.Fatalf("insert child: %v", err)
	}
	if err := db.UpdateBuildState(ctx, "b-child", "failed", "boom", nil); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	got, err = db.GetBuild(ctx, "b-child")
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if got.State != "failed" || got.Error != "boom" ||
		got.KernelVersion != "" || got.SizeBytes != 0 {
		t.Fatalf("child after fail = %+v", got)
	}

	kids, err := db.ChildBuilds(ctx, "b-root")
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	if len(kids) != 1 || kids[0].BuildID != "b-child" {
		t.Fatalf("children = %+v", kids)
	}
	if _, err := db.GetBuild(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v, want ErrNotFound", err)
	}
}

// TestSandboxUpsertTwice upserts the same sandbox id twice (resume
// reuses the id) and reads back the latest row.
func TestSandboxUpsertTwice(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	start := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	first := SandboxRow{
		SandboxID: "i0123456789abcdefghijklmnop", LeaseID: "lease-1",
		BuildID: "b-root", ExecutionID: "exec-1", HostIP: "10.11.0.2",
		VCPU: 2, MemoryMB: 1024, StartedAt: start, EndAt: start.Add(time.Hour),
	}
	if err := db.UpsertSandbox(ctx, first); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	second := first
	second.LeaseID = "lease-2"
	second.ExecutionID = "exec-2"
	second.HostIP = "10.11.0.3"
	second.StartedAt = start.Add(time.Minute)
	if err := db.UpsertSandbox(ctx, second); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err := db.GetSandbox(ctx, first.SandboxID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != second {
		t.Fatalf("got %+v, want %+v", got, second)
	}
	byLease, err := db.GetSandboxByLease(ctx, "lease-2")
	if err != nil {
		t.Fatalf("get by lease: %v", err)
	}
	if byLease != second {
		t.Fatalf("by lease = %+v, want %+v", byLease, second)
	}
	list, err := db.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0] != second {
		t.Fatalf("list = %+v", list)
	}
	if _, err := db.GetSandboxByLease(ctx, "lease-9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get by missing lease = %v, want ErrNotFound", err)
	}
}

// TestSandboxPoolAndDelete covers the LeaseID "" (pool) row and that
// DeleteSandbox of an absent id returns nil.
func TestSandboxPoolAndDelete(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	pool := SandboxRow{
		SandboxID: "i0123456789abcdefghijklmnop", LeaseID: "",
		BuildID: "b-root", ExecutionID: "exec-pool",
		VCPU: 2, MemoryMB: 1024,
		StartedAt: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC),
		EndAt:     time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC),
	}
	if err := db.UpsertSandbox(ctx, pool); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := db.GetSandboxByLease(ctx, "")
	if err != nil {
		t.Fatalf("get pool by lease: %v", err)
	}
	if got != pool {
		t.Fatalf("pool = %+v, want %+v", got, pool)
	}

	if err := db.DeleteSandbox(ctx, pool.SandboxID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deleting an absent id is not an error.
	if err := db.DeleteSandbox(ctx, pool.SandboxID); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if _, err := db.GetSandbox(ctx, pool.SandboxID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

// TestMarkStaleBuildingFailed: only a building row older than the
// cutoff is failed; a fresh building row, a ready row and a deleted row
// are left as they were.
func TestMarkStaleBuildingFailed(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	old := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	cutoff := old.Add(time.Hour)
	fresh := cutoff.Add(time.Minute)
	seed := func(id, state string, updated time.Time) {
		t.Helper()
		if err := db.InsertBuild(ctx, BuildRow{
			BuildID: id, Kind: "template", TemplateID: "t", Image: "py-base",
			State: state, CreatedAt: old, UpdatedAt: updated,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	seed("stale", "building", old)
	seed("active", "building", fresh)
	seed("done", "ready", old)
	seed("gone", "deleted", old)

	marked, err := db.MarkStaleBuildingFailed(ctx, cutoff, "build timed out")
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if len(marked) != 1 || marked[0].BuildID != "stale" {
		t.Fatalf("marked = %+v, want only stale", marked)
	}
	got, _ := db.GetBuild(ctx, "stale")
	if got.State != "failed" || !strings.Contains(got.Error, "timed out") {
		t.Fatalf("stale after mark = %+v", got)
	}
	for id, want := range map[string]string{"active": "building", "done": "ready", "gone": "deleted"} {
		b, err := db.GetBuild(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if b.State != want {
			t.Errorf("%s state = %q, want %q", id, b.State, want)
		}
	}

	// A second pass has nothing left to mark: the failed row is not
	// building any more.
	marked, err = db.MarkStaleBuildingFailed(ctx, cutoff, "build timed out")
	if err != nil || len(marked) != 0 {
		t.Fatalf("second mark = %+v, %v, want none", marked, err)
	}
}

// TestImageUses counts grants per image and survives an image upsert.
func TestImageUses(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, img := range []string{"py-base", "py-base", "go-base"} {
		if err := db.CountImageUse(ctx, img); err != nil {
			t.Fatalf("count %s: %v", img, err)
		}
	}
	if err := db.UpsertImage(ctx, ImageRow{Name: "py-base", TemplateID: "tpl", VCPU: 2, MemoryMB: 1024, DiskMB: 4096, UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := db.ImageUses(ctx)
	if err != nil {
		t.Fatalf("uses: %v", err)
	}
	if want := map[string]int{"py-base": 2, "go-base": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("uses = %v, want %v", got, want)
	}
}

// TestDeleteBuildsPermanently covers the L3 prune: a build row in state
// deleted older than the cutoff goes, with its build_refs rows; a recent
// deleted row and any non-deleted row stay.
func TestDeleteBuildsPermanently(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	insert := func(id, state string, updated time.Time) {
		t.Helper()
		if err := db.InsertBuild(ctx, BuildRow{
			BuildID: id, Kind: "pause", TemplateID: "tpl0123456789abcdefgh",
			Image: "py-base", State: state, CreatedAt: base, UpdatedAt: updated,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("b-old", "deleted", base)
	insert("b-recent", "deleted", base.Add(48*time.Hour))
	insert("b-ready", "ready", base)
	// A ref from the old deleted build and one from a live one: only the
	// deleted build's refs may go.
	if err := db.AddBuildRefs(ctx, "b-old", []string{"b-dep-old"}); err != nil {
		t.Fatalf("refs b-old: %v", err)
	}
	if err := db.AddBuildRefs(ctx, "b-ready", []string{"b-dep-live"}); err != nil {
		t.Fatalf("refs b-ready: %v", err)
	}

	cutoff := base.Add(24 * time.Hour)
	removed, err := db.DeleteBuildsPermanently(ctx, cutoff)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(removed) != 1 || removed[0] != "b-old" {
		t.Fatalf("removed = %v, want [b-old]", removed)
	}
	if _, err := db.GetBuild(ctx, "b-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("b-old survived: %v", err)
	}
	for _, id := range []string{"b-recent", "b-ready"} {
		if _, err := db.GetBuild(ctx, id); err != nil {
			t.Fatalf("%s wrongly pruned: %v", id, err)
		}
	}
	refs, err := db.ListBuildRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if _, ok := refs["b-old"]; ok {
		t.Errorf("deleted build's refs survived: %v", refs)
	}
	if len(refs["b-ready"]) != 1 || refs["b-ready"][0] != "b-dep-live" {
		t.Errorf("live build's refs = %v, want [b-dep-live]", refs["b-ready"])
	}
}
