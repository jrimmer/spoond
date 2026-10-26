package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
)

// TestParsePinJobLeases: keys are owner/repo or owner, trimmed, empty
// entries dropped; a repository's entry and its owner's both pin, case
// apart; "*" pins every job; an empty value pins nothing.
func TestParsePinJobLeases(t *testing.T) {
	p, err := ParsePinJobLeases(" Acme/Web , ,example ")
	if err != nil {
		t.Fatalf("ParsePinJobLeases: %v", err)
	}
	if got := p.Keys(); !slices.Equal(got, []string{"Acme/Web", "example"}) {
		t.Fatalf("Keys = %v, want the keys as configured", got)
	}
	for repo, want := range map[string]bool{
		"acme/web":     true,  // the repository's own entry, case apart
		"ACME/WEB":     true,  // and upper case
		"acme/api":     false, // a repository entry covers that repository only
		"example/site": true,  // the owner's entry covers its repositories
		"Example/docs": true,
		"examples/x":   false, // an owner key is the whole owner, not a prefix
		"other/web":    false,
		"":             false,
	} {
		if got := p.Pins(repo); got != want {
			t.Errorf("Pins(%q) = %v, want %v", repo, got, want)
		}
	}

	// A repository entry beside its owner's: both pin, the repository's
	// matched first.
	p, err = ParsePinJobLeases("acme,acme/web")
	if err != nil {
		t.Fatalf("ParsePinJobLeases: %v", err)
	}
	if !p.Pins("acme/web") || !p.Pins("acme/api") {
		t.Fatal("acme/web and acme/api want pinned by their own and their owner's entries")
	}

	all, err := ParsePinJobLeases("*")
	if err != nil {
		t.Fatalf("ParsePinJobLeases(*): %v", err)
	}
	if !all.Pins("any/repo") || !all.Pins("") {
		t.Fatal("* must pin every job")
	}

	for _, v := range []string{"", " , ,"} {
		none, err := ParsePinJobLeases(v)
		if err != nil || none != nil {
			t.Fatalf("ParsePinJobLeases(%q) = %v, %v, want nil, nil", v, none, err)
		}
		if none.Pins("acme/web") || none.PinFor() != nil || none.Keys() != nil {
			t.Fatalf("an empty PIN_JOB_LEASES pins something")
		}
	}
}

// TestParsePinJobLeasesErrors: a malformed entry, a key given twice and
// "*" beside other entries fail, naming the entry by its position.
func TestParsePinJobLeasesErrors(t *testing.T) {
	for v, want := range map[string]string{
		"acme/web/extra":    "entry 1: the key is not owner/repo, owner or *",
		"acme,/web":         "entry 2: the key is not owner/repo, owner or *",
		"acme/":             "entry 1: the key is not owner/repo, owner or *",
		"ac me":             "entry 1: the key is not owner/repo, owner or *",
		"acme=/etc/x":       "entry 1: the key is not owner/repo, owner or *",
		"acme/w*":           "entry 1: the key is not owner/repo, owner or *",
		"acme,x,ACME":       "entry 3: ACME is listed twice",
		"acme/web,Acme/Web": "entry 2: Acme/Web is listed twice",
		"*,acme":            "entry 2: * pins every job and cannot be combined with other entries",
		"acme,*":            "entry 2: * pins every job and cannot be combined with other entries",
		"*,*":               "entry 2: * is listed twice",
	} {
		_, err := ParsePinJobLeases(v)
		if err == nil || err.Error() != "PIN_JOB_LEASES: "+want {
			t.Errorf("ParsePinJobLeases(%q) err = %v, want %q", v, err, "PIN_JOB_LEASES: "+want)
		}
	}
}

// TestHTTPLeaseClientCreatePinned: a pending pin (WithPin) rides the
// create body as "pinned": true and is spent by that create; without
// one, or after WithPin(false), no "pinned" key is sent.
func TestHTTPLeaseClientCreatePinned(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/leases" {
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			bodies = append(bodies, b)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"sb-%d"}`, len(bodies))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.WithPin(true)
	for i := 0; i < 2; i++ {
		if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	c.WithPin(true)
	c.WithPin(false)
	if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
		t.Fatalf("Create 3: %v", err)
	}
	if bodies[0]["pinned"] != true {
		t.Errorf("first create's pinned = %v, want true", bodies[0]["pinned"])
	}
	for i := 1; i < 3; i++ {
		if _, ok := bodies[i]["pinned"]; ok {
			t.Errorf("create %d sent pinned = %v, want no key (the pin is spent or cleared)", i+1, bodies[i]["pinned"])
		}
	}
}

// fakePinLease is a fakeLease with the LeasePinner capability: it
// records whether each create was asked for a pinned lease.
type fakePinLease struct {
	*fakeLease
	pending bool
	pinned  []bool // per create
}

func (f *fakePinLease) WithPin(pinned bool) { f.pending = pinned }

func (f *fakePinLease) Create(ctx context.Context, image string, ttl int) (string, error) {
	f.pinned = append(f.pinned, f.pending)
	f.pending = false
	return f.fakeLease.Create(ctx, image, ttl)
}

// TestExecutorPinsCoveredJobLeases: with PIN_JOB_LEASES set, a job whose
// repository it covers creates its lease pinned and one it does not
// creates it unpinned — through the job's own lease identity when
// LEASE_TOKEN_MAP routes it — and a pin a job set but never spent does
// not carry over to the worker's next job.
func TestExecutorPinsCoveredJobLeases(t *testing.T) {
	pins, err := ParsePinJobLeases("acme")
	if err != nil {
		t.Fatalf("ParsePinJobLeases: %v", err)
	}
	dir := t.TempDir()
	web := writeTokenFile(t, dir, "web", "tok-web", 0o600)
	m, _, err := ParseLeaseTokenMap("acme/web="+web, os.ReadFile)
	if err != nil {
		t.Fatalf("ParseLeaseTokenMap: %v", err)
	}
	routed := &fakePinLease{fakeLease: newFakeLease()}
	def := &fakePinLease{fakeLease: newFakeLease()}
	e := &Executor{
		Sandbox:     def,
		Sink:        &fakeSink{},
		Labels:      map[string]string{"ubuntu-latest": "ci-image"},
		RepoBaseURL: "https://code.example.com",
		SandboxFor:  m.Router(func(string) SandboxProvider { return routed }),
		PinLease:    pins.PinFor(),
	}
	job := func(id int64, repo string) *Job {
		j := testJob(routedPayload)
		j.ID = id
		j.Context["repository"] = repo
		return j
	}

	if err := e.Run(context.Background(), job(1, "acme/api")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := e.Run(context.Background(), job(2, "someone/else")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(def.pinned, []bool{true, false}) {
		t.Fatalf("default identity's creates pinned = %v, want [true false]", def.pinned)
	}
	// Routed through LEASE_TOKEN_MAP, the pin follows the job.
	if err := e.Run(context.Background(), job(3, "Acme/Web")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(routed.pinned, []bool{true}) {
		t.Fatalf("routed identity's creates pinned = %v, want [true]", routed.pinned)
	}

	// A covered job that fails before its create leaves a pin pending;
	// the next, uncovered job clears it.
	bad := job(4, "acme/api")
	bad.Workflow = []byte("not: [a workflow")
	_ = e.Run(context.Background(), bad)
	if !def.pending {
		t.Fatal("the failed covered job set no pin (the carry-over case is not exercised)")
	}
	if err := e.Run(context.Background(), job(5, "someone/else")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := def.pinned[len(def.pinned)-1]; got {
		t.Fatal("an uncovered job's lease was created pinned: a failed job's pin carried over")
	}

	// Without PIN_JOB_LEASES nothing is pinned and WithPin is never
	// called.
	plain := &fakePinLease{fakeLease: newFakeLease()}
	e2 := &Executor{Sandbox: plain, Sink: &fakeSink{}, Labels: map[string]string{"ubuntu-latest": "ci-image"}, RepoBaseURL: "https://code.example.com"}
	if err := e2.Run(context.Background(), job(6, "acme/api")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(plain.pinned, []bool{false}) {
		t.Fatalf("creates pinned = %v without PIN_JOB_LEASES, want [false]", plain.pinned)
	}
}
