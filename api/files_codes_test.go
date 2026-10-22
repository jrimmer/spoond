package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v4/substrate"
)

// Stable error codes on the file routes (spoond-wir9): every error
// response carries a machine-readable "code" beside the free-form
// "error" message. The code is the contract; the message is diagnostic
// and may change. These table tests pin status + code for every case;
// the HTTP statuses were already the contract and do not change.

// filesCodeCase is one files-route refusal: the request to make, its
// preconditions (seeds, substrate failures, the lease's busy flag), and
// the status + code it must answer with.
type filesCodeCase struct {
	name   string
	method string
	path   string
	query  string
	body   string
	token  string // "" = token-a (the owner)
	want   int
	code   string
	// seed files and directories the case starts from
	seeds   []string
	dirs    []string
	subFail string // method whose calls fail, "" = none
	subErr  error  // the error FailCall answers those calls with
	busy    bool   // the lease is mid-lifecycle-operation
}

// filesDecode drains resp into a JSON body map.
func filesDecode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

// filesSeed writes the case's files and (as containers) its directories
// through the substrate directly.
func filesSeed(t *testing.T, sub *testSub, sandboxID string, files, dirs []string) {
	t.Helper()
	for _, d := range dirs {
		if err := sub.MakeDir(t.Context(), sandboxID, d, 0o755); err != nil {
			t.Fatalf("seed dir %s: %v", d, err)
		}
	}
	for _, p := range files {
		// 300 bytes: over the shrunken EFBIG cap, tiny at the default.
		if err := sub.WriteFile(t.Context(), sandboxID, p, []byte(strings.Repeat("x", 300)), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
}

// TestFilesErrorCodes walks the file routes' error codes. The route side
// (path, mode, op, the request-body cap, the lease lookup) and the
// substrate side (mapFileError's branches) both get their cases.
func TestFilesErrorCodes(t *testing.T) {
	cases := []filesCodeCase{
		// mapFileError's substrate branches:
		{name: "stat missing -> ENOENT", method: "GET", path: "/no/such/file", query: "stat=1", want: 404, code: "ENOENT"},
		{name: "delete missing -> ENOENT", method: "DELETE", path: "/no/such/file", want: 404, code: "ENOENT"},
		{name: "mkdir under a file -> ENOTDIR", method: "POST", path: "/plain.txt/child", query: "op=mkdir", want: 409, code: "ENOTDIR", seeds: []string{"/plain.txt"}},
		{name: "non-recursive delete of a non-empty dir -> ENOTEMPTY", method: "DELETE", path: "/populated", want: 409, code: "ENOTEMPTY", seeds: []string{"/populated/child.txt"}},
		{name: "mkdir over an existing file -> EEXIST", method: "POST", path: "/plain.txt", query: "op=mkdir", want: 409, code: "EEXIST", seeds: []string{"/plain.txt"}},
		{name: "PUT onto a directory -> EISDIR", method: "PUT", path: "/dironly", body: "x", want: 409, code: "EISDIR", dirs: []string{"/dironly"}},
		{name: "GET content of a directory -> EISDIR", method: "GET", path: "/dironly", want: 409, code: "EISDIR", dirs: []string{"/dironly"}},
		{name: "GET content of a missing file -> ENOENT", method: "GET", path: "/no/such/file", want: 404, code: "ENOENT"},
		{name: "substrate refuses the operation -> EACCES", method: "PUT", path: "/f.txt", body: "x", want: 403, code: "EACCES",
			subFail: "WriteFile", subErr: substrate.ErrPermission},
		{name: "PUT over the size cap -> EFBIG", method: "PUT", path: "/big.bin", body: "xxx", want: 413, code: "EFBIG"},
		{name: "GET over the size cap -> EFBIG", method: "GET", path: "/huge.bin", want: 413, code: "EFBIG", seeds: []string{"/huge.bin"}},
		{name: "capacity -> capacity_wait", method: "GET", path: "/f.txt", query: "stat=1", want: 503, code: "capacity_wait", subFail: "Stat", subErr: substrate.ErrCapacity},
		{name: "substrate failure -> EIO", method: "PUT", path: "/f.txt", body: "x", want: 502, code: "EIO", subFail: "WriteFile", subErr: errors.New("disk gone")},
		// The mid-lifecycle busy answer keeps the existing lease_busy code:
		// mid-checkpoint the whole sandbox reads as missing from the
		// orchestrator, so the stat of an existing path reports busy, not
		// a missing file.
		{name: "stat mid-checkpoint -> lease_busy", method: "GET", path: "/plain.txt", query: "stat=1", want: 409, code: "lease_busy", busy: true,
			subFail: "Stat", subErr: substrate.ErrNotFound},
		// Route-side refusals:
		{name: "bad mode -> EINVAL", method: "PUT", path: "/m.bin", body: "x", query: "mode=0999", want: 400, code: "EINVAL"},
		{name: "bad op -> EINVAL", method: "POST", path: "/f.bin", query: "op=touch", want: 400, code: "EINVAL"},
		{name: "root path -> EINVAL", method: "GET", path: "/", want: 404, code: "EINVAL"},
		{name: "unknown lease -> not_found", method: "GET", path: "/f.txt", token: "token-b", want: 404, code: "not_found"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The 413 cases shrink the cap; every other case runs at the
			// default. Restored on cleanup, so the cases stay independent.
			defer func(n int64) { maxFileBytes = n }(maxFileBytes)
			if strings.Contains(tc.name, "size cap -> EFBIG") {
				// Shrink the cap so a small body crosses it: the seeded
				// huge.bin is 300 bytes over, the PUT's 3-byte body over.
				maxFileBytes = 2
			}
			ts, svc, sub, id := filesSetup(t)
			svc.store.mu.Lock()
			sandboxID := svc.store.leases[id].SandboxID
			svc.store.mu.Unlock()
			filesSeed(t, sub, sandboxID, tc.seeds, tc.dirs)
			if tc.subFail != "" {
				sub.FailCall(tc.subFail, 0, tc.subErr)
			}
			if tc.busy {
				svc.store.mu.Lock()
				svc.store.leases[id].busy = true
				svc.store.mu.Unlock()
			}
			token := tc.token
			if token == "" {
				token = "token-a"
			}
			resp := filesDo(t, tc.method, filesURL(ts, id, tc.path, tc.query), token, tc.body)
			body := filesDecode(t, resp)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %v", resp.StatusCode, tc.want, body)
			}
			if got, _ := body["code"].(string); got != tc.code {
				t.Fatalf("code = %q, want %q (body %v)", got, tc.code, body)
			}
			if tc.code == "lease_busy" && resp.Header.Get("Retry-After") == "" {
				t.Fatal("the lease_busy answer lost its Retry-After")
			}
		})
	}
}

// TestFilesErrorCodesTransferCap pins the transfer-cap 429 (too_busy) on
// the API surface: with the one-slot pool held, the next transfer is
// refused with the code.
func TestFilesErrorCodesTransferCap(t *testing.T) {
	s := &Server{fileXfer: make(chan struct{}, 1), svc: &Service{}}
	if !s.acquireXfer(httptest.NewRecorder()) {
		t.Fatal("first transfer refused")
	}
	rec := httptest.NewRecorder()
	if s.acquireXfer(rec) {
		t.Fatal("second transfer admitted")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("transfer cap = %d, want 429", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body["code"] != "too_busy" {
		t.Fatalf("code = %v, want too_busy", body["code"])
	}
}
