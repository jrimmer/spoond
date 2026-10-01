package store

import (
	"context"
	"errors"
	"reflect"
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
