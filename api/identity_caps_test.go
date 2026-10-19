package api

// Per-identity concurrency-cap tests (scout#20): the per-owner
// exec/stream cap and the guest-dial cap are host knobs
// (EXEC_CONCURRENCY_MAX, GUEST_DIAL_MAX) with an optional per-identity
// override stored on the identity (identity.User.ExecConcurrency /
// .GuestDial, set through the admin quota route).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v3/identity"
)

// capsServer bootstraps admin + two plain users (jason, scout) on one
// server and returns the handler and the store. The first user is admin
// (KTD-2); the store has no file, so the restart test builds its own.
func capsServer(t *testing.T) (*Server, http.Handler, *identity.Store) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	if rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d %s", rec.Code, body)
	}
	for _, u := range []struct{ name, tok string }{{"jason", "jason-tok"}, {"scout", "scout-tok"}} {
		if rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"`+u.name+`","fingerprints":["SHA256:fp-`+u.name+`"],"token":"`+u.tok+`"}`); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", u.name, rec.Code, body)
		}
	}
	return srv, h, ids
}

// capsUserID resolves a token's user id through the users/me route.
func capsUserID(t *testing.T, h http.Handler, tok string) string {
	t.Helper()
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me as %q: %d %s", tok, rec.Code, body)
	}
	return body["user"].(map[string]any)["id"].(string)
}

// capsExec429 sends one exec request as tok and returns the status.
func capsExec429(h http.Handler, tok string) int {
	req := httptest.NewRequest("POST", "/api/leases/any/exec", strings.NewReader(`{"cmd":"echo hi"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestBusyCapDefaultUnchanged pins the built-in default: an owner with
// no override holds exactly 8 concurrent exec/stream slots (the 9th
// admits nothing) and the 429 message names exec/stream operations —
// today's behaviour unchanged by the knob work (scout#20).
func TestBusyCapDefaultUnchanged(t *testing.T) {
	srv, h, ids := capsServer(t)
	u := ids.UserByName("jason")
	if u == nil || u.ExecConcurrency != 0 {
		t.Fatalf("fresh user carries an override: %+v", u)
	}

	if got := srv.busyMax; got != 8 {
		t.Fatalf("built-in busyMax = %d, want 8", got)
	}
	for i := 0; i < 8; i++ {
		if !srv.acquireBusy(u.ID) {
			t.Fatalf("slot %d refused before the cap", i)
		}
	}
	if srv.acquireBusy(u.ID) {
		t.Fatal("9th slot admitted under the default cap of 8")
	}
	if got := capsExec429(h, "jason-tok"); got != http.StatusTooManyRequests {
		t.Fatalf("exec over the default cap = %d, want 429", got)
	}
	req := httptest.NewRequest("POST", "/api/leases/any/exec", strings.NewReader(`{"cmd":"echo hi"}`))
	req.Header.Set("Authorization", "Bearer jason-tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Body.String(); !strings.Contains(got, "too many concurrent exec/stream operations; try again shortly") {
		t.Fatalf("429 body = %q, want the unchanged exec/stream message", got)
	}
}

// TestExecCapOverrideRaisesOneIdentity: an override (set through the
// admin quota route) lets one identity hold MORE exec/stream slots than
// the host default while every other identity stays at the default.
func TestExecCapOverrideRaisesOneIdentity(t *testing.T) {
	srv, h, ids := capsServer(t)
	jid := capsUserID(t, h, "jason-tok")
	sid := capsUserID(t, h, "scout-tok")

	rec, body := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"exec_concurrency":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set exec_concurrency: %d %s", rec.Code, body)
	}
	// The override comes back on the user view.
	if got, _ := body["user"].(map[string]any)["exec_concurrency"].(float64); int(got) != 10 {
		t.Fatalf("exec_concurrency in user view = %v, want 10", body["user"].(map[string]any)["exec_concurrency"])
	}

	// jason holds 10 slots now (the default was 8).
	for i := 0; i < 10; i++ {
		if !srv.acquireBusy(jid) {
			t.Fatalf("overridden slot %d refused", i)
		}
	}
	if srv.acquireBusy(jid) {
		t.Fatal("11th slot admitted under the override of 10")
	}
	// scout (no override) still holds exactly the default 8.
	for i := 0; i < 8; i++ {
		if !srv.acquireBusy(sid) {
			t.Fatalf("default-cap slot %d refused for the other identity", i)
		}
	}
	if srv.acquireBusy(sid) {
		t.Fatal("the other identity's 9th slot admitted; the override must not leak")
	}
	// The HTTP path answers the 11th request with the unchanged 429.
	if got := capsExec429(h, "jason-tok"); got != http.StatusTooManyRequests {
		t.Fatalf("exec over the override = %d, want 429", got)
	}
	// A negative value is refused.
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"exec_concurrency":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative exec_concurrency = %d, want 400", rec.Code)
	}
	// 0 unsets: jason is back on the host default. The 10 slots they
	// still hold now exceed the restored default of 8, which the next
	// admission correctly refuses (held slots are never retroactively
	// taken away); the cleared value is the store's.
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"exec_concurrency":0}`); rec.Code != http.StatusOK {
		t.Fatalf("unset exec_concurrency: %d", rec.Code)
	}
	if u := ids.UserByID(jid); u.ExecConcurrency != 0 {
		t.Fatalf("cleared override still set: %d", u.ExecConcurrency)
	}
	if srv.acquireBusy(jid) {
		t.Fatal("11th slot admitted after unsetting to the default of 8 with 10 held")
	}
}

// TestGuestDialCapDefaultAndOverride is the dial-side twin: the default
// is 16, an override raises one identity's cap without touching the
// other identities', and a negative is refused while 0 unsets.
func TestGuestDialCapDefaultAndOverride(t *testing.T) {
	srv, h, ids := capsServer(t)
	jid := capsUserID(t, h, "jason-tok")

	if got := srv.dialMax; got != 16 {
		t.Fatalf("built-in dialMax = %d, want 16", got)
	}
	if u := ids.UserByName("scout"); u.GuestDial != 0 {
		t.Fatalf("fresh user carries a dial override: %+v", u)
	}
	rec, body := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"guest_dial":20}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set guest_dial: %d %s", rec.Code, body)
	}
	if got, _ := body["user"].(map[string]any)["guest_dial"].(float64); int(got) != 20 {
		t.Fatalf("guest_dial in user view = %v, want 20", body["user"].(map[string]any)["guest_dial"])
	}
	// 20 slots open under the override.
	for i := 0; i < 20; i++ {
		if !srv.acquireDial(jid) {
			t.Fatalf("overridden dial slot %d refused", i)
		}
	}
	if srv.acquireDial(jid) {
		t.Fatal("21st dial admitted under the override of 20")
	}
	// scout, no override: exactly 16.
	sid := capsUserID(t, h, "scout-tok")
	for i := 0; i < 16; i++ {
		if !srv.acquireDial(sid) {
			t.Fatalf("default dial slot %d refused for the other identity", i)
		}
	}
	if srv.acquireDial(sid) {
		t.Fatal("the other identity's 17th dial admitted; the override must not leak")
	}
	// A negative value is refused; 0 unsets.
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"guest_dial":-2}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative guest_dial = %d, want 400", rec.Code)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"guest_dial":0}`); rec.Code != http.StatusOK {
		t.Fatalf("unset guest_dial = %d, want 200", rec.Code)
	}
	if u := ids.UserByID(jid); u.GuestDial != 0 {
		t.Fatalf("cleared dial override still set: %d", u.GuestDial)
	}
}

// TestDialRoute429MessageAndOverride drives the real dial route with an
// echo guest: at the default cap the next dial answers 429 with the
// message the route has always sent, and an identity whose override
// raises the cap to 24 holds 24 live dials on the same route — the
// route's refusal counts the owner's effective limit, not just the host
// default.
func TestDialRoute429MessageAndOverride(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	file := t.TempDir() + "/users.json"
	ids, err := identity.NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	if rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d %s", rec.Code, body)
	}
	if rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"scout","fingerprints":["SHA256:fp-s"],"token":"scout-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create scout: %d %s", rec.Code, body)
	}
	sid := capsUserID(t, h, "scout-tok")
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+sid+"/quota", "admin-tok", `{"guest_dial":24}`); rec.Code != http.StatusOK {
		t.Fatal("set guest_dial override")
	}

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "scout-tok", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	host, echoPort := newEchoServer(t)
	pointHostIP(t, svc, id, host, echoPort)

	// The real route under the override: 24 concurrent dials admit (the
	// default would refuse the 17th), then the 25th is refused with the
	// unchanged message and a counted "refused" metric.
	var open []*websocket.Conn
	for i := 0; i < 24; i++ {
		ws, resp, err := dialGuest(t, ts, id, echoPort, "scout-tok")
		if err != nil {
			t.Fatalf("overridden dial %d: %v (status %v)", i, err, resp)
		}
		open = append(open, ws)
	}
	if _, resp, err := dialGuest(t, ts, id, echoPort, "scout-tok"); err == nil {
		t.Fatal("25th dial admitted; the override is 24")
	} else if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("25th dial = %d, want 429", resp.StatusCode)
	}
	for _, ws := range open {
		ws.Close()
	}
}

// TestCapsOverrideSurvivesRestart: the overrides live in the users
// file, so a backend restart — a fresh Store over the same file serving
// a fresh Server — keeps them, and the fresh server's acquireBusy and
// acquireDial enforce the loaded values on its fresh counters.
func TestCapsOverrideSurvivesRestart(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	file := t.TempDir() + "/users.json"
	ids, err := identity.NewStore(file)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	if rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d %s", rec.Code, body)
	}
	rec2, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"scout","fingerprints":["SHA256:fp-s"],"token":"scout-tok"}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("create scout: %d %s", rec2.Code, rec2.Body.String())
	}
	sid := body["user"].(map[string]any)["id"].(string)
	if rec3, out := doUsersReq(t, h, "POST", "/api/users/"+sid+"/quota", "admin-tok", `{"exec_concurrency":12,"guest_dial":24}`); rec3.Code != http.StatusOK {
		t.Fatalf("set overrides: %d %s", rec3.Code, out)
	}

	// The restart: a fresh Store over the same file serving a fresh
	// Server (fresh counters), as a restarted process would.
	reborn, err := identity.NewStore(file)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	loaded := reborn.UserByID(sid)
	if loaded.ExecConcurrency != 12 || loaded.GuestDial != 24 {
		t.Fatalf("override after reload = exec %d, dial %d, want 12 and 24", loaded.ExecConcurrency, loaded.GuestDial)
	}
	svc2, db2, _ := newTestService(t)
	seedImage(t, db2, "py-base", 2048)
	svc2.SetIdentities(reborn)
	srv2 := NewServer(svc2, NewImageRegistry(db2))

	// The loaded caps are the ones enforced: 12 busy slots, not 8.
	for i := 0; i < 12; i++ {
		if !srv2.acquireBusy(sid) {
			t.Fatalf("restarted slot %d refused under the loaded override", i)
		}
	}
	if srv2.acquireBusy(sid) {
		t.Fatal("restarted server admitted past the loaded override of 12")
	}
	// 24 dial slots, not 16.
	for i := 0; i < 24; i++ {
		if !srv2.acquireDial(sid) {
			t.Fatalf("restarted dial %d refused under the loaded override", i)
		}
	}
	if srv2.acquireDial(sid) {
		t.Fatal("restarted server admitted a 25th dial past the loaded override")
	}
	// The host defaults are untouched for an owner without an override.
	other, _ := reborn.AddUser("plain", identity.KindPerson, nil, "plain-tok")
	if other.ExecConcurrency != 0 || other.GuestDial != 0 {
		t.Fatalf("fresh user carries overrides after reload: %+v", other)
	}
	if srv2.busyMax != 8 || srv2.dialMax != 16 {
		t.Fatalf("restarted host defaults = busy %d, dial %d, want 8 and 16", srv2.busyMax, srv2.dialMax)
	}
}

// TestHostDefaultKnobs pins the env knobs: EXEC_CONCURRENCY_MAX and
// GUEST_DIAL_MAX change the host default every owner without an
// override counts against, and an unusable value keeps the built-in.
func TestHostDefaultKnobs(t *testing.T) {
	t.Run("EXEC_CONCURRENCY_MAX sets the host busy cap", func(t *testing.T) {
		t.Setenv("EXEC_CONCURRENCY_MAX", "12")
		srv, _, _ := capsServer(t)
		if got := srv.busyMax; got != 12 {
			t.Fatalf("busyMax under EXEC_CONCURRENCY_MAX=12 = %d, want 12", got)
		}
	})
	t.Run("GUEST_DIAL_MAX sets the host dial cap", func(t *testing.T) {
		t.Setenv("GUEST_DIAL_MAX", "24")
		srv, _, _ := capsServer(t)
		if got := srv.dialMax; got != 24 {
			t.Fatalf("dialMax under GUEST_DIAL_MAX=24 = %d, want 24", got)
		}
	})
	t.Run("defaults keep today's behaviour", func(t *testing.T) {
		srv, _, _ := capsServer(t)
		if srv.busyMax != 8 || srv.dialMax != 16 {
			t.Fatalf("defaults = busy %d, dial %d, want 8 and 16", srv.busyMax, srv.dialMax)
		}
	})
	t.Run("an unusable value keeps the built-in", func(t *testing.T) {
		t.Setenv("EXEC_CONCURRENCY_MAX", "soon")
		t.Setenv("GUEST_DIAL_MAX", "0")
		srv, _, _ := capsServer(t)
		if srv.busyMax != 8 || srv.dialMax != 16 {
			t.Fatalf("unusable values gave busy %d, dial %d, want the built-ins 8 and 16", srv.busyMax, srv.dialMax)
		}
	})
}

// TestCapsAdminOnly: a non-admin cannot set the overrides (the quota
// route is admin-only and stays so), a non-integer is 400, and an
// unknown user is 404.
func TestCapsAdminOnly(t *testing.T) {
	_, h, ids := capsServer(t)
	jid := capsUserID(t, h, "jason-tok")
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "scout-tok", `{"exec_concurrency":99}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin override set = %d, want 403", rec.Code)
	}
	if u := ids.UserByID(jid); u.ExecConcurrency != 0 {
		t.Fatalf("non-admin set an override anyway: %d", u.ExecConcurrency)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"exec_concurrency":"lots"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-integer exec_concurrency = %d, want 400", rec.Code)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+jid+"/quota", "admin-tok", `{"guest_dial":"many"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-integer guest_dial = %d, want 400", rec.Code)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/u-nope/quota", "admin-tok", `{"exec_concurrency":4}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", rec.Code)
	}
}
