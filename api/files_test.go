package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
)

// filesDo performs a raw request (no JSON encoding) and returns the
// response with its body unread.
func filesDo(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" || method == http.MethodPut {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// filesBody drains and returns the response body as a string.
func filesBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	return string(b)
}

// filesCreate makes a lease as token-a and returns its id.
func filesCreate(t *testing.T, ts *httptest.Server, persistent bool) string {
	t.Helper()
	body := map[string]any{"image": "py-base", "ttl": 300}
	if persistent {
		body["persistent"] = true
	}
	resp, doc := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", body)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, doc)
	}
	return doc["id"].(string)
}

// filesSetup builds a server with a running lease owned by token-a's
// consumer.
func filesSetup(t *testing.T) (*httptest.Server, *Service, *testSub, string) {
	t.Helper()
	ts, svc, _, sub := newTestServerWithService(t)
	return ts, svc, sub, filesCreate(t, ts, false)
}

// filesURL renders the files URL for id and guest path, appending the
// given query.
func filesURL(ts *httptest.Server, id, p, query string) string {
	u := ts.URL + "/api/sandboxes/" + id + "/files" + p
	if query != "" {
		u += "?" + query
	}
	return u
}

// statFile GETs ?stat=1 and decodes the document.
func statFile(t *testing.T, ts *httptest.Server, token, id, p string) (int, map[string]any) {
	t.Helper()
	resp := filesDo(t, "GET", filesURL(ts, id, p, "stat=1"), token, "")
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		t.Fatalf("decode stat: %v", err)
	}
	return resp.StatusCode, out
}

func TestFilesWriteReadRoundTrip(t *testing.T) {
	ts, _, _, id := filesSetup(t)

	resp := filesDo(t, "PUT", filesURL(ts, id, "/etc/motd", "mode=0640"), "token-a", "hello files")
	if resp.StatusCode != 201 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	st, doc := statFile(t, ts, "token-a", id, "/etc/motd")
	if st != 200 {
		t.Fatalf("stat: %d %v", st, doc)
	}
	if doc["name"] != "motd" || doc["size"] != float64(11) || doc["mode"] != "640" || doc["is_dir"] != false {
		t.Fatalf("stat doc: %v", doc)
	}
	if mt, _ := doc["mod_time"].(string); mt == "" {
		t.Fatalf("stat doc missing mod_time: %v", doc)
	}

	resp = filesDo(t, "GET", filesURL(ts, id, "/etc/motd", ""), "token-a", "")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type %q", ct)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "hello files" {
		t.Fatalf("GET body = %q", b)
	}

	// Overwrite replaces the content.
	resp = filesDo(t, "PUT", filesURL(ts, id, "/etc/motd", ""), "token-a", "v2")
	if resp.StatusCode != 201 {
		t.Fatalf("second PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp = filesDo(t, "GET", filesURL(ts, id, "/etc/motd", ""), "token-a", "")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "v2" {
		t.Fatalf("after overwrite GET = %q", b)
	}
}

func TestFilesDefaultModes(t *testing.T) {
	ts, _, _, id := filesSetup(t)

	resp := filesDo(t, "PUT", filesURL(ts, id, "/a.txt", ""), "token-a", "x")
	if resp.StatusCode != 201 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	st, doc := statFile(t, ts, "token-a", id, "/a.txt")
	if st != 200 || doc["mode"] != "644" {
		t.Fatalf("file default mode: %d %v", st, doc)
	}

	// POST ?op=mkdir defaults to 0755.
	resp = filesDo(t, "POST", filesURL(ts, id, "/dir", "op=mkdir"), "token-a", "")
	if resp.StatusCode != 201 {
		t.Fatalf("mkdir: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	st, doc = statFile(t, ts, "token-a", id, "/dir")
	if st != 200 || doc["is_dir"] != true || doc["mode"] != "755" {
		t.Fatalf("mkdir default mode: %d %v", st, doc)
	}

	// Explicit modes are honoured.
	resp = filesDo(t, "POST", filesURL(ts, id, "/private", "op=mkdir&mode=0700"), "token-a", "")
	if resp.StatusCode != 201 {
		t.Fatalf("mkdir private: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	st, doc = statFile(t, ts, "token-a", id, "/private")
	if st != 200 || doc["mode"] != "700" {
		t.Fatalf("mkdir mode: %d %v", st, doc)
	}

	// Malformed and over-wide modes are 400.
	resp = filesDo(t, "POST", filesURL(ts, id, "/bad", "op=mkdir&mode=0999"), "token-a", "")
	if resp.StatusCode != 400 {
		t.Fatalf("mkdir mode 0999: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = filesDo(t, "PUT", filesURL(ts, id, "/bad.txt", "mode=nope"), "token-a", "x")
	if resp.StatusCode != 400 {
		t.Fatalf("PUT bad mode: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFilesMkdirAndRemove(t *testing.T) {
	ts, _, _, id := filesSetup(t)

	// mkdir creates missing parents.
	resp := filesDo(t, "POST", filesURL(ts, id, "/x/y/z", "op=mkdir"), "token-a", "")
	if resp.StatusCode != 201 {
		t.Fatalf("mkdir: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	st, doc := statFile(t, ts, "token-a", id, "/x/y/z")
	if st != 200 || doc["is_dir"] != true {
		t.Fatalf("mkdir parents: %d %v", st, doc)
	}

	// Unknown op is a 400.
	resp = filesDo(t, "POST", filesURL(ts, id, "/x", "op=touch"), "token-a", "")
	if resp.StatusCode != 400 {
		t.Fatalf("bad op: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// DELETE removes a file.
	resp = filesDo(t, "PUT", filesURL(ts, id, "/x/y/f.txt", ""), "token-a", "data")
	if resp.StatusCode != 201 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	resp = filesDo(t, "DELETE", filesURL(ts, id, "/x/y/f.txt", ""), "token-a", "")
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE file: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	st, _ = statFile(t, ts, "token-a", id, "/x/y/f.txt")
	if st != 404 {
		t.Fatalf("stat after delete: %d", st)
	}

	// A non-recursive DELETE of a non-empty directory is a 409.
	resp = filesDo(t, "DELETE", filesURL(ts, id, "/x", ""), "token-a", "")
	if resp.StatusCode != 409 {
		t.Fatalf("non-recursive dir delete: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	// ?recursive=1 removes it with everything under it.
	resp = filesDo(t, "DELETE", filesURL(ts, id, "/x", "recursive=1"), "token-a", "")
	if resp.StatusCode != 204 {
		t.Fatalf("recursive delete: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	st, _ = statFile(t, ts, "token-a", id, "/x")
	if st != 404 {
		t.Fatalf("stat after recursive delete: %d", st)
	}

	// Deleting something that is not there is a 404.
	resp = filesDo(t, "DELETE", filesURL(ts, id, "/never-existed", ""), "token-a", "")
	if resp.StatusCode != 404 {
		t.Fatalf("delete missing: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFilesLimit(t *testing.T) {
	ts, svc, _, id := filesSetup(t)

	// PUT beyond the cap is a 413 and writes nothing.
	big := strings.Repeat("a", maxFileBytes+1)
	resp := filesDo(t, "PUT", filesURL(ts, id, "/big.bin", ""), "token-a", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT over cap: %d", resp.StatusCode)
	}
	resp.Body.Close()
	st, _ := statFile(t, ts, "token-a", id, "/big.bin")
	if st != 404 {
		t.Fatalf("over-cap write landed: %d", st)
	}

	// Exactly at the cap works.
	resp = filesDo(t, "PUT", filesURL(ts, id, "/max.bin", ""), "token-a", strings.Repeat("a", maxFileBytes))
	if resp.StatusCode != 201 {
		t.Fatalf("PUT at cap: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()

	// GET beyond the cap is a 413: seed an over-size file directly
	// through the substrate.
	svc.store.mu.Lock()
	sbxID := svc.store.leases[id].SandboxID
	svc.store.mu.Unlock()
	if err := svc.sub.WriteFile(context.Background(), sbxID, "/huge.bin", make([]byte, maxFileBytes+1), 0o644); err != nil {
		t.Fatalf("seed huge file: %v", err)
	}
	resp = filesDo(t, "GET", filesURL(ts, id, "/huge.bin", ""), "token-a", "")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("GET over cap: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
}

func TestFilesTraversalCleaned(t *testing.T) {
	ts, _, _, id := filesSetup(t)

	// A literal .. segment never reaches the handler: Go's ServeMux
	// cleans the request path first (redirect for the unparsed client,
	// and a mux-cleaned path with no .. match here is a plain 404).
	resp := filesDo(t, "PUT", filesURL(ts, id, "/a/../../etc/passwd", ""), "token-a", "x")
	if resp.StatusCode != 404 {
		t.Fatalf("literal .. PUT: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Percent-encoded .. segments do reach the handler and clean inside
	// the guest root, never out of it: /a/../../etc/shadow lands on
	// /etc/shadow (which does not exist) — 404, not a write outside /.
	resp = filesDo(t, "GET", filesURL(ts, id, "/a/%2e%2e/%2e%2e/etc/shadow", ""), "token-a", "")
	if resp.StatusCode != 404 {
		t.Fatalf("encoded traversal GET: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()

	// The same cleaned path can be written and read back at its clean
	// location: the tail is guest-absolute after cleaning.
	resp = filesDo(t, "PUT", filesURL(ts, id, "/a/%2e%2e/etc/motd", ""), "token-a", "clean")
	if resp.StatusCode != 201 {
		t.Fatalf("cleaning PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	st, doc := statFile(t, ts, "token-a", id, "/etc/motd")
	if st != 200 {
		t.Fatalf("cleaned path missing: %d %v", st, doc)
	}
	resp = filesDo(t, "GET", filesURL(ts, id, "/etc/motd", ""), "token-a", "")
	if got := filesBody(t, resp); got != "clean" {
		t.Fatalf("cleaned GET body = %q", got)
	}

	// A bare .. tail cleans to /, which names no file: 404.
	resp = filesDo(t, "GET", filesURL(ts, id, "/%2e%2e", ""), "token-a", "")
	if resp.StatusCode != 404 {
		t.Fatalf("bare .. GET: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = filesDo(t, "DELETE", filesURL(ts, id, "/%2e%2e", ""), "token-a", "")
	if resp.StatusCode != 404 {
		t.Fatalf("bare .. DELETE: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFilesOwnership(t *testing.T) {
	ts, _, _, id := filesSetup(t)

	// Another consumer is 404 on every verb.
	for _, method := range []string{"GET", "PUT", "POST", "DELETE"} {
		query := ""
		if method == "POST" {
			query = "op=mkdir"
		}
		resp := filesDo(t, method, filesURL(ts, id, "/f.txt", query), "token-b", "x")
		if resp.StatusCode != 404 {
			t.Fatalf("%s as non-owner: %d %s", method, resp.StatusCode, filesBody(t, resp))
		}
		resp.Body.Close()
	}

	// No token at all is 401 (auth middleware, before ownership).
	resp := filesDo(t, "GET", filesURL(ts, id, "/f.txt", ""), "", "")
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// filesSetupAdmin builds a server with an identity store whose first
// (admin) user holds tok-admin; a second, non-admin user holds tok-user.
// The lease is created by the legacy consumer.
func filesSetupAdmin(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatalf("identity store: %v", err)
	}
	svc.SetIdentities(ids)
	h := NewServer(svc, NewImageRegistry(db)).Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"root","kind":"person","fingerprints":["SHA256:admin"],"token":"tok-admin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d %v", rec.Code, body)
	}
	rec, body = doUsersReq(t, h, "POST", "/api/users", "tok-admin", `{"name":"peon","kind":"person","fingerprints":["SHA256:peon"],"token":"tok-user"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %v", rec.Code, body)
	}

	resp, doc := doReq(t, "POST", ts.URL+"/api/sandboxes", "legacy-tok", map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %v", resp.StatusCode, doc)
	}
	return ts, doc["id"].(string)
}

func TestFilesAdminAccess(t *testing.T) {
	ts, id := filesSetupAdmin(t)

	// A non-admin identity user is 404.
	resp := filesDo(t, "PUT", filesURL(ts, id, "/f.txt", ""), "tok-user", "x")
	if resp.StatusCode != 404 {
		t.Fatalf("non-admin user: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()

	// The admin can write and read someone else's lease.
	resp = filesDo(t, "PUT", filesURL(ts, id, "/admin.txt", ""), "tok-admin", "from-admin")
	if resp.StatusCode != 201 {
		t.Fatalf("admin PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	resp = filesDo(t, "GET", filesURL(ts, id, "/admin.txt", ""), "tok-admin", "")
	if resp.StatusCode != 200 {
		t.Fatalf("admin GET: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
}

func TestFilesSuspendedConflict(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	ctx := context.Background()
	id, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant persistent: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", id.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	for _, method := range []string{"GET", "PUT", "POST", "DELETE"} {
		query := ""
		if method == "POST" {
			query = "op=mkdir"
		}
		resp := filesDo(t, method, filesURL(ts, id.ID, "/f.txt", query), "token-a", "x")
		if resp.StatusCode != 409 {
			t.Fatalf("%s on suspended lease: %d %s", method, resp.StatusCode, filesBody(t, resp))
		}
		resp.Body.Close()
	}
}

func TestFilesMissingPath(t *testing.T) {
	ts, _, _, id := filesSetup(t)

	st, _ := statFile(t, ts, "token-a", id, "/no/such/file")
	if st != 404 {
		t.Fatalf("stat missing: %d", st)
	}
	resp := filesDo(t, "GET", filesURL(ts, id, "/no/such/file", ""), "token-a", "")
	if resp.StatusCode != 404 {
		t.Fatalf("GET missing: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFilesTouchCountsAsActivity(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	before := l.LastActive
	time.Sleep(2 * time.Millisecond)

	resp := filesDo(t, "PUT", filesURL(ts, l.ID, "/act.txt", ""), "token-a", "x")
	if resp.StatusCode != 201 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, filesBody(t, resp))
	}
	resp.Body.Close()
	if !l.LastActive.After(before) {
		t.Fatalf("LastActive not moved by files PUT: %v -> %v", before, l.LastActive)
	}
}
