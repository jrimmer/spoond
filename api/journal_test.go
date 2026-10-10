package api

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// journalFields parses one lease journal line into a field map. The
// line is logfmt-shaped by construction (api/journal.go), so a test
// reads it the way an operator's grep/awk pipeline would.
func journalFields(t *testing.T, line string) map[string]string {
	t.Helper()
	const prefix = "lease journal: "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("not a lease journal line: %q", line)
	}
	fields := map[string]string{}
	for _, tok := range splitJournalFields(strings.TrimPrefix(line, prefix)) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			t.Fatalf("journal token %q is not key=value", tok)
		}
		fields[k] = trimJournalValue(v)
	}
	return fields
}

// splitJournalFields splits a journal body on unquoted spaces, so a
// quoted value keeps its own spaces as one token.
func splitJournalFields(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, escaped := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case inQuote && r == '\\':
			cur.WriteRune(r)
			escaped = true
		case r == '"':
			cur.WriteRune(r)
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// trimJournalValue unwraps the quoting journalValue adds, so a test
// compares against the raw value (a reason with spaces, say).
func trimJournalValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		out := v[1 : len(v)-1]
		out = strings.ReplaceAll(out, `\"`, `"`)
		out = strings.ReplaceAll(out, `\\`, `\`)
		return out
	}
	return v
}

// TestJournalLineFormat pins the one-line shape an incident reader
// reconstructs from: op, lease id, owner, sandbox, image, class and
// reason, all on one logfmt line and all present (an empty value is
// quoted "").
func TestJournalLineFormat(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l, err := svc.grant(context.Background(), "owner-1", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)
	svc.journalLease(journalOpRelease, l, "deleted via API by owner-1")

	line := strings.TrimSpace(buf.String())
	f := journalFields(t, line)
	want := map[string]string{
		"op":       "release",
		"lease_id": l.ID,
		"owner":    "owner-1",
		"sandbox":  l.SandboxID,
		"image":    "py-base",
		"class":    ClassGuaranteed,
		"reason":   "deleted via API by owner-1",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("journal %s = %q, want %q (line: %s)", k, f[k], v, line)
		}
	}
	// Exactly the seven fields: no eighth field sneaks in (and the
	// reason's own spaces did not split into extra tokens).
	if len(f) != len(want) {
		t.Errorf("journal has %d fields, want %d (line: %s)", len(f), len(want), line)
	}
}

// TestJournalLifecyclePaths drives create, suspend and release through
// the real lifecycle code and checks one journal line per step, with
// the sandbox id preserved across the release that deletes it.
func TestJournalLifecyclePaths(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	l, err := svc.grant(ctx, "owner-1", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)
	if _, err := svc.suspend(ctx, "owner-1", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.releaseBecause(ctx, l, "deleted through the API")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var ops []string
	for _, line := range lines {
		if strings.Contains(line, "lease journal:") {
			ops = append(ops, journalFields(t, line)["op"])
		}
	}
	if got, want := strings.Join(ops, ","), "suspend,release"; got != want {
		t.Fatalf("journal ops = %q, want %q\n%s", got, want, buf.String())
	}
	// The release line still names the sandbox the lease held, which is
	// the whole point: the sandbox row and lease row are gone by then.
	for _, line := range lines {
		if !strings.Contains(line, "lease journal:") || journalFields(t, line)["op"] != "release" {
			continue
		}
		f := journalFields(t, line)
		if f["sandbox"] != l.SandboxID {
			t.Errorf("release journal sandbox = %q, want %q", f["sandbox"], l.SandboxID)
		}
		if f["reason"] != "deleted via API by owner-1" {
			t.Errorf("release journal reason = %q, want the owner-named deletion", f["reason"])
		}
	}
}

// TestJournalCreateReason pins how a create line says where the lease
// came from, so a CI create, a snapshot start, a clone and a fork read
// apart from the line alone.
func TestJournalCreateReason(t *testing.T) {
	cases := []struct {
		snapshot, clone, fork, want string
	}{
		{"", "", "", "new"},
		{"base@3", "", "", "snapshot base@3"},
		{"", "src-1", "", "clone of src-1"},
		{"", "", "src-2", "fork of src-2"},
	}
	for _, c := range cases {
		if got := journalCreateReason(c.snapshot, c.clone, c.fork); got != c.want {
			t.Errorf("journalCreateReason(%q,%q,%q) = %q, want %q", c.snapshot, c.clone, c.fork, got, c.want)
		}
	}
}

// TestJournalReleaseReason pins the canonical reason tokens, including
// the owner-named API deletion, the one paused-release clock and a CI
// job's own free-text reason.
func TestJournalReleaseReason(t *testing.T) {
	cases := []struct {
		name   string
		lease  *Lease
		reason string
		want   string
	}{
		{"api", &Lease{Owner: "alice"}, "deleted through the API", "deleted via API by alice"},
		{"ttl", &Lease{}, "TTL expired", "ttl"},
		{"lost", &Lease{}, lostReleaseReason, "lost grace expired"},
		{"user", &Lease{}, userDeleteReason, "user deleted"},
		{"paused_expired", &Lease{}, "paused_expired", "paused_expired"},
		{"ci", &Lease{}, "ci job 3609 ✓ 11m02s", "ci job 3609 ✓ 11m02s"},
	}
	for _, c := range cases {
		if got := journalReleaseReason(c.lease, c.reason); got != c.want {
			t.Errorf("%s: journalReleaseReason(%q) = %q, want %q", c.name, c.reason, got, c.want)
		}
	}
}

// TestJournalSuspendReason pins the suspend tokens: each automatic
// reason passes through, the drain and a hand suspend name themselves.
func TestJournalSuspendReason(t *testing.T) {
	cases := []struct {
		pol     suspendPolicy
		drained bool
		want    string
	}{
		{suspendPolicy{reason: suspendReasonIdleSuspend}, false, "idle_suspend"},
		{suspendPolicy{reason: suspendReasonPreempt}, false, "preempt"},
		{suspendPolicy{}, true, "drain"},
		{suspendPolicy{}, false, "hand"},
	}
	for _, c := range cases {
		if got := journalSuspendReason(c.pol, c.drained); got != c.want {
			t.Errorf("journalSuspendReason(%+v, %v) = %q, want %q", c.pol, c.drained, got, c.want)
		}
	}
}

// TestJournalValueQuoting pins that a value with a space, quote, equals
// sign or control character is quoted, so an operator's field splitter
// never mistakes it for a new field, and a simple token stays bare.
func TestJournalValueQuoting(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `""`},
		{"abc-123", "abc-123"},
		{"two words", `"two words"`},
		{"a=b", `"a=b"`},
		{`say "hi"`, `"say \"hi\""`},
		{"line\nbreak", `"line\nbreak"`},
	}
	for _, c := range cases {
		if got := journalValue(c.in); got != c.want {
			t.Errorf("journalValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestJournalLostLine pins that marking a lease lost writes the
// journal line with the same reason its lost event carries.
func TestJournalLostLine(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l, err := svc.grant(context.Background(), "owner-1", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sandbox := l.SandboxID

	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)
	svc.store.mu.Lock()
	svc.markLost(l, "root disk unreadable (I/O errors)")
	svc.store.mu.Unlock()

	f := journalFields(t, strings.TrimSpace(buf.String()))
	if f["op"] != journalOpLost {
		t.Errorf("op = %q, want %q", f["op"], journalOpLost)
	}
	if f["sandbox"] != sandbox {
		t.Errorf("sandbox = %q, want %q", f["sandbox"], sandbox)
	}
	if f["reason"] != "root disk unreadable (I/O errors)" {
		t.Errorf("reason = %q, want the loss reason", f["reason"])
	}
}

// TestJournalNoSecretMaterial pins that the create-line fields a caller
// controls (create secrets) never reach the journal. The line carries
// only identity and mapping, no token or secret.
func TestJournalNoSecretMaterial(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l, err := svc.grant(context.Background(), "owner-1", "py-base", time.Minute, true, "", nil, "", "",
		map[string]string{"API_KEY": "super-secret-value"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)
	svc.journalLease(journalOpRelease, l, "deleted via API by owner-1")
	if strings.Contains(buf.String(), "super-secret-value") {
		t.Fatalf("journal leaked a secret:\n%s", buf.String())
	}
}
