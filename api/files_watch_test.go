package api

// File watch SSE tests: the handler at api/files_watch.go over the fake
// substrate, driven through a real HTTP server (httptest) with real
// SSE lines.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// watchOpen opens a watch and returns the response with its body wrapped
// for line reading. The handler has already answered 200 with the SSE
// headers by the time the client's Do returns only if something was
// flushed; the caller waits for the ready line first thing.
func watchOpen(t *testing.T, ts *httptest.Server, token, id, path, query string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	url := ts.URL + "/api/leases/" + id + "/files" + path + "?watch=1"
	if query != "" {
		url += "&" + query
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET watch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("GET watch: %d %s", resp.StatusCode, b)
	}
	// The caller cancels to hang the stream up (a body Close alone also
	// does, but an explicit cancel is deterministic).
	return resp, bufio.NewReader(resp.Body), cancel
}

// watchLines decodes SSE data lines as they arrive.
func watchLines(t *testing.T, rd *bufio.Reader) <-chan fileWatchEvent {
	t.Helper()
	out := make(chan fileWatchEvent, 16)
	go func() {
		defer close(out)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			if strings.HasPrefix(line, "data: ") {
				var ev fileWatchEvent
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
					out <- ev
				}
			}
		}
	}()
	return out
}

// waitWatchData reads one data event with a deadline.
func waitWatchData(t *testing.T, lines <-chan fileWatchEvent) fileWatchEvent {
	t.Helper()
	select {
	case ev, ok := <-lines:
		if !ok {
			t.Fatal("watch stream ended before the expected event")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("watch event did not arrive within 10s")
		return fileWatchEvent{}
	}
}

// expectNoWatchData pins that no event arrives within its window.
func expectNoWatchData(t *testing.T, lines <-chan fileWatchEvent) {
	t.Helper()
	select {
	case ev, ok := <-lines:
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(250 * time.Millisecond):
	}
}

// watchSandbox is the setup for handler tests: a running lease with a
// /work directory on its sandbox. It answers the lease id (for URLs and
// the API) and the sandbox id (for direct substrate calls in tests).
func watchSandbox(t *testing.T) (*httptest.Server, *Service, *testSub, string, string) {
	t.Helper()
	ts, svc, _, sub := newTestServerWithService(t)
	id := filesCreate(t, ts, false)
	svc.store.mu.Lock()
	sbxID := svc.store.leases[id].SandboxID
	svc.store.mu.Unlock()
	if err := sub.MakeDir(context.Background(), sbxID, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	return ts, svc, sub, id, sbxID
}

// The live SSE check: a file write produces a change event on the
// stream, after the ready event.
func TestFileWatchSSELive(t *testing.T) {
	ts, _, sub, id, sbxID := watchSandbox(t)
	resp, rd, cancel := watchOpen(t, ts, "token-a", id, "/work", "recursive=true")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	if ev := waitWatchData(t, lines); ev.Kind != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	if err := sub.WriteFile(context.Background(), sbxID, "/work/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ev := waitWatchData(t, lines)
	if ev.Kind != "change" || len(ev.Paths) != 1 || ev.Paths[0] != "/work/f.txt" {
		t.Fatalf("change = %+v, want one /work/f.txt", ev)
	}
}

// Recursive false: only direct children report.
func TestFileWatchNonRecursive(t *testing.T) {
	ts, _, sub, id, sbxID := watchSandbox(t)
	resp, rd, cancel := watchOpen(t, ts, "token-a", id, "/work", "recursive=false")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	waitWatchData(t, lines) // ready
	if err := sub.WriteFile(context.Background(), sbxID, "/work/deep/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile deep: %v", err)
	}
	expectNoWatchData(t, lines)
	if err := sub.WriteFile(context.Background(), sbxID, "/work/top.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile top: %v", err)
	}
	if ev := waitWatchData(t, lines); ev.Kind != "change" || len(ev.Paths) != 1 || ev.Paths[0] != "/work/top.txt" {
		t.Fatalf("change = %+v, want one /work/top.txt", ev)
	}
}

// A missing path is 404 and a path that is a file is 409, both before
// any stream opens.
func TestFileWatchBadPaths(t *testing.T) {
	ts, _, sub, id, sbxID := watchSandbox(t)
	if err := sub.WriteFile(context.Background(), sbxID, "/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	resp, doc := doReq(t, "GET", filesURL(ts, id, "/no/such", "watch=1"), "token-a", "")
	if resp.StatusCode != 404 {
		t.Fatalf("missing: %d %v", resp.StatusCode, doc)
	}
	resp2, doc2 := doReq(t, "GET", filesURL(ts, id, "/f.txt", "watch=1"), "token-a", "")
	if resp2.StatusCode != 409 {
		t.Fatalf("a file: %d %v", resp2.StatusCode, doc2)
	}
}

// A watch holds an exec/stream slot for its lifetime: a full cap 429s a
// further watcher (and the release frees it again).
func TestFileWatchSlot429(t *testing.T) {
	t.Setenv("EXEC_CONCURRENCY_MAX", "1")
	ts, _, _, id, _ := watchSandbox(t)
	resp1, rd1, cancel1 := watchOpen(t, ts, "token-a", id, "/work", "")
	defer resp1.Body.Close()
	defer cancel1()
	lines1 := watchLines(t, rd1)
	waitWatchData(t, lines1) // ready, the slot is held
	resp2 := filesDo(t, "GET", filesURL(ts, id, "/work", "watch=1"), "token-a", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second watcher: %d %s", resp2.StatusCode, filesBody(t, resp2))
	}
	// Exec rides the same pool: it 429s too.
	execBody := strings.NewReader(fmt.Sprintf(`{"argv":["true"]}`))
	req, _ := http.NewRequest("POST", ts.URL+"/api/leases/"+id+"/exec", execBody)
	req.Header.Set("Authorization", "Bearer token-a")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exec while watching: %d", resp3.StatusCode)
	}
}

// A watch is not activity: an idle lease with an open watch suspends on
// schedule and the watch ends with lease_suspended.
func TestFileWatchNotActivity(t *testing.T) {
	ts, svc, sub, _, _ := watchSandbox(t)
	// The lease needs idle_suspend above 0 and age past it: create it
	// persistent (the sweeper only idles persistent leases) with a policy
	// on it.
	respC, doc := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if respC.StatusCode != 201 {
		t.Fatalf("create: %d %v", respC.StatusCode, doc)
	}
	pid := doc["id"].(string)
	svc.store.mu.Lock()
	sbxID := svc.store.leases[pid].SandboxID
	svc.store.mu.Unlock()
	if err := sub.MakeDir(context.Background(), sbxID, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if _, err := svc.setIdlePolicy(svc.lookup("consumer-a", pid), 1); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	resp, rd, cancel := watchOpen(t, ts, "token-a", pid, "/work", "")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	if ev := waitWatchData(t, lines); ev.Kind != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	// Age the lease past its idle window and run the sweep. A watch
	// holds no activity marker; the suspend must proceed.
	svc.store.mu.Lock()
	l := svc.store.leases[pid]
	base := l.LastActive
	svc.store.mu.Unlock()
	svc.suspendIdleLeases(context.Background(), base.Add(2*time.Minute))
	svc.store.mu.Lock()
	suspended := svc.store.leases[pid].Suspended
	svc.store.mu.Unlock()
	if !suspended {
		t.Fatal("an open watch kept the lease from idle-suspending")
	}
	// Either the suspended event or the lease check ends the stream with
	// lease_suspended.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-lines:
			if !ok {
				t.Fatal("stream ended without the terminal error")
			}
			if ev.Kind == "error" && ev.Code == "lease_suspended" {
				return
			}
			if ev.Kind == "error" {
				t.Fatalf("terminal = %+v, want lease_suspended", ev)
			}
		case <-deadline:
			t.Fatal("lease_suspended never arrived")
		}
	}
}

// A released lease ends the watch with lease_gone.
func TestFileWatchLeaseGone(t *testing.T) {
	ts, _, _, id, _ := watchSandbox(t)
	resp, rd, cancel := watchOpen(t, ts, "token-a", id, "/work", "")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	if ev := waitWatchData(t, lines); ev.Kind != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	if respDel := filesDo(t, "DELETE", ts.URL+"/api/leases/"+id, "token-a", ""); respDel.StatusCode != 200 && respDel.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(respDel.Body)
		respDel.Body.Close()
		t.Fatalf("release: %d %s", respDel.StatusCode, b)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-lines:
			if !ok {
				t.Fatal("stream ended without the terminal error")
			}
			if ev.Kind == "error" && ev.Code == "lease_gone" {
				return
			}
			if ev.Kind == "error" {
				t.Fatalf("terminal = %+v, want lease_gone", ev)
			}
		case <-deadline:
			t.Fatal("lease_gone never arrived")
		}
	}
}

// Coalescing: paths of a burst inside the window land as one event.
func TestFileWatchCoalesce(t *testing.T) {
	old := fileWatchPace.coalesce
	fileWatchPace.coalesce = 400 * time.Millisecond
	defer func() { fileWatchPace.coalesce = old }()

	ts, _, sub, id, sbxID := watchSandbox(t)
	resp, rd, cancel := watchOpen(t, ts, "token-a", id, "/work", "")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	if ev := waitWatchData(t, lines); ev.Kind != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	// Three writes in one window become one event, deduplicated and
	// sorted, a repeat write one path.
	for _, name := range []string{"z.txt", "a.txt", "z.txt"} {
		if err := sub.WriteFile(context.Background(), sbxID, "/work/"+name, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if ev := waitWatchData(t, lines); ev.Kind != "change" ||
		len(ev.Paths) != 2 || ev.Paths[0] != "/work/a.txt" || ev.Paths[1] != "/work/z.txt" {
		t.Fatalf("coalesced = %+v, want [a.txt z.txt]", ev)
	}
	expectNoWatchData(t, lines)
}

// A lost lease ends the watch with lease_lost.
func TestFileWatchLeaseLost(t *testing.T) {
	ts, svc, _, id, _ := watchSandbox(t)
	resp, rd, cancel := watchOpen(t, ts, "token-a", id, "/work", "")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	if ev := waitWatchData(t, lines); ev.Kind != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	// markLost runs under the store lock (its callers hold it).
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	svc.markLost(l, "substrate lost the sandbox")
	svc.store.mu.Unlock()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-lines:
			if !ok {
				t.Fatal("stream ended without the terminal error")
			}
			if ev.Kind == "error" && ev.Code == "lease_lost" {
				return
			}
			if ev.Kind == "error" {
				t.Fatalf("terminal = %+v, want lease_lost", ev)
			}
		case <-deadline:
			t.Fatal("lease_lost never arrived")
		}
	}
}

// A hand suspend ends the watch with lease_suspended as well (the idle
// sweep is not the only pause).
func TestFileWatchHandSuspend(t *testing.T) {
	ts, svc, sub, _, _ := watchSandbox(t)
	respC, doc := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if respC.StatusCode != 201 {
		t.Fatalf("create: %d %v", respC.StatusCode, doc)
	}
	pid := doc["id"].(string)
	svc.store.mu.Lock()
	sbxID := svc.store.leases[pid].SandboxID
	svc.store.mu.Unlock()
	if err := sub.MakeDir(context.Background(), sbxID, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	resp, rd, cancel := watchOpen(t, ts, "token-a", pid, "/work", "")
	defer resp.Body.Close()
	defer cancel()
	lines := watchLines(t, rd)
	if ev := waitWatchData(t, lines); ev.Kind != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	if r := filesDo(t, "POST", ts.URL+"/api/leases/"+pid+"/suspend", "token-a", ""); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		t.Fatalf("suspend: %d %s", r.StatusCode, b)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-lines:
			if !ok {
				t.Fatal("stream ended without the terminal error")
			}
			if ev.Kind == "error" && ev.Code == "lease_suspended" {
				return
			}
			if ev.Kind == "error" {
				t.Fatalf("terminal = %+v, want lease_suspended", ev)
			}
		case <-deadline:
			t.Fatal("lease_suspended never arrived")
		}
	}
}
