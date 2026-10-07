package spoondgateway

import (
	"strings"
	"testing"
)

// TestSnapshotSaveBody pins the `snapshot save` argument parsing: the
// name, --key and --keep map onto the save body, and malformed input is
// refused.
func TestSnapshotSaveBody(t *testing.T) {
	t.Run("name only", func(t *testing.T) {
		body, errMsg := snapshotSaveBody([]string{"save", "lease-id", "spoond/warm"})
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if body["name"] != "spoond/warm" {
			t.Fatalf("name = %v", body["name"])
		}
		if _, ok := body["idempotency_key"]; ok {
			t.Fatalf("idempotency_key should be absent, got %v", body["idempotency_key"])
		}
	})
	t.Run("key and keep", func(t *testing.T) {
		body, errMsg := snapshotSaveBody([]string{"save", "l", "warm", "--key", "fl/1", "--keep", "5"})
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if body["idempotency_key"] != "fl/1" || body["keep"] != 5 {
			t.Fatalf("body = %v", body)
		}
	})
	t.Run("keep order independent", func(t *testing.T) {
		body, errMsg := snapshotSaveBody([]string{"save", "l", "warm", "--keep", "2", "--key", "k"})
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if body["keep"] != 2 || body["idempotency_key"] != "k" {
			t.Fatalf("body = %v", body)
		}
	})
	t.Run("missing name", func(t *testing.T) {
		if _, errMsg := snapshotSaveBody([]string{"save", "lease"}); errMsg == "" {
			t.Fatal("want a usage error")
		}
	})
	t.Run("bad keep", func(t *testing.T) {
		if _, errMsg := snapshotSaveBody([]string{"save", "l", "w", "--keep", "x"}); errMsg == "" {
			t.Fatal("want a keep error")
		}
	})
	t.Run("unknown option", func(t *testing.T) {
		if _, errMsg := snapshotSaveBody([]string{"save", "l", "w", "--nope"}); errMsg == "" {
			t.Fatal("want an unknown-option error")
		}
	})
}

// TestNamedSnapshotPaths pins the list/show/delete URL construction: the
// prefix is escaped, a slash in the name is left for the wildcard route,
// and force adds the query.
func TestNamedSnapshotPaths(t *testing.T) {
	if got := namedSnapshotsPath(""); got != "/api/named-snapshots" {
		t.Fatalf("no prefix: %q", got)
	}
	if got := namedSnapshotsPath("spoond/"); got != "/api/named-snapshots?prefix=spoond%2F" {
		t.Fatalf("prefix: %q", got)
	}
	if got := namedSnapshotPath("spoond/warm@3", false); got != "/api/named-snapshots/spoond/warm@3" {
		t.Fatalf("show: %q", got)
	}
	if got := namedSnapshotPath("spoond/warm", true); got != "/api/named-snapshots/spoond/warm?force=1" {
		t.Fatalf("force: %q", got)
	}
}

// TestParseCreateArgs pins the `create` body: an image or a snapshot is
// required, --snapshot sets the snapshot field, --ttl overrides the
// default and --persistent toggles persistence.
func TestParseCreateArgs(t *testing.T) {
	t.Run("snapshot", func(t *testing.T) {
		body, errMsg := parseCreateArgs([]string{"--snapshot", "spoond/warm@3"})
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if body["snapshot"] != "spoond/warm@3" {
			t.Fatalf("snapshot = %v", body["snapshot"])
		}
		if _, ok := body["image"]; ok {
			t.Fatalf("image should be absent, got %v", body["image"])
		}
	})
	t.Run("image and ttl", func(t *testing.T) {
		body, errMsg := parseCreateArgs([]string{"py-base", "--ttl", "120"})
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if body["image"] != "py-base" || body["ttl"] != 120 {
			t.Fatalf("body = %v", body)
		}
	})
	t.Run("ephemeral", func(t *testing.T) {
		body, errMsg := parseCreateArgs([]string{"py-base", "--ephemeral"})
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if body["persistent"] != false {
			t.Fatalf("persistent = %v", body["persistent"])
		}
	})
	t.Run("nothing requested", func(t *testing.T) {
		if _, errMsg := parseCreateArgs(nil); errMsg == "" {
			t.Fatal("want a usage error")
		}
	})
	t.Run("two images", func(t *testing.T) {
		if _, errMsg := parseCreateArgs([]string{"a", "b"}); errMsg == "" {
			t.Fatal("want an unexpected-argument error")
		}
	})
}

// TestPrettySnapshots pins the list rendering: names group versions,
// newest first, and empty answers read "no snapshots".
func TestPrettySnapshots(t *testing.T) {
	b := []byte(`{"snapshots":[
		{"name":"spoond/warm","latest":2,"versions":[
			{"version":1,"build_id":"0123456789abcdef","image":"py-base","memory_mb":2048,"size_bytes":1073741824,"created_at":"2026-10-06T09:12:30Z","in_use":0,"stale":false},
			{"version":2,"build_id":"feedfacefeedface","image":"py-base","memory_mb":2048,"size_bytes":2097152,"created_at":"2026-10-06T10:00:00Z","in_use":1,"stale":true}]}
	]}`)
	out := prettySnapshots(b)
	for _, want := range []string{"NAME", "VERSION", "spoond/warm", "2 GiB", "yes", "no"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	// Newest (version 2) must appear before version 1.
	if strings.Index(out, "feedfacefeed…") > strings.Index(out, "0123456789ab…") {
		t.Fatalf("versions not newest-first:\n%s", out)
	}
	if got := prettySnapshots([]byte(`{"snapshots":[]}`)); got != "no snapshots" {
		t.Fatalf("empty: %q", got)
	}
	// Not our shape passes through.
	raw := `{"error":"boom"}`
	if got := prettySnapshots([]byte(raw)); got != raw {
		t.Fatalf("passthrough: %q", got)
	}
}

// TestPrettySnapshotDetail pins the show rendering.
func TestPrettySnapshotDetail(t *testing.T) {
	b := []byte(`{"name":"spoond/warm","version":3,"build_id":"abc","image":"go-base","memory_mb":4096,"size_bytes":2254857830,"created_at":"2026-10-06T09:12:30Z","in_use":2,"stale":false}`)
	out := prettySnapshotDetail(b)
	for _, want := range []string{"spoond/warm@3", "build_id : abc", "image    : go-base", "in use   : 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

// TestHumanBytes pins the size formatting used by the table and detail.
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "-", 512: "512 B", 1073741824: "1.0 GiB", 2254857830: "2.1 GiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if got := humanMiB(4096); got != "4 GiB" {
		t.Fatalf("humanMiB(4096) = %q", got)
	}
}
