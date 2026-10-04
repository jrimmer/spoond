// Package spoonddash is `spoond dash`: a read-only, live view of spoond's
// present operation, for watching rather than triage.
//
// One collector loop builds a Snapshot every DASH_INTERVAL from spoond's
// /metrics (scrape-only METRICS_TOKEN), the SQLite catalog (opened
// read-only), the identity store (names only), /proc and systemd. Every
// viewer shares that loop: each page holds one SSE stream that receives
// the snapshot as Datastar signal patches (the numbers) and the frame as
// element patches (one per changed row of the character grid, the whole
// <pre> when the row count changed). `spoond top` renders the same
// snapshot as the same grid with ANSI styles in the terminal. The server
// keeps DASH_HISTORY points of history so a new page starts with trends,
// not empty sparklines.
//
// Environment:
//
//	DASH_ADDR            listen address (default 0.0.0.0:8893)
//	DASH_TLS_CERT, DASH_TLS_KEY  serve HTTPS with this pair (basic auth
//	                     sends the password, so use TLS beyond localhost)
//	DASH_USER            basic-auth user (required)
//	DASH_PASSWORD_HASH   bcrypt hash of the password (required)
//	METRICS_URL          spoond /metrics (default https://127.0.0.1:8890/metrics)
//	METRICS_SERVER_NAME  TLS server name for METRICS_URL (default vm2.lacy.casa)
//	METRICS_TOKEN        spoond's scrape-only token (required)
//	SPOOND_DB_PATH       catalog database (default /var/lib/spoond/spoond.db)
//	USERS_FILE           identity store (default /var/lib/spoond/users.json)
//	E2B_TEMPLATE_STORAGE_PATH  disk to report (default /forkdcache/e2b/storage/templates)
//	DASH_SERVICES        systemd units to show (comma-separated)
//	DASH_ACTIVITY_UNIT   unit whose journal feeds the events panel
//	                     (default spoond-backend)
//	DASH_INTERVAL        refresh interval (default 2s)
//	DASH_HISTORY         sparkline points kept (default 150, i.e. 5 min at 2s; max 200)
//	DASH_WIDTH           frame width in cells (default 104, 72–104; the
//	                     page and spoond top both draw at this width —
//	                     top clamps the terminal's COLUMNS into it)
//	DASH_HOST            header label (default the hostname)
package spoonddash

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jrimmer/spoond/v2/grid"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed static page.html.tmpl
var assets embed.FS

// dashVersion is the spoond version the header shows (spoond top shows
// its own binary's; the page shows this build's).
var dashVersion = readDashVersion()

func readDashVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "dev"
}

// Config is the dashboard's configuration (see the package comment).
type Config struct {
	Addr, User, PasswordHash       string
	TLSCert, TLSKey                string
	MetricsURL, MetricsServerName  string
	MetricsToken                   string
	DBPath, UsersFile, StoragePath string
	Services                       []string
	// ActivityUnit is the systemd unit whose journal feeds the events
	// panel (DASH_ACTIVITY_UNIT).
	ActivityUnit string
	Interval     time.Duration
	History      int
	Width        int    // grid width in cells (DASH_WIDTH, 72–104)
	Host         string // header label (DASH_HOST, else the hostname)
}

func configFromEnv() (Config, error) {
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Addr:              env("DASH_ADDR", "0.0.0.0:8893"),
		TLSCert:           os.Getenv("DASH_TLS_CERT"),
		TLSKey:            os.Getenv("DASH_TLS_KEY"),
		User:              os.Getenv("DASH_USER"),
		PasswordHash:      os.Getenv("DASH_PASSWORD_HASH"),
		MetricsURL:        env("METRICS_URL", "https://127.0.0.1:8890/metrics"),
		MetricsServerName: env("METRICS_SERVER_NAME", "vm2.lacy.casa"),
		MetricsToken:      os.Getenv("METRICS_TOKEN"),
		DBPath:            env("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db"),
		UsersFile:         env("USERS_FILE", "/var/lib/spoond/users.json"),
		StoragePath:       env("E2B_TEMPLATE_STORAGE_PATH", "/forkdcache/e2b/storage/templates"),
		Services: strings.Split(env("DASH_SERVICES",
			"spoond-backend,spoond-runner,spoond-sshd-gateway,e2b-orchestrator,e2b-guard,otelcol"), ","),
		ActivityUnit: env("DASH_ACTIVITY_UNIT", "spoond-backend"),
	}
	var err error
	if c.Interval, err = time.ParseDuration(env("DASH_INTERVAL", "2s")); err != nil || c.Interval < time.Second {
		return c, fmt.Errorf("DASH_INTERVAL: want a duration of at least 1s")
	}
	if c.History, err = strconv.Atoi(env("DASH_HISTORY", "150")); err != nil || c.History < 10 || c.History > 200 {
		return c, fmt.Errorf("DASH_HISTORY: want an integer from 10 to 200 (the sparkline keeps at most 200 points)")
	}
	if c.Width, err = strconv.Atoi(env("DASH_WIDTH", strconv.Itoa(DefaultWidth))); err != nil || c.Width < minW || c.Width > maxW {
		return c, fmt.Errorf("DASH_WIDTH: want an integer from %d to %d (the grid's width in cells)", minW, maxW)
	}
	c.Host = env("DASH_HOST", "")
	if c.Host == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			c.Host = hn
		}
	}
	if c.Host == "" {
		c.Host = "host"
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, fmt.Errorf("set both DASH_TLS_CERT and DASH_TLS_KEY, or neither")
	}
	if c.MetricsToken == "" {
		return c, fmt.Errorf("METRICS_TOKEN is required")
	}
	return c, nil
}

// requireLogin checks the settings only the web dashboard needs: spoond
// top draws in the operator's own terminal and has no login.
func requireLogin(c Config) error {
	for k, v := range map[string]string{"DASH_USER": c.User, "DASH_PASSWORD_HASH": c.PasswordHash} {
		if v == "" {
			return fmt.Errorf("%s is required", k)
		}
	}
	return nil
}

// Main runs the dashboard; registered as `spoond dash`.
func Main(args []string) int {
	if len(args) > 0 && (args[0] == "hash" || args[0] == "-h" || args[0] == "--help") {
		if args[0] != "hash" || len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: spoond dash            run the dashboard (configured by environment)\n       spoond dash hash PASS  print a bcrypt hash for DASH_PASSWORD_HASH")
			return 2
		}
		h, err := bcrypt.GenerateFromPassword([]byte(args[1]), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(string(h))
		return 0
	}
	cfg, err := configFromEnv()
	if err == nil {
		err = requireLogin(cfg)
	}
	if err != nil {
		log.Printf("spoond dash: %v", err)
		return 2
	}
	d, err := newDash(cfg)
	if err != nil {
		log.Printf("spoond dash: %v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go d.run(ctx)

	srv := &http.Server{Addr: cfg.Addr, Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("spoond dash listening on %s (tls %v, refresh %s, history %d)", cfg.Addr, cfg.TLSCert != "", cfg.Interval, cfg.History)
	serve := srv.ListenAndServe
	if cfg.TLSCert != "" {
		serve = func() error { return srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey) }
	}
	if err := serve(); err != nil && err != http.ErrServerClosed {
		log.Printf("spoond dash: %v", err)
		return 1
	}
	return 0
}

// dash holds the latest snapshot, the history and the connected viewers.
type dash struct {
	cfg   Config
	col   *collector
	page  *template.Template
	width int // frame width in cells (DASH_WIDTH, default 104)

	mu      sync.Mutex
	last    Snapshot
	hist    map[string][]float64 // sparkline series, oldest first
	viewers map[chan struct{}]bool
}

// Sparkline series kept in history, by signal name.
var series = []string{"running", "reqPerSec", "createsPerMin", "cpuPct", "memUsedPct", "fwConns"}

func newDash(cfg Config) (*dash, error) {
	funcs := template.FuncMap{
		// signals seeds Datastar with the current snapshot, so the page
		// shows real numbers before the stream's first frame.
		"signals": func(v pageData) (string, error) {
			b, err := json.Marshal(map[string]any{"_s": v.Snapshot, "_h": v.Hist})
			return string(b), err
		},
		"hist": func() int { return cfg.History },
	}
	page, err := template.New("page.html.tmpl").Funcs(funcs).ParseFS(assets, "page.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &dash{cfg: cfg, col: newCollector(cfg), page: page, width: cfg.Width, hist: map[string][]float64{}, viewers: map[chan struct{}]bool{}}, nil
}

func (d *dash) run(ctx context.Context) {
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		s := d.col.collect(ctx)
		d.mu.Lock()
		d.last = s
		appendHist(d.hist, s, d.cfg.History)
		for ch := range d.viewers {
			select {
			case ch <- struct{}{}:
			default: // viewer still busy with the previous frame
			}
		}
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// appendHist appends one snapshot's sparkline values to hist, keeping
// the last n points per series; shared by the dash run loop and spoond
// top's loop.
func appendHist(hist map[string][]float64, s Snapshot, n int) {
	for _, k := range series {
		h := append(hist[k], seriesValue(s, k))
		if len(h) > n {
			h = h[len(h)-n:]
		}
		hist[k] = h
	}
}

func seriesValue(s Snapshot, k string) float64 {
	switch k {
	case "running":
		return float64(s.Running)
	case "reqPerSec":
		return s.ReqPerSec
	case "createsPerMin":
		return s.CreatesPerMin
	case "cpuPct":
		return s.CPUPct
	case "memUsedPct":
		return s.MemUsedPct
	case "fwConns":
		return float64(s.FwConns)
	}
	return 0
}

func (d *dash) handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static))))
	mux.HandleFunc("GET /{$}", d.handlePage)
	mux.HandleFunc("GET /stream", d.handleStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	// Readiness (issue #81): 200 only when every source the dashboard
	// reads answers — metrics, catalog and identity store. Auth-exempt
	// like /healthz: an uptime monitor holds no credentials.
	mux.HandleFunc("GET /readyz", d.handleReadyz)
	return d.basicAuth(mux)
}

// handleReadyz reports whether the dashboard can reach its sources
// (issue #81): the /metrics scrape, the SQLite catalog and the identity
// store file — the same reads the collector loop makes each tick. 200
// {"status":"ok"} when all pass, 503 {"status":"fail","checks":[…]}
// naming the failing ones. A monitor pointing here learns about a stale
// dashboard before a person staring at a frozen frame does.
func (d *dash) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	checks := []dashCheck{
		d.metricsReady(),
		d.dbReady(),
		d.usersReady(),
	}
	r := readyzResult{Status: "ok"}
	for _, c := range checks {
		if !c.OK {
			r.Status = "fail"
		}
	}
	r.Checks = checks
	code := http.StatusServiceUnavailable
	if r.Status == "ok" {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(r)
}

// dashCheck is one dashboard readiness check's result.
type dashCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// readyzResult is the /readyz response body.
type readyzResult struct {
	Status string      `json:"status"`
	Checks []dashCheck `json:"checks,omitempty"`
}

// probeSource runs one source check under a 2 s bound: readiness is
// answered from the live sources, not from the last snapshot, so a
// source that dies between ticks is reported at once.
func probeSource(name string, fn func(context.Context) error) dashCheck {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		return dashCheck{Name: name, OK: false, Detail: err.Error()}
	}
	return dashCheck{Name: name, OK: true, Detail: "ok"}
}

// metricsReady probes the /metrics scrape endpoint with the configured
// token. An empty token sends no Authorization header at all: a
// hand-built Config (an embedding, a test) may leave METRICS_TOKEN
// unset, and an Authorization header reading "Bearer ", a token of
// literally nothing, is never what a probe means.
func (d *dash) metricsReady() dashCheck {
	return probeSource("metrics", func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.MetricsURL, nil)
		if err != nil {
			return err
		}
		if d.cfg.MetricsToken != "" {
			req.Header.Set("Authorization", "Bearer "+d.cfg.MetricsToken)
		}
		resp, err := d.col.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s", resp.Status)
		}
		return nil
	})
}

// dbReady opens the catalog read-only and runs a trivial query.
func (d *dash) dbReady() dashCheck {
	return probeSource("database", func(ctx context.Context) error {
		db, err := sql.Open("sqlite", "file:"+d.cfg.DBPath+"?mode=ro&_pragma=busy_timeout(3000)")
		if err != nil {
			return err
		}
		defer db.Close()
		var one int
		return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	})
}

// usersReady checks the identity store file is readable — the source of
// every name the dashboard shows.
func (d *dash) usersReady() dashCheck {
	return probeSource("identity store", func(context.Context) error {
		f, err := os.Open(d.cfg.UsersFile)
		if err != nil {
			return err
		}
		return f.Close()
	})
}

func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// basicAuth guards everything except /healthz and /readyz (the probe
// endpoints uptime monitors hit, which hold no credentials).
func (d *dash) basicAuth(next http.Handler) http.Handler {
	user := []byte(d.cfg.User)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), user) != 1 ||
			bcrypt.CompareHashAndPassword([]byte(d.cfg.PasswordHash), []byte(p)) != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="spoond", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pageData is what the page renders: the latest snapshot, the history
// and the grid's row elements (the frame carries the host, the version
// and the clock in its own header and status line).
type pageData struct {
	Snapshot
	Hist  map[string][]float64
	Width int
	// Grid is the grid's rows as HTML; already escaped by grid.HTML, so
	// the template must not escape it again.
	Grid template.HTML
}

func (d *dash) handlePage(w http.ResponseWriter, r *http.Request) {
	hist := d.history()
	d.mu.Lock()
	s := d.last
	d.mu.Unlock()
	g, links := d.gridFor(s, hist, time.Now())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{Snapshot: s, Hist: hist, Width: g.Cols(), Grid: template.HTML(pageGrid(g, links))}
	if err := d.page.Execute(w, data); err != nil {
		log.Printf("spoond dash: render: %v", err)
	}
}

// gridFor renders a snapshot plus history into a grid and the holder
// links found in it (row → URL), for both the page and the stream.
func (d *dash) gridFor(s Snapshot, hist map[string][]float64, now time.Time) (*grid.Grid, []linkAt) {
	g, err := drawFrame(s, hist, d.width, d.cfg.Host, now)
	if err != nil {
		// Check is a rendered invariant, not a data condition: nothing a
		// snapshot contains should trip it. Report and fall back to an
		// empty frame rather than streaming a broken one.
		log.Printf("spoond dash: %v", err)
		g = grid.New(clamp(d.width, minW, maxW), 1)
	}
	return g, holderLinks(s, d.width, now)
}

// history returns a copy of the sparkline series.
func (d *dash) history() map[string][]float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string][]float64, len(d.hist))
	for k, v := range d.hist {
		out[k] = append([]float64(nil), v...)
	}
	return out
}

// handleStream is one viewer's SSE stream: the full state (with history)
// at once, then a frame per collector tick until the page goes away.
// Each stream owns a streamState, so the rows it diffs against are the
// rows it actually sent — two open pages never cross-contaminate.
func (d *dash) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	st := &streamState{rows: map[int]string{}}
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.viewers[ch] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.viewers, ch)
		d.mu.Unlock()
	}()

	first := true
	for {
		var hist map[string][]float64
		if first {
			hist = d.history()
		}
		d.mu.Lock()
		s := d.last
		d.mu.Unlock()
		if err := d.writeFrame(w, st, s, hist); err != nil {
			return
		}
		fl.Flush()
		first = false
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		}
	}
}

// streamState is one stream's diff base: the row HTML this viewer was
// last sent, by row number. A frame patches only the rows that differ
// from it, so a viewer is never sent row patches it cannot apply.
type streamState struct {
	rows map[int]string
}

// writeFrame sends the snapshot as a signal patch ($_s, plus $_h history
// on the first frame) and the grid as element patches into the page's
// <pre id=grid>: one patch per changed row (#rN, inner mode — the span
// itself stays), or all of the <pre>'s rows (inner mode on #grid, so
// the <pre> and its class stay) when the row count changed. Replacing
// the <pre> itself with bare rows lost its class, and the rows then
// ran together and wrapped. A page that loads drawn keeps its rows; the rest are dropped
// and the next frame's <pre> patch recreates them.
func (d *dash) writeFrame(w http.ResponseWriter, st *streamState, s Snapshot, hist map[string][]float64) error {
	sig := map[string]any{"_s": s}
	if hist != nil {
		sig["_h"] = hist
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: datastar-patch-signals\ndata: signals %s\n\n", b); err != nil {
		return err
	}
	g, links := d.gridFor(s, hist, time.Now())
	lines := pageLines(g, links)
	d.mu.Lock()
	sent := st.rows
	d.mu.Unlock()
	if len(lines) != len(sent) {
		// Row count changed: replace every row inside the <pre>.
		if err := writeElements(w, "#grid", "inner", pageGrid(g, links)); err != nil {
			return err
		}
	} else {
		for _, l := range lines {
			if l.HTML == sent[l.N] {
				continue
			}
			if err := writeElements(w, fmt.Sprintf("#r%d", l.N), "inner", l.HTML); err != nil {
				return err
			}
		}
	}
	next := make(map[int]string, len(lines))
	for _, l := range lines {
		next[l.N] = l.HTML
	}
	d.mu.Lock()
	st.rows = next
	d.mu.Unlock()
	return nil
}

// writeElements sends one datastar-patch-elements event patching the
// element(s) at selector with html in the named mode ("outer" replaces
// the element itself, "inner" its children).
func writeElements(w io.Writer, selector, mode, html string) error {
	if _, err := fmt.Fprintf(w, "event: datastar-patch-elements\ndata: selector %s\ndata: mode %s\n", selector, mode); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimRight(html, "\n"), "\n") {
		if _, err := fmt.Fprintf(w, "data: elements %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}
