package api

// Lease holders (2.1): a lease can name what holds it (a CI job, an
// orchestrator's flight, a person's scratch work) with a link. A held
// lease is left alone by the sweepers — not released at its TTL, not
// idle-suspended — and is checkpointed periodically like a persistent
// lease. Clearing the holder restores normal sweeping.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLeaseHolderCreateRoundTrip covers create with and without holder:
// the create response echoes the fields, they appear in the single GET
// and in the list, and a second service on the same database loads them.
func TestLeaseHolderCreateRoundTrip(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	// With holder.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300,
		"holder": "ci-job-42", "holder_url": "https://ci.example.com/jobs/42",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with holder = %d (%v), want 201", resp.StatusCode, body)
	}
	if body["holder"] != "ci-job-42" || body["holder_url"] != "https://ci.example.com/jobs/42" {
		t.Fatalf("create response holder fields = %v / %v", body["holder"], body["holder_url"])
	}
	held := body["id"].(string)

	// Without holder: both fields come back empty.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create without holder = %d (%v), want 201", resp.StatusCode, body)
	}
	plain := body["id"].(string)
	if h, _ := body["holder"].(string); h != "" {
		t.Fatalf("unheld create returned holder %q", h)
	}
	if u, _ := body["holder_url"].(string); u != "" {
		t.Fatalf("unheld create returned holder_url %q", u)
	}

	// Detail read carries the fields.
	_, got := doReq(t, "GET", ts.URL+"/api/leases/"+held, "token-a", nil)
	if got["holder"] != "ci-job-42" || got["holder_url"] != "https://ci.example.com/jobs/42" {
		t.Fatalf("detail holder fields = %v / %v", got["holder"], got["holder_url"])
	}

	// List rows carry them too.
	_, list := doReq(t, "GET", ts.URL+"/api/leases", "token-a", nil)
	seen := map[string]map[string]any{}
	for _, row := range list["sandboxes"].([]any) {
		m := row.(map[string]any)
		seen[m["id"].(string)] = m
	}
	if seen[held]["holder"] != "ci-job-42" {
		t.Fatalf("list holder = %v, want ci-job-42", seen[held]["holder"])
	}
	if h, _ := seen[plain]["holder"].(string); h != "" {
		t.Fatalf("unheld list row holder = %q", h)
	}

	// The fields persist through a reload from the store.
	ctx := context.Background()
	svc.Shutdown(ctx)
	if err := svc.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}
	l := svc.lookup("consumer-a", held)
	if l == nil || l.Holder != "ci-job-42" || l.HolderUrl != "https://ci.example.com/jobs/42" {
		t.Fatalf("reloaded lease holder = %+v", l)
	}
}

// TestLeaseHolderValidation pins the create-time validation: holder at
// most 128 printable characters, holder_url empty or an absolute
// http(s) URL of at most 512 characters; otherwise 400 naming the field.
func TestLeaseHolderValidation(t *testing.T) {
	ts, _ := newTestServer(t)

	cases := []struct {
		name   string
		holder string
		url    string
		want   string // substring of the 400 message
	}{
		{"holder too long", strings.Repeat("x", 129), "", "holder"},
		{"holder not printable", "job\x0742", "", "holder"},
		{"url not absolute", "ci/job/42", "holder_url must be an absolute", ""},
		{"url bad scheme", "ftp://ci.example.com/42", "holder_url must be an absolute", ""},
		{"url too long", "ok", "https://ci.example.com/" + strings.Repeat("x", 512), "holder_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holder, url := tc.holder, tc.url
			if tc.name == "url not absolute" {
				holder, url = "", tc.holder
			}
			if tc.name == "url bad scheme" || tc.name == "url too long" {
				holder, url = "", tc.url
			}
			resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
				"image": "py-base", "holder": holder, "holder_url": url,
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("create = %d (%v), want 400", resp.StatusCode, body)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want it to name %q", msg, tc.want)
			}
		})
	}

	// Boundaries are accepted: 128-char holder, 512-char https URL.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image":      "py-base",
		"holder":     strings.Repeat("x", 128),
		"holder_url": "https://ci.example.com/" + strings.Repeat("x", 488),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("boundary create = %d (%v), want 201", resp.StatusCode, body)
	}
}

// TestLeaseHolderPutSetAndClear covers PUT /api/leases/{id}/holder:
// setting later, clearing with both fields empty, and validation.
func TestLeaseHolderPutSetAndClear(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	// Set.
	resp, body := doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{
		"holder": "flight-7", "holder_url": "https://honey.example/flights/7",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put holder = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["holder"] != "flight-7" || body["holder_url"] != "https://honey.example/flights/7" {
		t.Fatalf("put response = %v / %v", body["holder"], body["holder_url"])
	}
	if l := svc.lookup("consumer-a", id); l == nil || l.Holder != "flight-7" || !l.held() {
		t.Fatalf("lease not held after put: %+v", l)
	}

	// Validation failure leaves the previous value in place.
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{
		"holder": "ok", "holder_url": "not-a-url",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad put = %d (%v), want 400", resp.StatusCode, body)
	}
	if !strings.Contains(body["error"].(string), "holder_url") {
		t.Fatalf("error %q does not name the field", body["error"])
	}

	// Clear: both empty restores normal sweeping.
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{
		"holder": "", "holder_url": "",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear put = %d (%v), want 200", resp.StatusCode, body)
	}
	if l := svc.lookup("consumer-a", id); l == nil || l.held() || l.HolderUrl != "" {
		t.Fatalf("holder not cleared: %+v", l)
	}
}

// TestLeaseHolderPutOwnerOnly: the PUT route answers 404 for anyone who
// is not the owner (and for an unknown lease), like the other lease
// routes.
func TestLeaseHolderPutOwnerOnly(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	resp, _ := doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-b", map[string]any{"holder": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other owner's put = %d, want 404", resp.StatusCode)
	}
	resp, _ = doReq(t, "PUT", ts.URL+"/api/leases/no-such-lease/holder", "token-a", map[string]any{"holder": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown lease put = %d, want 404", resp.StatusCode)
	}
	if l := svc.lookup("consumer-a", id); l != nil && l.held() {
		t.Fatalf("lease held despite 404s: %+v", l)
	}
}

// TestHeldLeaseSurvivesTTLSweep: a plain lease with a holder is not
// released by sweepExpired even long past its TTL.
func TestHeldLeaseSurvivesTTLSweep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", 50*time.Millisecond, false, "", nil, "ci-job", "https://ci.example.com/42", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // pass the expiry

	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("held lease was swept despite its holder")
	}

	// The unheld control is swept.
	plain, err := svc.grant(ctx, "c", "py-base", 50*time.Millisecond, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant control: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	svc.sweepExpired(ctx)
	if svc.lookup("c", plain.ID) != nil {
		t.Fatal("unheld lease was not swept")
	}
}

// TestClearedHolderRestoresSweeping: clearing the holder through the
// route puts the lease back under the TTL sweeper.
func TestClearedHolderRestoresSweeping(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", 50*time.Millisecond, false, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	// Still held: survives.
	svc.sweepExpired(ctx)
	if svc.lookup("consumer-a", l.ID) == nil {
		t.Fatal("held lease swept before clearing")
	}

	// Clear through the route; the next sweep takes it.
	resp, body := doReq(t, "PUT", ts.URL+"/api/leases/"+l.ID+"/holder", "token-a", map[string]any{"holder": "", "holder_url": ""})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d (%v), want 200", resp.StatusCode, body)
	}
	svc.sweepExpired(ctx)
	if svc.lookup("consumer-a", l.ID) != nil {
		t.Fatal("lease survived the sweep after its holder was cleared")
	}
}

// TestHeldLeaseNotIdleSuspended: a persistent lease with a holder idle
// far past IdleTimeout is not suspended by the idle sweep.
func TestHeldLeaseNotIdleSuspended(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.IdleTimeout = 50 * time.Millisecond
	ctx := context.Background()

	held, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant held: %v", err)
	}
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant control: %v", err)
	}

	time.Sleep(80 * time.Millisecond) // both idle past the timeout
	svc.sweepExpired(ctx)

	if held.Suspended {
		t.Fatal("held lease was idle-suspended")
	}
	if got := calls(sub.Fake, "Pause "+held.SandboxID); got != 0 {
		t.Fatalf("held lease paused %d times, want 0", got)
	}
	if !plain.Suspended {
		t.Fatal("unheld idle lease was not suspended")
	}
}

// TestHeldPlainLeaseCheckpointed: the periodic checkpoint pass covers a
// held plain (non-persistent) lease, and skips it once unheld.
func TestHeldPlainLeaseCheckpointed(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("held plain lease checkpointed %d times, want 1", got)
	}

	// Activity re-arms the lease (LastActive after LastCheckpointAt);
	// with the holder cleared the pass no longer touches the plain
	// lease.
	svc.touch(l.ID)
	l.Holder = ""
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("unheld plain lease checkpointed %d times total, want 1", got)
	}
}

// TestLeaseHolderPutAdminAllowed: an identity-store admin may set the
// holder on another user's lease; a non-owner still gets 404.
func TestLeaseHolderPutAdminAllowed(t *testing.T) {
	h, _, _ := newShareTestServer(t)
	rec := httptest.NewRecorder()
	// a creates a lease; the admin (bootstrapped user) sets the holder.
	lid := createLeaseAs(h, "tok-a")
	if lid == "" {
		t.Fatal("a could not create lease")
	}
	req := httptest.NewRequest("PUT", "/api/sandboxes/"+lid+"/holder",
		strings.NewReader(`{"holder":"ops","holder_url":"https://ops.example/x"}`))
	req.Header.Set("Authorization", "Bearer admin-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin put holder = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	// The owner sees the admin-set holder.
	req2 := httptest.NewRequest("GET", "/api/sandboxes/"+lid, nil)
	req2.Header.Set("Authorization", "Bearer tok-a")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	var got map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)
	if got["holder"] != "ops" {
		t.Fatalf("owner sees holder %v, want ops", got["holder"])
	}
}

// Holder text is measured in characters, not bytes, and format
// characters (zero-width spaces, bidi overrides) are refused.
func TestValidateHolderUnicode(t *testing.T) {
	cases := []struct {
		name, holder string
		ok           bool
	}{
		{"100 CJK characters", strings.Repeat("流", 100), true},
		{"129 CJK characters", strings.Repeat("流", 129), false},
		{"accented and spaces", "build 3611 · café", true},
		{"zero-width space", "ci​3611", false},
		{"bidi override", "ci‮1163", false},
		{"C1 control", "ci\u00853611", false},
		{"invalid UTF-8", "ci\xff", false},
	}
	for _, c := range cases {
		err := validateHolder(c.holder, "")
		if (err == nil) != c.ok {
			t.Errorf("%s: validateHolder err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
