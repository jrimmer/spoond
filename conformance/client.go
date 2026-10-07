//go:build conformance

// Package conformance is spoond's substrate conformance suite (U02). It
// exercises spoond through the lease API — the compatibility contract —
// plus a few host-level checks. Every file in this package carries the
// build tag "conformance", so a plain `go test ./...` never builds it.
// See README.md for how to run it.
package conformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// config is the suite configuration, read from the environment (U02
// §Configuration). Everything except the per-run variables comes from an
// env file loaded with `set -a; . <file>; set +a`.
type config struct {
	API          string
	Token        string
	SSH          string
	Substrate    string
	BackendUnit  string
	Destructive  bool
	Images       []string
	SSHGateway   string
	SSHKey       string
	User         string
	UserID       string
	ProxyURL     string
	ProxySecret  string
	ProxySuffix  string
	GuestService string
	// MixedPrivate is a private (LAN) IP reachable from a restricted lease
	// that allowlists it together with a public domain (TestN9). It has no
	// default: vm2 sets CONFORMANCE_MIXED_PRIVATE and the case skips when
	// it is unset, so a non-vm2 run never probes an assumed LAN address.
	// MixedPrivatePort is its TLS port (default 443).
	MixedPrivate     string
	MixedPrivatePort int
	// MixedDomain is a public domain reachable from the same restricted
	// lease (TestN9). Defaults to example.com when unset.
	MixedDomain string
	// MixedBlockedPrivate is a private IP that the same lease does not
	// allowlist; connections to it must be blocked (TestN9). It has no
	// default and skips with MixedPrivate when unset.
	MixedBlockedPrivate string
	// MixedPrivateDomain is a domain that resolves to MixedPrivate. It is
	// added to the same restricted allowlist and probed with SNI
	// (TestN9), covering the fork's domain path. It has no default; unset
	// skips only this probe.
	MixedPrivateDomain string
	// MixedSSHPort is a non-TLS port on MixedPrivate, probed with a plain
	// TCP connect under the same restricted allowlist (TestN9), covering
	// the IP path for a non-443 destination. Default 22; 0 skips the
	// probe.
	MixedSSHPort int
	// SecondToken is the token of a second, non-admin identity user
	// (CONFORMANCE_SECOND_TOKEN). Group C uses it as the owner of the
	// guaranteed lease: the production conformance user is not admin and
	// there is no promote API, so the suite cannot create a second user
	// there.
	SecondToken string
}

// client is a small HTTP client for the lease API (U02 §Harness
// requirements). TLS verification is skipped because the backend serves a
// self-signed cert on the host (docs/security.md), matching every other spoond
// client (gateway, tests/integration/wsclient).
type client struct {
	base, token, proxyURL, proxyUser, proxySecret, proxySuffix string
	hc                                                         *http.Client
}

func newClient(cfg config) *client {
	return &client{
		base:        strings.TrimRight(cfg.API, "/"),
		token:       cfg.Token,
		proxyURL:    strings.TrimRight(cfg.ProxyURL, "/"),
		proxyUser:   cfg.User,
		proxySecret: cfg.ProxySecret,
		proxySuffix: cfg.ProxySuffix,
		hc: &http.Client{
			Timeout:   360 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
}

// do performs one authenticated API call and returns the status and body.
func (c *client) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (c *client) create(body map[string]any) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes", body)
}

func (c *client) exec(id string, body execReq) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/exec", body)
}

func (c *client) delete(id string) (int, []byte, error) {
	return c.do("DELETE", "/api/sandboxes/"+id, nil)
}

// filePut uploads raw bytes to the lease file at path (guest-absolute).
func (c *client) filePut(id, path string, data []byte, query string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPut, c.base+"/api/leases/"+id+"/files"+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if query != "" {
		req.URL.RawQuery = query
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// fileGet downloads the lease file at path (guest-absolute).
func (c *client) fileGet(id, path, query string) (int, []byte, error) {
	return c.do("GET", "/api/leases/"+id+"/files"+path+"?"+query, nil)
}

// fileMkdir creates a directory (and missing parents) at path.
func (c *client) fileMkdir(id, path, query string) (int, []byte, error) {
	if query == "" {
		query = "op=mkdir"
	}
	return c.do("POST", "/api/leases/"+id+"/files"+path+"?"+query, nil)
}

// fileDelete removes the path; recursive for directories.
func (c *client) fileDelete(id, path, query string) (int, []byte, error) {
	return c.do("DELETE", "/api/leases/"+id+"/files"+path+"?"+query, nil)
}

func (c *client) keepalive(id string, body map[string]any) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/keepalive", body)
}

func (c *client) suspend(id string) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/suspend", nil)
}

func (c *client) resume(id string) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/resume", nil)
}

func (c *client) restart(id string) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/restart", nil)
}

func (c *client) clone(id string, body map[string]any) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/clone", body)
}

func (c *client) fork(id string, body map[string]any) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/fork", body)
}

func (c *client) network(id string, body map[string]any) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/network", body)
}

func (c *client) checkpoint(id string) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/checkpoint", nil)
}

// checkpointKeep checkpoints with {"keep":true} (2.3, #121): the build
// joins the GC's kept set and is a restore point.
func (c *client) checkpointKeep(id string) (int, []byte, error) {
	return c.do("POST", "/api/sandboxes/"+id+"/checkpoint", map[string]any{"keep": true})
}

// startJob runs a command as a background job (2.6, #135).
func (c *client) startJob(id string, req execReq) (int, []byte, error) {
	req.Background = true
	return c.exec(id, req)
}

// readJob reads a job record (with optional wait seconds) and its output.
func (c *client) readJob(id, jobID string, wait int) (int, []byte, error) {
	path := "/api/leases/" + id + "/jobs/" + jobID
	if wait > 0 {
		path += "?wait=" + strconv.Itoa(wait)
	}
	return c.do("GET", path, nil)
}

// listJobs lists a lease's background jobs.
func (c *client) listJobs(id string) (int, []byte, error) {
	return c.do("GET", "/api/leases/"+id+"/jobs", nil)
}

// signalJob signals a job's process group.
func (c *client) signalJob(id, jobID, sig string) (int, []byte, error) {
	return c.do("POST", "/api/leases/"+id+"/jobs/"+jobID+"/signal", map[string]any{"signal": sig})
}

// eventsURL returns the lease event stream URL for one lease.
func (c *client) eventsURL(id string) string {
	return c.base + "/api/leases/" + id + "/events"
}

// watchEvents opens the lease's event SSE stream and returns a channel
// of event data payloads. The returned cancel function stops the stream.
// Used by the background-job conformance group to catch job_started and
// job_exited (2.6, #135).
func (c *client) watchEvents(ctx context.Context, id string) (<-chan string, func()) {
	ch := make(chan string, 64)
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.eventsURL(id), nil)
	if err != nil {
		cancel()
		close(ch)
		return ch, func() {}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	go func() {
		defer close(ch)
		resp, err := c.hc.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			select {
			case ch <- strings.TrimPrefix(line, "data: "):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, cancel
}

// restore rolls the lease in place back to the kept checkpoint build
// (2.3, #121).
func (c *client) restore(id, buildID string) (int, []byte, error) {
	return c.do("POST", "/api/leases/"+id+"/restore", map[string]any{"build_id": buildID})
}

func (c *client) stat(id string) (int, []byte, error) {
	return c.do("GET", "/api/sandboxes/"+id+"/stat", nil)
}

func (c *client) list() (int, []byte, error) {
	return c.do("GET", "/api/sandboxes", nil)
}

func (c *client) images(query string) (int, []byte, error) {
	return c.do("GET", "/api/images"+query, nil)
}

// deleteSnapshot is U11's DELETE /api/snapshots/{build_id}.
func (c *client) deleteSnapshot(buildID string) (int, []byte, error) {
	return c.do("DELETE", "/api/snapshots/"+buildID, nil)
}

// saveNamedSnapshot is POST /api/leases/{id}/snapshots (2.7, #83): save a
// live lease as a named snapshot. keep is omitted when <= 0, so the
// server default applies; key is omitted when empty.
func (c *client) saveNamedSnapshot(id, name, key string, keep int) (int, []byte, error) {
	body := map[string]any{"name": name}
	if key != "" {
		body["idempotency_key"] = key
	}
	if keep > 0 {
		body["keep"] = keep
	}
	return c.do("POST", "/api/leases/"+id+"/snapshots", body)
}

// listNamedSnapshots is GET /api/named-snapshots (?prefix=).
func (c *client) listNamedSnapshots(prefix string) (int, []byte, error) {
	path := "/api/named-snapshots"
	if prefix != "" {
		path += "?prefix=" + urlQueryEscape(prefix)
	}
	return c.do("GET", path, nil)
}

// showNamedSnapshot is GET /api/named-snapshots/{name[@v]}.
func (c *client) showNamedSnapshot(ref string) (int, []byte, error) {
	return c.do("GET", "/api/named-snapshots/"+ref, nil)
}

// deleteNamedSnapshot is DELETE /api/named-snapshots/{name[@v]} [?force=1].
func (c *client) deleteNamedSnapshot(ref string, force bool) (int, []byte, error) {
	path := "/api/named-snapshots/" + ref
	if force {
		path += "?force=1"
	}
	return c.do("DELETE", path, nil)
}

// setNamedSnapshotKeep is PUT /api/named-snapshots/{name} {"keep":N}.
func (c *client) setNamedSnapshotKeep(name string, keep int) (int, []byte, error) {
	return c.do("PUT", "/api/named-snapshots/"+name, map[string]any{"keep": keep})
}

// hostClock reads the API host's clock from a response Date header. It
// issues GET /healthz (no auth required) and parses Date per RFC 7231.
// Used by the named-snapshot case's guest-clock check.
func (c *client) hostClock() (time.Time, error) {
	req, err := http.NewRequest("GET", c.base+"/healthz", nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	date := resp.Header.Get("Date")
	if date == "" {
		return time.Time{}, fmt.Errorf("no Date header")
	}
	return http.ParseTime(date)
}

// urlQueryEscape escapes a prefix for a query string.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", ch)
	}
	return b.String()
}

// execReq is the exec body: {"cmd","cwd","env","timeout"} (A1 §5).
// Background (2.6, #135) starts the command as a tracked job.
type execReq struct {
	Cmd        string            `json:"cmd"`
	Cwd        string            `json:"cwd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
	Background bool              `json:"background,omitempty"`
}

// jobInfo is the background-job record shape (2.6, #135).
type jobInfo struct {
	JobID      string `json:"job_id"`
	LeaseID    string `json:"lease_id"`
	Cmd        string `json:"cmd"`
	State      string `json:"state"`
	ExitCode   *int   `json:"exit_code"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`
	StderrTail string `json:"stderr_tail"`
}

// jobStart is the POST .../exec "background": true answer.
type jobStart struct {
	JobID     string `json:"job_id"`
	StartedAt string `json:"started_at"`
}

// jobRead is the GET .../jobs/{job} answer.
type jobRead struct {
	Job    jobInfo `json:"job"`
	Stdout string  `json:"stdout"`
	Stderr string  `json:"stderr"`
}

// execResult is the exec response: {"stdout","stderr","exit"}.
type execResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

// leaseInfo is the subset of lease JSON the suite reads, including the
// U10 "state", U11 "resume_build_id" and 2.2 "generation" additions.
type leaseInfo struct {
	ID            string            `json:"id"`
	Owner         string            `json:"owner"`
	Address       string            `json:"address"`
	Image         string            `json:"image"`
	TTL           int               `json:"ttl"`
	Persistent    bool              `json:"persistent"`
	ExpiresAt     string            `json:"expires_at"`
	Exposed       map[string]string `json:"exposed"`
	State         string            `json:"state"`
	ResumeBuildID string            `json:"resume_build_id"`
	Generation    int64             `json:"generation"`
	Name          string            `json:"name"`
	Class         string            `json:"class"`
	Preempted     bool              `json:"preempted"`
	// WaitedMS is how long a queued create waited for admission
	// (#129 part 1); absent (0) when it answered at once.
	WaitedMS int64 `json:"waited_ms"`
	// Snapshot is the resolved named-snapshot version a lease started
	// from (2.7, #83 A3), or nil.
	Snapshot *snapshotRef `json:"snapshot"`
}

// snapshotRef is the {"name","version","build_id"} object a lease
// carries when it started from a named snapshot (A3).
type snapshotRef struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
	BuildID string `json:"build_id"`
}

// statResult is the /stat response shape (A1 §5.9).
type statResult struct {
	CPU struct {
		Load1 float64 `json:"load1"`
	} `json:"cpu"`
	Mem struct {
		UsedMiB  int64 `json:"used_mib"`
		TotalMiB int64 `json:"total_mib"`
	} `json:"mem"`
	Disk struct {
		UsedMiB  int64 `json:"used_mib"`
		TotalMiB int64 `json:"total_mib"`
	} `json:"disk"`
	Net struct {
		RXBytes int64 `json:"rx_bytes"`
		TXBytes int64 `json:"tx_bytes"`
	} `json:"net"`
}

// imageDetail is one entry of GET /api/images?detail=1 (U08).
type imageDetail struct {
	Name       string `json:"name"`
	BuildID    string `json:"build_id"`
	TemplateID string `json:"template_id"`
}

// proxyGet issues the N3 request against the public proxy listener with
// the forward-auth headers (A1 §6.3): the Host header names the lease and
// guest port, X-Proxy-Auth carries the shared secret and Remote-User the
// authenticated user.
func (c *client) proxyGet(leaseID string, port int) (int, string, error) {
	req, err := http.NewRequest("GET", c.proxyURL+"/", nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = fmt.Sprintf("%s-%d%s", leaseID, port, c.proxySuffix)
	req.Header.Set("X-Proxy-Auth", c.proxySecret)
	req.Header.Set("Remote-User", c.proxyUser)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// streamReq is the first client frame of the stream protocol (A1 §5.6).
type streamReq struct {
	Args []string          `json:"args"`
	Cwd  string            `json:"cwd,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Pty  bool              `json:"pty"`
}

// wsConn is an open stream WebSocket.
type wsConn struct {
	conn *websocket.Conn
}

// wsURLOf builds the WebSocket URL for an API path, keeping the API
// base's scheme (wss for https) and host.
func (c *client) wsURLOf(path string) string {
	scheme := "ws"
	host := strings.TrimPrefix(c.base, "http://")
	if strings.HasPrefix(c.base, "https://") {
		scheme = "wss"
		host = strings.TrimPrefix(c.base, "https://")
	}
	return scheme + "://" + host + path
}

// stream opens GET /api/sandboxes/{id}/stream, sends the exec request as
// the first frame and returns the connection.
func (c *client) stream(id string, req streamReq) (*wsConn, error) {
	url := c.wsURLOf("/api/sandboxes/" + id + "/stream")
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
	}
	hdr := http.Header{"Authorization": {"Bearer " + c.token}}
	conn, resp, err := dialer.Dial(url, hdr)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("stream dial: %v %s", err, body)
	}
	w := &wsConn{conn: conn}
	if err := w.send(req); err != nil {
		conn.Close()
		return nil, err
	}
	return w, nil
}

// dialGuest opens GET /api/sandboxes/{id}/ports/{port}/dial — the raw
// host-to-guest TCP bridge (2.2, #113) — and returns the WebSocket. A
// failed upgrade returns the HTTP status (0 when the transport itself
// failed) for status assertions.
func (c *client) dialGuest(id string, port int) (*websocket.Conn, int, error) {
	url := c.wsURLOf("/api/sandboxes/" + id + "/ports/" + strconv.Itoa(port) + "/dial")
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
	}
	conn, resp, err := dialer.Dial(url, http.Header{"Authorization": {"Bearer " + c.token}})
	if err != nil {
		status := 0
		body := ""
		if resp != nil {
			status = resp.StatusCode
			b, _ := io.ReadAll(resp.Body)
			body = strings.TrimSpace(string(b))
		}
		return nil, status, fmt.Errorf("guest dial: %v %s", err, body)
	}
	return conn, resp.StatusCode, nil
}

func (w *wsConn) send(v any) error {
	return w.conn.WriteJSON(v)
}

// read reads one text frame as JSON, with the given timeout.
func (w *wsConn) read(timeout time.Duration) (map[string]any, error) {
	if err := w.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	_, b, err := w.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("bad frame %q: %w", b, err)
	}
	return m, nil
}

func (w *wsConn) close() {
	_ = w.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	_ = w.conn.Close()
}
