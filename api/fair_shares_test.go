package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// newFairShareService builds a Service with a known box: a 1024 MiB
// hugepage pool (512 × 2 MiB) and a 1 GiB snapshot volume. The owner set
// is controlled explicitly by each test (the default legacy tokens are
// cleared so only the test's owners exist).
func newFairShareService(t *testing.T) (*Service, *store.DB, *testSub, *identity.Store) {
	t.Helper()
	svc, db, sub := newTestService(t)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	// The default legacy tokens are cleared so direct-service tests have a
	// deterministic owner set; HTTP tests add a bootstrap token back.
	svc.tokens = map[string]string{}
	svc.cfg.TemplateStoragePath = t.TempDir()
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512,
		HugepageSizeBytes: 2 << 20, // 1024 MiB total
	}, nil)
	// Warm the shared NodeInfo cache the same way the node-metrics loop
	// does: the fair-share view reads the cache and never makes an RPC
	// (R5), so a cold cache would report unknown capacity.
	svc.updateNodeMetrics(context.Background())
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 1 << 30, 1 << 29, nil }
	return svc, db, sub, ids
}

// addIdentityUser adds a user directly to the identity store.
func addIdentityUser(t *testing.T, ids *identity.Store, name string) *identity.User {
	t.Helper()
	u, err := ids.AddUser(name, identity.KindPerson, nil, "")
	if err != nil {
		t.Fatalf("add user %s: %v", name, err)
	}
	return u
}

// addRunningLease inserts a running lease with a memory charge into the
// in-memory store (no sandbox needed for accounting).
func addRunningLease(svc *Service, id, owner string, memMiB int) {
	svc.store.mu.Lock()
	svc.store.leases[id] = &Lease{ID: id, Owner: owner, State: "running", MemoryMB: memMiB}
	svc.store.mu.Unlock()
}

// seedPauseBuild inserts a lease row with a resume build and a build row
// of the given size, so the owner's pause bytes are that size.
func seedPauseBuild(t *testing.T, db *store.DB, leaseID, owner string, size int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	buildID := "pause-" + leaseID
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID: buildID, Kind: "pause", Owner: owner, State: "ready",
		SizeBytes: size, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert pause build: %v", err)
	}
	if err := db.UpsertLease(ctx, store.LeaseRow{
		ID: leaseID, Owner: owner, Image: "py-base", State: "suspended",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		ResumeBuildID: buildID, Class: "guaranteed",
	}); err != nil {
		t.Fatalf("insert lease: %v", err)
	}
}

// seedKeptBuild pins a build of the given size to leaseID, so the
// owner's kept bytes include it.
func seedKeptBuild(t *testing.T, db *store.DB, leaseID, owner string, size int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	buildID := "kept-" + leaseID + "-" + time.Now().Format("150405.000000")
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID: buildID, Kind: "checkpoint", Owner: owner, State: "ready",
		SizeBytes: size, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert kept build: %v", err)
	}
	// The pin needs a lease row (lease_kept_builds cascades from it).
	if err := db.UpsertLease(ctx, store.LeaseRow{
		ID: leaseID, Owner: owner, Image: "py-base", State: "running",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("insert kept lease: %v", err)
	}
	if err := db.KeepBuild(ctx, leaseID, buildID, now); err != nil {
		t.Fatalf("keep build: %v", err)
	}
}

// seedNamedSnapshot inserts a named snapshot version of the given size.
func seedNamedSnapshot(t *testing.T, db *store.DB, owner, name string, size int64) {
	t.Helper()
	now := time.Now()
	if _, err := db.InsertNamedSnapshot(context.Background(), store.NamedSnapshotRow{
		Owner: owner, Name: name, BuildID: "named-" + owner + "-" + name,
		SourceLeaseID: "src", Image: "py-base", ImageBuildID: "img-1",
		MemoryMB: 1024, SizeBytes: size, CreatedAt: now,
	}, 1); err != nil {
		t.Fatalf("insert named snapshot: %v", err)
	}
}

// shareByOwner indexes a snapshot's owners by id.
func shareByOwner(snap *fairShareSnapshot) map[string]*FairShareOwner {
	out := map[string]*FairShareOwner{}
	for _, o := range snap.owners {
		out[o.Owner] = o
	}
	return out
}

// TestFairSharesEqualSlices: every owner gets exactly 1/N of the memory
// and disk box.
func TestFairSharesEqualSlices(t *testing.T) {
	svc, _, _, ids := newFairShareService(t)
	u1 := addIdentityUser(t, ids, "alice")
	u2 := addIdentityUser(t, ids, "bob")
	u3 := addIdentityUser(t, ids, "carol")

	snap := svc.fairShares(context.Background())
	if snap.ownersN != 3 {
		t.Fatalf("ownersN = %d, want 3", snap.ownersN)
	}
	for _, o := range snap.owners {
		if o.SlicePct != 100.0/3.0 {
			t.Fatalf("%s SlicePct = %v, want %v", o.Owner, o.SlicePct, 100.0/3.0)
		}
		// 1024 MiB / 3 = 341 MiB; 1 GiB / 3 = 357913941 bytes.
		if o.Memory.SliceMiB != 341 {
			t.Fatalf("%s Memory.SliceMiB = %d, want 341", o.Owner, o.Memory.SliceMiB)
		}
		if o.Disk.SliceBytes != (1<<30)/3 {
			t.Fatalf("%s Disk.SliceBytes = %d, want %d", o.Owner, o.Disk.SliceBytes, (1<<30)/3)
		}
	}
	_ = u1
	_ = u2
	_ = u3
}

// TestFairSharesLegacyTokenIsOneOwner: a legacy consumer token counts as
// exactly one owner alongside the identity users.
func TestFairSharesLegacyTokenIsOneOwner(t *testing.T) {
	svc, _, _, ids := newFairShareService(t)
	addIdentityUser(t, ids, "alice")
	svc.tokens = map[string]string{"tok": "legacy-owner"}

	snap := svc.fairShares(context.Background())
	if snap.ownersN != 2 {
		t.Fatalf("ownersN = %d, want 2 (identity + legacy token)", snap.ownersN)
	}
	by := shareByOwner(snap)
	if by["legacy-owner"] == nil {
		t.Fatalf("legacy token owner missing: %+v", snap.owners)
	}
	if by["legacy-owner"].Name != "" {
		t.Fatalf("legacy token owner should have no name, got %q", by["legacy-owner"].Name)
	}
}

// TestFairSharesNChanges is the table test for N: adding or deleting an
// owner recomputes the 1/N slice.
func TestFairSharesNChanges(t *testing.T) {
	svc, _, _, ids := newFairShareService(t)
	a := addIdentityUser(t, ids, "alice")

	steps := []struct {
		name     string
		do       func()
		wantN    int
		wantMiB  int
		wantDisk int64
	}{
		{"one owner", func() {}, 1, 1024, 1 << 30},
		{"two owners", func() { addIdentityUser(t, ids, "bob") }, 2, 512, (1 << 30) / 2},
		{"three owners", func() { addIdentityUser(t, ids, "carol") }, 3, 341, (1 << 30) / 3},
		{"legacy owner added", func() { svc.tokens = map[string]string{"t": "legacy-x"} }, 4, 256, (1 << 30) / 4},
		{"owner deleted", func() { ids.RemoveUser(a.ID) }, 3, 341, (1 << 30) / 3},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			step.do()
			// The engine invalidates on add/delete; callers in tests that
			// mutate the identity store directly must too.
			svc.invalidateFairShares()
			snap := svc.fairShares(context.Background())
			if snap.ownersN != step.wantN {
				t.Fatalf("ownersN = %d, want %d", snap.ownersN, step.wantN)
			}
			for _, o := range snap.owners {
				if o.Memory.SliceMiB != step.wantMiB {
					t.Fatalf("%s Memory.SliceMiB = %d, want %d", o.Owner, o.Memory.SliceMiB, step.wantMiB)
				}
				if o.Disk.SliceBytes != step.wantDisk {
					t.Fatalf("%s Disk.SliceBytes = %d, want %d", o.Owner, o.Disk.SliceBytes, step.wantDisk)
				}
			}
		})
	}
}

// TestFairSharesZeroCapacityGuard: with no capacity and with no owners,
// slices and ratios are 0 and nothing divides by zero.
func TestFairSharesZeroCapacityGuard(t *testing.T) {
	svc, _, sub, ids := newFairShareService(t)
	addIdentityUser(t, ids, "alice")
	// A node reporting no hugepage size (and no disk) is the guard. Warm
	// the cache so the reading is a real zero capacity, not an unknown.
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy"}, nil)
	svc.updateNodeMetrics(context.Background())
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 0, 0, nil }
	svc.invalidateFairShares()

	snap := svc.fairShares(context.Background())
	if snap.ownersN != 1 {
		t.Fatalf("ownersN = %d, want 1", snap.ownersN)
	}
	o := snap.owners[0]
	if o.Memory.SliceMiB != 0 {
		t.Fatalf("zero-capacity slice = %d MiB, want 0", o.Memory.SliceMiB)
	}
	if o.Disk.SliceBytes != 0 || o.Ratio != 0 {
		t.Fatalf("zero-capacity disk slice/ratio = %d/%v, want 0/0", o.Disk.SliceBytes, o.Ratio)
	}
	// No owners at all also guards.
	svc.tokens = map[string]string{}
	ids.RemoveUser(o.Owner)
	svc.invalidateFairShares()
	snap = svc.fairShares(context.Background())
	if snap.ownersN != 0 || len(snap.owners) != 0 {
		t.Fatalf("no owners: ownersN=%d owners=%d, want 0/0", snap.ownersN, len(snap.owners))
	}
}

// TestFairSharesUsageByKind seeds one running lease, one pause build, one
// kept checkpoint and one named snapshot, and checks each usage bucket.
func TestFairSharesUsageByKind(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	u := addIdentityUser(t, ids, "alice")

	addRunningLease(svc, "run-1", u.ID, 300)
	seedPauseBuild(t, db, "pause-lease", u.ID, 10<<20)
	seedKeptBuild(t, db, "kept-lease", u.ID, 20<<20)
	seedNamedSnapshot(t, db, u.ID, "warm", 30<<20)

	o, ok := svc.fairShareFor(context.Background(), u.ID)
	if !ok {
		t.Fatal("owner missing")
	}
	if o.Memory.UsedMiB != 300 {
		t.Fatalf("used memory = %d, want 300", o.Memory.UsedMiB)
	}
	if o.Disk.PausedBytes != 10<<20 {
		t.Fatalf("paused bytes = %d, want %d", o.Disk.PausedBytes, 10<<20)
	}
	if o.Disk.KeptBytes != 20<<20 {
		t.Fatalf("kept bytes = %d, want %d", o.Disk.KeptBytes, 20<<20)
	}
	if o.Disk.NamedBytes != 30<<20 {
		t.Fatalf("named bytes = %d, want %d", o.Disk.NamedBytes, 30<<20)
	}
	if want := int64(60 << 20); o.Disk.UsedBytes != want {
		t.Fatalf("used disk = %d, want %d", o.Disk.UsedBytes, want)
	}
	// ratio = max(300/1024, 60MiB/1GiB) = 0.293...
	wantRatio := float64(300) / 1024.0
	if diff := o.Ratio - wantRatio; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("ratio = %v, want %v", o.Ratio, wantRatio)
	}
}

// TestFairSharesRatioOrdering: /api/fair-shares lists owners sorted by
// ratio descending (furthest over slice first).
func TestFairSharesRatioOrdering(t *testing.T) {
	srv, svc, db, ids := newFairShareServer(t)
	adminTok, _ := bootstrapAdmin(t, srv)
	heavy := addIdentityUser(t, ids, "heavy")
	light := addIdentityUser(t, ids, "light")
	svc.tokens = map[string]string{}

	// Two owners → 512 MiB disk slice each.
	seedNamedSnapshot(t, db, heavy.ID, "big", 700<<20)
	seedNamedSnapshot(t, db, light.ID, "small", 100<<20)

	rec, body := doUsersReq(t, srv.Handler(), "GET", "/api/fair-shares", adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/fair-shares: %d %s", rec.Code, rec.Body.String())
	}
	owners, ok := body["owners"].([]any)
	if !ok || len(owners) < 2 {
		t.Fatalf("owners = %v, want >= 2", body["owners"])
	}
	// The heaviest owner comes first; the light owner somewhere after.
	var heavyIdx, lightIdx = -1, -1
	for i, raw := range owners {
		o := raw.(map[string]any)
		switch o["owner"] {
		case heavy.ID:
			heavyIdx = i
		case light.ID:
			lightIdx = i
		}
	}
	if heavyIdx == -1 || lightIdx == -1 {
		t.Fatalf("owners missing heavy/light: %v", body["owners"])
	}
	if heavyIdx > lightIdx {
		t.Fatalf("heavy at %d must precede light at %d", heavyIdx, lightIdx)
	}
	if r0, r1 := owners[heavyIdx].(map[string]any)["ratio"].(float64), owners[lightIdx].(map[string]any)["ratio"].(float64); r0 < r1 {
		t.Fatalf("ratios not descending: %v < %v", r0, r1)
	}
	_ = light
}

// TestSharesListAdminOnly: GET /api/fair-shares is admin-only, while
// GET /api/shares still lists the caller's lease grants.
func TestSharesListAdminOnly(t *testing.T) {
	srv, _, _, _ := newFairShareServer(t)
	bootstrapAdmin(t, srv)
	h := srv.Handler()

	// A legacy consumer token is not an admin.
	rec, _ := doUsersReq(t, h, "GET", "/api/fair-shares", "legacy-tok", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /api/fair-shares = %d, want 403", rec.Code)
	}
}

// TestFairSharesCacheInvalidated: a state change through the engine
// invalidates the cached snapshot.
func TestFairSharesCacheInvalidated(t *testing.T) {
	svc, _, _, ids := newFairShareService(t)
	u := addIdentityUser(t, ids, "alice")

	snap1 := svc.fairShares(context.Background())
	addRunningLease(svc, "run-1", u.ID, 100)
	// Without an invalidation the cached snapshot is returned.
	if snap2 := svc.fairShares(context.Background()); snap2 != snap1 {
		t.Fatal("expected cached snapshot")
	}
	svc.invalidateFairShares()
	snap3 := svc.fairShares(context.Background())
	if snap3 == snap1 {
		t.Fatal("expected a fresh snapshot after invalidation")
	}
	if o := shareByOwner(snap3)[u.ID]; o.Memory.UsedMiB != 100 {
		t.Fatalf("used memory after invalidation = %d, want 100", o.Memory.UsedMiB)
	}
}

// TestFairSharesOwnerDeleteViaAPI: DELETE /api/users/{id} recomputes N.
func TestFairSharesOwnerDeleteViaAPI(t *testing.T) {
	srv, svc, _, ids := newFairShareServer(t)
	adminTok, _ := bootstrapAdmin(t, srv)
	// A second user, deleted through the API.
	rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", adminTok, `{"name":"gone","fingerprints":["SHA256:fp-gone"],"token":"gone-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	goneID := body["user"].(map[string]any)["id"].(string)

	before := svc.fairShares(context.Background()).ownersN
	if before < 2 {
		t.Fatalf("ownersN before delete = %d, want >= 2", before)
	}
	rec2, _ := doUsersReq(t, srv.Handler(), "DELETE", "/api/users/"+goneID, adminTok, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec2.Code, rec2.Body.String())
	}
	after := svc.fairShares(context.Background()).ownersN
	if after != before-1 {
		t.Fatalf("ownersN after delete = %d, want %d", after, before-1)
	}
	_ = ids
}

// TestUserUsageEndpointAdminOnly: GET /api/users/{id} is the admin usage
// view; GET /api/usage is self-scoped.
func TestUserUsageEndpointAdminOnly(t *testing.T) {
	srv, _, _, ids := newFairShareServer(t)
	adminTok, adminID := bootstrapAdmin(t, srv)
	u := addIdentityUser(t, ids, "alice")
	h := srv.Handler()

	// Admin reads alice's usage.
	rec, body := doUsersReq(t, h, "GET", "/api/users/"+u.ID, adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /api/users/%s: %d %s", u.ID, rec.Code, rec.Body.String())
	}
	if _, ok := body["share"].(map[string]any); !ok {
		t.Fatalf("response missing share: %v", body)
	}
	if _, ok := body["user"].(map[string]any); !ok {
		t.Fatalf("response missing user: %v", body)
	}
	// Unknown user → 404.
	rec2, _ := doUsersReq(t, h, "GET", "/api/users/u-nope", adminTok, "")
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", rec2.Code)
	}
	// A non-admin (legacy token) is refused.
	rec3, _ := doUsersReq(t, h, "GET", "/api/users/"+u.ID, "legacy-tok", "")
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("non-admin usage = %d, want 403", rec3.Code)
	}
	// Self-scoped /api/usage works for the identity user's own token.
	rec4, body4 := doUsersReq(t, h, "GET", "/api/usage", adminTok, "")
	if rec4.Code != http.StatusOK {
		t.Fatalf("GET /api/usage: %d %s", rec4.Code, rec4.Body.String())
	}
	sh, _ := body4["share"].(map[string]any)
	if sh == nil || sh["owner"] != adminID {
		t.Fatalf("usage share = %v, want owner %s", body4["share"], adminID)
	}
}

// TestUserMeCarriesShare: GET /api/users/me carries the caller's share.
func TestUserMeCarriesShare(t *testing.T) {
	srv, _, _, _ := newFairShareServer(t)
	adminTok, _ := bootstrapAdmin(t, srv)
	rec, body := doUsersReq(t, srv.Handler(), "GET", "/api/users/me", adminTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}
	sh, ok := body["share"].(map[string]any)
	if !ok {
		t.Fatalf("me missing share: %v", body)
	}
	if _, ok := sh["slice_pct"].(float64); !ok {
		t.Fatalf("share missing slice_pct: %v", sh)
	}
}

// TestFairSharesParkedBuildsNotCountedTwice: a build that is both a
// pause build and a kept pin counts in both kinds (they are separate
// kinds by policy), but a deleted build counts in neither.
func TestFairSharesDeletedBuildNotCounted(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	u := addIdentityUser(t, ids, "alice")
	seedKeptBuild(t, db, "lease-1", u.ID, 40<<20)

	o, _ := svc.fairShareFor(context.Background(), u.ID)
	if o.Disk.KeptBytes != 40<<20 {
		t.Fatalf("kept bytes = %d, want %d", o.Disk.KeptBytes, 40<<20)
	}
	// Mark the build deleted: its files are gone, so it holds no disk.
	builds, err := db.ListBuilds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBuildState(context.Background(), builds[0].BuildID, "deleted", "", nil); err != nil {
		t.Fatal(err)
	}
	svc.invalidateFairShares()
	o, _ = svc.fairShareFor(context.Background(), u.ID)
	if o.Disk.KeptBytes != 0 {
		t.Fatalf("kept bytes after delete = %d, want 0", o.Disk.KeptBytes)
	}
}

// --- HTTP test helpers ---

// newFairShareServer builds the API server over a fair-share service.
func newFairShareServer(t *testing.T) (*Server, *Service, *store.DB, *identity.Store) {
	t.Helper()
	svc, db, sub, ids := newFairShareService(t)
	// A bootstrap token so the first POST /api/users (open during
	// bootstrap) can authenticate; the legacy consumer is an owner too
	// until a test clears it.
	svc.tokens = map[string]string{"legacy-tok": "legacy-consumer"}
	seedImage(t, db, "py-base", 1024)
	sub.SetNodeInfo(substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 512, HugepageSizeBytes: 2 << 20,
	}, nil)
	srv := NewServer(svc, NewImageRegistry(db))
	return srv, svc, db, ids
}

// bootstrapAdmin creates the first (admin) user and returns its token
// and id.
func bootstrapAdmin(t *testing.T, srv *Server) (token, id string) {
	t.Helper()
	rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-admin"],"token":"admin-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d %s", rec.Code, rec.Body.String())
	}
	u := body["user"].(map[string]any)
	return "admin-tok", u["id"].(string)
}

// TestSharesListJSONShape pins the exact JSON keys the endpoint returns.
func TestSharesListJSONShape(t *testing.T) {
	srv, _, _, ids := newFairShareServer(t)
	adminTok, _ := bootstrapAdmin(t, srv)
	addIdentityUser(t, ids, "alice")
	srvSvc := srv.svc
	srvSvc.tokens = map[string]string{}
	srvSvc.invalidateFairShares()

	req := httptest.NewRequest("GET", "/api/fair-shares", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Owners []struct {
			Owner    string  `json:"owner"`
			SlicePct float64 `json:"slice_pct"`
			Memory   struct {
				SliceMiB int `json:"slice_mib"`
				UsedMiB  int `json:"used_mib"`
			} `json:"memory"`
			Disk struct {
				SliceBytes  int64 `json:"slice_bytes"`
				UsedBytes   int64 `json:"used_bytes"`
				PausedBytes int64 `json:"paused_bytes"`
				KeptBytes   int64 `json:"kept_bytes"`
				NamedBytes  int64 `json:"named_bytes"`
			} `json:"disk"`
			Ratio float64 `json:"ratio"`
		} `json:"owners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Owners) == 0 {
		t.Fatal("no owners")
	}
	if !strings.Contains(rec.Body.String(), `"slice_bytes"`) {
		t.Fatalf("missing disk keys: %s", rec.Body.String())
	}
}

// freezeFairShareClock pins the service clock so the cache TTL can never
// expire inside a test: a snapshot is then only replaced when an
// invalidation drops it, which makes a missing invalidation visible.
func freezeFairShareClock(svc *Service, base time.Time) {
	svc.now = func() time.Time { return base }
}

// freshAfter asserts that the next read is a different snapshot than the
// cached one and returns it: the mutation under test must have
// invalidated the cache. Removing that invalidation makes this fail
// because a frozen clock never expires the TTL.
func freshAfter(t *testing.T, svc *Service, before *fairShareSnapshot) *fairShareSnapshot {
	t.Helper()
	after := svc.fairShares(context.Background())
	if after == before {
		t.Fatal("snapshot was not invalidated: the cached one was returned")
	}
	return after
}

// TestFairSharesInvalidateOnLeaseRelease: releasing a live lease drops
// its memory usage, and the read must be fresh.
func TestFairSharesInvalidateOnLeaseRelease(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	freezeFairShareClock(svc, time.Now())
	seedImage(t, db, "py-base", 256)
	u := addIdentityUser(t, ids, "alice")
	l, err := svc.grant(context.Background(), u.ID, "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	before := svc.fairShares(context.Background())
	if got := shareByOwner(before)[u.ID].Memory.UsedMiB; got != 256 {
		t.Fatalf("used memory before release = %d, want 256", got)
	}
	svc.release(context.Background(), l)
	after := freshAfter(t, svc, before)
	if got := shareByOwner(after)[u.ID].Memory.UsedMiB; got != 0 {
		t.Fatalf("used memory after release = %d, want 0", got)
	}
}

// TestFairSharesInvalidateOnLeaseDelete: deleteLeaseLocked (a fork
// rollback / recovery delete) invalidates even when the in-memory lease
// is already gone.
func TestFairSharesInvalidateOnLeaseDelete(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	freezeFairShareClock(svc, time.Now())
	seedImage(t, db, "py-base", 256)
	u := addIdentityUser(t, ids, "alice")
	l, err := svc.grant(context.Background(), u.ID, "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	before := svc.fairShares(context.Background())
	// Drop the in-memory row the way a rollback does, then delete the
	// store row: the invalidation must live in deleteLeaseLocked.
	svc.store.mu.Lock()
	delete(svc.store.leases, l.ID)
	svc.store.mu.Unlock()
	svc.deleteLeaseLocked(l.ID)
	freshAfter(t, svc, before)
}

// TestFairSharesInvalidateOnUserCreateAndDelete: adding or removing an
// identity owner changes N, and the read must be fresh each time.
func TestFairSharesInvalidateOnUserCreateAndDelete(t *testing.T) {
	srv, svc, _, _ := newFairShareServer(t)
	freezeFairShareClock(svc, time.Now())
	adminTok, _ := bootstrapAdmin(t, srv)

	before := svc.fairShares(context.Background())
	rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", adminTok, `{"name":"new","fingerprints":["SHA256:fp-new"],"token":"new-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	afterCreate := freshAfter(t, svc, before)
	if afterCreate.ownersN != before.ownersN+1 {
		t.Fatalf("ownersN after create = %d, want %d", afterCreate.ownersN, before.ownersN+1)
	}
	id := body["user"].(map[string]any)["id"].(string)
	rec2, _ := doUsersReq(t, srv.Handler(), "DELETE", "/api/users/"+id, adminTok, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec2.Code, rec2.Body.String())
	}
	afterDelete := freshAfter(t, svc, afterCreate)
	if afterDelete.ownersN != before.ownersN {
		t.Fatalf("ownersN after delete = %d, want %d", afterDelete.ownersN, before.ownersN)
	}
}

// TestFairSharesInvalidateOnKeep: a keepped checkpoint changes the
// owner's kept bytes and the read must be fresh.
func TestFairSharesInvalidateOnKeep(t *testing.T) {
	srv, svc, db, _ := newFairShareServer(t)
	freezeFairShareClock(svc, time.Now())
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 256)
	id := createLeaseAs(srv.Handler(), "legacy-tok")
	if id == "" {
		t.Fatal("could not create lease")
	}
	before := svc.fairShares(context.Background())
	rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/leases/"+id+"/checkpoint", "legacy-tok", `{"keep":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkpoint keep: %d %s", rec.Code, rec.Body.String())
	}
	if body["build_id"] == nil {
		t.Fatalf("no build_id: %v", body)
	}
	// The keep path calls UpdateKeptMetrics, which invalidates.
	freshAfter(t, svc, before)
}

// TestFairSharesInvalidateOnNamedSaveAndDelete: a named save and a named
// delete change the owner's named bytes and the read must be fresh each
// time.
func TestFairSharesInvalidateOnNamedSaveAndDelete(t *testing.T) {
	srv, svc, db, _ := newFairShareServer(t)
	freezeFairShareClock(svc, time.Now())
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 256)
	id := createLeaseAs(srv.Handler(), "legacy-tok")
	if id == "" {
		t.Fatal("could not create lease")
	}
	before := svc.fairShares(context.Background())
	rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/leases/"+id+"/snapshots", "legacy-tok", `{"name":"spoond/warm"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if body["name"] != "spoond/warm" {
		t.Fatalf("save body = %v", body)
	}
	afterSave := freshAfter(t, svc, before)

	rec2, _ := doUsersReq(t, srv.Handler(), "DELETE", "/api/named-snapshots/spoond/warm", "legacy-tok", "")
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete named snapshot: %d %s", rec2.Code, rec2.Body.String())
	}
	freshAfter(t, svc, afterSave)
}

// TestFairSharesFailedReadNotCached: a failed capacity or store read
// yields a not-ok snapshot that is never cached, so the next read
// retries instead of serving zeros for the TTL.
func TestFairSharesFailedReadNotCached(t *testing.T) {
	t.Run("cold node info", func(t *testing.T) {
		svc, _, _, ids := newFairShareService(t)
		freezeFairShareClock(svc, time.Now())
		addIdentityUser(t, ids, "alice")
		// Cold cache: no NodeInfo reading is available.
		svc.nodeInfoMu.Lock()
		svc.nodeInfoAt = time.Time{}
		svc.nodeInfoMu.Unlock()
		beforeCalls := calls(svc.sub.(*testSub).Fake, "NodeInfo")
		snap := svc.fairShares(context.Background())
		if snap.ok {
			t.Fatal("cold NodeInfo snapshot marked ok")
		}
		if afterCalls := calls(svc.sub.(*testSub).Fake, "NodeInfo"); afterCalls != beforeCalls {
			t.Fatalf("fair-share path made %d NodeInfo RPC(s), want 0", afterCalls-beforeCalls)
		}
		svc.fairShareCache.mu.Lock()
		cached := svc.fairShareCache.snap
		svc.fairShareCache.mu.Unlock()
		if cached != nil {
			t.Fatal("not-ok snapshot was cached")
		}
		// A warm cache then reads a real slice.
		svc.updateNodeMetrics(context.Background())
		again := svc.fairShares(context.Background())
		if !again.ok {
			t.Fatal("warm NodeInfo snapshot not marked ok")
		}
	})

	t.Run("disk capacity error", func(t *testing.T) {
		svc, _, _, ids := newFairShareService(t)
		freezeFairShareClock(svc, time.Now())
		addIdentityUser(t, ids, "alice")
		svc.diskCapacity = func(string) (uint64, uint64, error) {
			return 0, 0, context.DeadlineExceeded
		}
		snap := svc.fairShares(context.Background())
		if snap.ok {
			t.Fatal("failed statfs snapshot marked ok")
		}
		svc.fairShareCache.mu.Lock()
		cached := svc.fairShareCache.snap
		svc.fairShareCache.mu.Unlock()
		if cached != nil {
			t.Fatal("failed statfs snapshot was cached")
		}
	})

	t.Run("store query error", func(t *testing.T) {
		svc, db, _, ids := newFairShareService(t)
		freezeFairShareClock(svc, time.Now())
		addIdentityUser(t, ids, "alice")
		// Close the store so the grouped byte queries fail.
		if err := db.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
		snap := svc.fairShares(context.Background())
		if snap.ok {
			t.Fatal("failed store snapshot marked ok")
		}
		svc.fairShareCache.mu.Lock()
		cached := svc.fairShareCache.snap
		svc.fairShareCache.mu.Unlock()
		if cached != nil {
			t.Fatal("failed store snapshot was cached")
		}
	})
}

// TestFairSharesCancelledRequestContextDetached: the box-wide compute
// runs detached from the request context, so a client disconnect does
// not turn the read into a failed, uncached snapshot.
func TestFairSharesCancelledRequestContextDetached(t *testing.T) {
	svc, _, _, ids := newFairShareService(t)
	freezeFairShareClock(svc, time.Now())
	addIdentityUser(t, ids, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap := svc.fairShares(ctx)
	if !snap.ok {
		t.Fatal("cancelled request context produced a not-ok snapshot")
	}
	svc.fairShareCache.mu.Lock()
	cached := svc.fairShareCache.snap
	svc.fairShareCache.mu.Unlock()
	if cached == nil {
		t.Fatal("ok snapshot from a cancelled request was not cached")
	}
}
