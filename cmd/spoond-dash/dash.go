// Package spoonddash is `spoond dash`: a read-only, live view of spoond's
// present operation, for watching rather than triage.
//
// One collector loop builds a Snapshot every DASH_INTERVAL from spoond's
// /metrics (scrape-only METRICS_TOKEN), the SQLite catalog (opened
// read-only), the identity store (names only), /proc and systemd. Every
// viewer shares that loop: each page holds one SSE stream that receives
// the snapshot as Datastar signal patches (numbers, bound to Starbase
// gauges, meters and sparklines) and element patches (the tables). The
// server keeps DASH_HISTORY points of history so a new page starts with
// trends, not empty sparklines.
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
//	DASH_INTERVAL        refresh interval (default 2s)
//	DASH_HISTORY         sparkline points kept (default 150, i.e. 5 min at 2s; max 200)
package spoonddash

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed static page.html.tmpl
var assets embed.FS

// Config is the dashboard's configuration (see the package comment).
type Config struct {
	Addr, User, PasswordHash       string
	TLSCert, TLSKey                string
	MetricsURL, MetricsServerName  string
	MetricsToken                   string
	DBPath, UsersFile, StoragePath string
	Services                       []string
	Interval                       time.Duration
	History                        int
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
	}
	var err error
	if c.Interval, err = time.ParseDuration(env("DASH_INTERVAL", "2s")); err != nil || c.Interval < time.Second {
		return c, fmt.Errorf("DASH_INTERVAL: want a duration of at least 1s")
	}
	if c.History, err = strconv.Atoi(env("DASH_HISTORY", "150")); err != nil || c.History < 10 || c.History > 200 {
		return c, fmt.Errorf("DASH_HISTORY: want an integer from 10 to 200 (the sparkline keeps at most 200 points)")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, fmt.Errorf("set both DASH_TLS_CERT and DASH_TLS_KEY, or neither")
	}
	for k, v := range map[string]string{"DASH_USER": c.User, "DASH_PASSWORD_HASH": c.PasswordHash, "METRICS_TOKEN": c.MetricsToken} {
		if v == "" {
			return c, fmt.Errorf("%s is required", k)
		}
	}
	return c, nil
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
	cfg  Config
	col  *collector
	page *template.Template

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
	return &dash{cfg: cfg, col: newCollector(cfg), page: page, hist: map[string][]float64{}, viewers: map[chan struct{}]bool{}}, nil
}

func (d *dash) run(ctx context.Context) {
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		s := d.col.collect(ctx)
		d.mu.Lock()
		d.last = s
		for _, k := range series {
			h := append(d.hist[k], seriesValue(s, k))
			if len(h) > d.cfg.History {
				h = h[len(h)-d.cfg.History:]
			}
			d.hist[k] = h
		}
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
	return d.basicAuth(mux)
}

func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// basicAuth guards everything except /healthz.
func (d *dash) basicAuth(next http.Handler) http.Handler {
	user := []byte(d.cfg.User)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
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

// pageData is what the page renders: the latest snapshot plus the
// history, so sparklines draw on load instead of after the first frame.
type pageData struct {
	Snapshot
	Hist map[string][]float64
}

func (d *dash) handlePage(w http.ResponseWriter, _ *http.Request) {
	hist := d.history()
	d.mu.Lock()
	s := d.last
	d.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.page.Execute(w, pageData{Snapshot: s, Hist: hist}); err != nil {
		log.Printf("spoond dash: render: %v", err)
	}
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
func (d *dash) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

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
		if err := d.writeFrame(w, s, hist); err != nil {
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

// writeFrame sends the snapshot as a signal patch ($_s, plus $_h history
// on the first frame) and the tables as element patches.
func (d *dash) writeFrame(w http.ResponseWriter, s Snapshot, hist map[string][]float64) error {
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
	for _, name := range []string{"leases", "images", "services"} {
		var buf strings.Builder
		if err := d.page.ExecuteTemplate(&buf, name, s); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w, "event: datastar-patch-elements\n"); err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if _, err := fmt.Fprintf(w, "data: elements %s\n", line); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}
