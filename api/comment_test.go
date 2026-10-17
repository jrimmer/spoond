package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestValidateCommentRules pins the shared comment validator
// directly: the cap, the control-character refusal (anything below
// 0x20 — a newline included — and 0x7F), and the plain text /
// non-ASCII passes.
func TestValidateCommentRules(t *testing.T) {
	cases := []struct {
		name    string
		comment string
		want    string // "" = accepted
	}{
		{"empty clears", "", ""},
		{"plain text", "integration demo box", ""},
		{"utf-8 printable", "é ✓", ""},
		{"tab is a control char", "a\tb", "comment must not contain control characters"},
		{"newline", "a\nb", "comment must not contain control characters"},
		{"carriage return", "a\rb", "comment must not contain control characters"},
		{"nul", "a\x00b", "comment must not contain control characters"},
		{"0x1f", "a\x1fb", "comment must not contain control characters"},
		{"escape 0x1b", "a\x1b[31mb", "comment must not contain control characters"},
		{"del 0x7f", "a\x7fb", "comment must not contain control characters"},
		{"512 ok", strings.Repeat("x", 512), ""},
		{"513 over the cap", strings.Repeat("x", 513), "comment must be <= 512 chars"},
	}
	for _, tc := range cases {
		err := validateComment(tc.comment)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: validateComment(%q) = %v, want %q", tc.name, tc.comment, err, tc.want)
		}
	}
}

// TestCommentRoutesRejectControlChars: both routes that take a lease
// comment — the create body's `comment` field and
// POST /api/leases/{id}/comment — refuse control characters (a newline
// and a 0x1b escape) with the shared 400, and accept plain and UTF-8
// text.
func TestCommentRoutesRejectControlChars(t *testing.T) {
	ts, _ := newTestServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 60, "persistent": true})
	if create["id"] == nil {
		t.Fatalf("create failed: %v", create)
	}
	id := create["id"].(string)

	// Both routes that carry a comment, over the same cases.
	createPath, setCommentPath := "/api/leases", "/api/leases/"+id+"/comment"
	bad := []struct {
		name  string
		value string
	}{
		{"newline", "line one\nline two"},
		{"tab", "a\tb"},
		{"escape 0x1b", "bad\x1b[31mcolor"},
		{"del 0x7f", "ok\x7fdone"},
	}
	for _, tc := range bad {
		// The create body's comment field.
		resp, body := doReq(t, "POST", ts.URL+createPath, "token-a",
			map[string]any{"image": "py-base", "ttl": 60, "comment": tc.value})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s create comment %q: status %d, want 400", createPath, tc.name, resp.StatusCode)
		} else if body["error"] != "comment must not contain control characters" {
			t.Errorf("%s create comment %q: error %v, want the control-character message", createPath, tc.name, body["error"])
		}
		// The comment route on a lease that already exists.
		resp, body = doReq(t, "POST", ts.URL+setCommentPath, "token-a", map[string]any{"comment": tc.value})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s set comment %q: status %d, want 400", setCommentPath, tc.name, resp.StatusCode)
		} else if body["error"] != "comment must not contain control characters" {
			t.Errorf("%s set comment %q: error %v, want the control-character message", setCommentPath, tc.name, body["error"])
		}
	}
	// Nothing was started for the refused creates; the existing
	// lease keeps its comment.
	createID, ok := create["id"].(string)
	if !ok || createID != id {
		t.Fatalf("unexpected create id %v", create["id"])
	}

	// Plain text and non-ASCII printable text pass both routes.
	good := []string{"forgejo job 42 https://code.example.com/jobs/42", "é ✓"}
	for _, g := range good {
		resp, body := doReq(t, "POST", ts.URL+setCommentPath, "token-a", map[string]any{"comment": g})
		if resp.StatusCode != http.StatusOK || body["comment"] != g {
			t.Errorf("%s set comment %q: status %d body %v, want 200 with the comment echoed", setCommentPath, g, resp.StatusCode, body)
		}
		resp, createBody := doReq(t, "POST", ts.URL+createPath, "token-a",
			map[string]any{"image": "py-base", "ttl": 60, "comment": g})
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("%s create comment %q: status %d, want 201", createPath, g, resp.StatusCode)
		}
		if cid, _ := createBody["id"].(string); cid != "" {
			_, got := doReq(t, "GET", ts.URL+createPath+"/"+cid, "token-a", nil)
			if got["comment"] != g {
				t.Errorf("%s create comment %q: lease carries %v", createPath, g, got["comment"])
			}
		}
	}
}
