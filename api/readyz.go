// Readiness for external uptime monitors (issue #81): GET /readyz on
// the lease API listener answers 200 {"status":"ok"} only when every
// check passes, and 503 {"status":"fail","checks":[…]} with a per-check
// breakdown otherwise — the difference between "the process is up"
// (/healthz, liveness) and "this backend can serve leases" (readiness).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Readiness knobs: each check is bounded to 2 s (a hung substrate or
// database must not wedge the monitor's scrape) and the whole response
// is cached for 5 s, so a poller hitting /readyz every few seconds
// costs nothing.
const (
	readyzCheckTimeout = 2 * time.Second
	readyzCacheFor     = 5 * time.Second
	// The dashboard's danger thresholds (cmd/spoond-dash/render.go):
	// the snapshot disk is in danger at ≥ 90 % used and the hugepage
	// pool at ≥ 92 % used. Readiness fails past them, like the banner.
	readyzDiskUsedDangerPct = 90.0
	readyzHugeUsedDangerPct = 92.0
)

// readyCheck is one named readiness check's result.
type readyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// readyzResult is the /readyz response body.
type readyzResult struct {
	Status string       `json:"status"`
	Checks []readyCheck `json:"checks,omitempty"`
}

// readyzState caches the last readiness result so polling is cheap:
// polls inside the cache window are served from the cache at once, and
// concurrent polls past it share one evaluation (single-flight) instead
// of each re-running the checks. The mutex is never held across an
// evaluation, so a slow check cannot queue polls up behind the lock.
type readyzState struct {
	mu    sync.Mutex
	at    time.Time
	res   readyzResult
	check func() readyzResult
	eval  *readyzEval // the evaluation in flight, if any
}

// readyzEval is one in-flight run of the checks; polls that arrive
// while it runs wait for its result instead of doubling the work.
type readyzEval struct {
	done chan struct{}
	res  readyzResult
}

// readyz returns the cached result while it is younger than
// readyzCacheFor, else re-runs every check and caches that.
func (st *readyzState) readyz() readyzResult {
	st.mu.Lock()
	now := time.Now()
	if st.at.Add(readyzCacheFor).After(now) {
		res := st.res
		st.mu.Unlock()
		return res
	}
	if st.eval == nil {
		// First poll past the window: it runs the checks — with the lock
		// released, so nothing waits on the mutex for them — and every
		// poll that arrives meanwhile shares its result.
		ev := &readyzEval{done: make(chan struct{}), res: readyzResult{Status: "fail"}}
		st.eval = ev
		st.mu.Unlock()
		func() {
			// Even on a panic, waiters are released and the next poll
			// runs the checks again instead of finding a stale eval.
			defer func() {
				st.mu.Lock()
				if st.eval == ev {
					st.eval = nil
				}
				st.mu.Unlock()
				close(ev.done)
			}()
			ev.res = st.check()
			st.mu.Lock()
			st.res, st.at = ev.res, time.Now()
			st.mu.Unlock()
		}()
		return ev.res
	}
	ev := st.eval
	st.mu.Unlock()
	<-ev.done
	return ev.res
}

// handleReadyz serves GET /readyz: 200 {"status":"ok"} when every check
// passes, 503 {"status":"fail","checks":[…]} otherwise. No auth, like
// /healthz.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	res := s.readyz.readyz()
	code := http.StatusServiceUnavailable
	if res.Status == "ok" {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}

// runReadyz evaluates every check and returns the combined verdict:
// "ok" only when all passed. The checks run concurrently, each under its
// own 2 s bound, so a hung orchestrator cannot make a healthy database
// or disk look timed out. The two NodeInfo checks share one fetch.
func (s *Service) runReadyz() readyzResult {
	bounded := func(f func(context.Context) readyCheck) func() readyCheck {
		return func() readyCheck {
			ctx, cancel := context.WithTimeout(context.Background(), readyzCheckTimeout)
			defer cancel()
			return f(ctx)
		}
	}
	var node, hp, db, disk readyCheck
	var wg sync.WaitGroup
	for _, run := range []func(){
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), readyzCheckTimeout)
			defer cancel()
			info, err := s.sub.NodeInfo(ctx)
			node, hp = nodeCheck(info, err), hugepagesCheck(info, err)
		},
		func() { db = bounded(s.databaseReady)() },
		func() { disk = bounded(s.diskReady)() },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	wg.Wait()
	checks := []readyCheck{node, db, disk, hp, s.drainingCheck()}
	status := "ok"
	for _, c := range checks {
		if !c.OK {
			status = "fail"
		}
	}
	return readyzResult{Status: status, Checks: checks}
}

// drainingCheck reports the admin drain state. It never fails readiness:
// a draining node is a node doing what it was told, and the create
// route's 503 draining already tells clients what to do. It is here so a
// monitor sees the state at all — and so does a person reading /readyz —
// instead of a drain looking indistinguishable from a healthy backend.
func (s *Service) drainingCheck() readyCheck {
	c := readyCheck{Name: "draining"}
	if s.draining.Load() {
		return passCheck(c, "admin drain in effect")
	}
	return passCheck(c, "off")
}

// nodeCheck reports the orchestrator's health from one NodeInfo fetch:
// it must answer and name the node "healthy" — the same condition
// admission enforces, so readiness can never pass while creates are
// refused for node status.
func nodeCheck(info substrate.NodeInfo, err error) readyCheck {
	c := readyCheck{Name: "orchestrator"}
	switch {
	case err != nil:
		return failCheck(c, "unreachable: "+err.Error())
	case info.Status != "healthy":
		return failCheck(c, "node status "+info.Status)
	}
	return passCheck(c, info.Status)
}

// databaseReady runs a trivial query against the catalog's reader pool
// under its 2 s bound: a database file that opens but does not
// answer fails readiness.
func (s *Service) databaseReady(ctx context.Context) readyCheck {
	c := readyCheck{Name: "database"}
	if err := s.db.Ping(ctx); err != nil {
		return failCheck(c, err.Error())
	}
	return passCheck(c, "ok")
}

// diskReady reports the snapshot store's used percentage against the
// dashboard's danger level (90 %). A store path that cannot be read
// fails readiness: the disk rules treat that as "stay off", but a
// monitor must hear about it. The statfs runs on its own goroutine so a
// wedged filesystem surfaces as a timeout, not a hung scrape.
func (s *Service) diskReady(ctx context.Context) readyCheck {
	c := readyCheck{Name: "disk"}
	if s.cfg.TemplateStoragePath == "" {
		return failCheck(c, "E2B_TEMPLATE_STORAGE_PATH is not set")
	}
	done := make(chan struct{})
	var freePct float64
	var ok bool
	go func() {
		defer close(done)
		freePct, ok = s.freePercent(s.cfg.TemplateStoragePath)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return failCheck(c, "timeout reading "+s.cfg.TemplateStoragePath)
	}
	if !ok {
		return failCheck(c, "cannot stat "+s.cfg.TemplateStoragePath)
	}
	usedPct := 100 - freePct
	if usedPct >= readyzDiskUsedDangerPct {
		return failCheck(c, fmt.Sprintf("%.0f%% used, danger level %.0f%%", usedPct, readyzDiskUsedDangerPct))
	}
	return passCheck(c, fmt.Sprintf("%.0f%% used", usedPct))
}

// hugepagesCheck reports the hugepage pool against the dashboard's
// danger level (92 % used). Past it, admission refuses every
// default-size lease. Shares the NodeInfo fetch with nodeCheck, so an
// unreachable orchestrator fails both.
//
// The pool counts as used when it is not free: the orchestrator's
// metric_hugepages_used means total − free, and the kernel keeps
// reserved pages inside free, so used+reserved over total matches the
// dashboard's total − (free − min(rsvd, free)) line for it
// (cmd/spoond-dash/collect.go memGauges).
func hugepagesCheck(info substrate.NodeInfo, err error) readyCheck {
	c := readyCheck{Name: "hugepages"}
	switch {
	case err != nil:
		return failCheck(c, "unreachable: "+err.Error())
	case info.HugepagesTotal == 0:
		// The same guard pressureShortensIdle applies: without the
		// counts the pool state is unknown, and a monitor must say so
		// rather than divide by zero.
		return failCheck(c, "node reports no hugepage pool")
	}
	busy := min(info.HugepagesUsed+info.HugepagesReserved, info.HugepagesTotal)
	free := (info.HugepagesTotal - busy) * info.HugepageSizeBytes
	usedPct := float64(busy) / float64(info.HugepagesTotal) * 100
	if usedPct >= readyzHugeUsedDangerPct {
		return failCheck(c, fmt.Sprintf("%.0f%% of the pool used, danger level %.0f%%", usedPct, readyzHugeUsedDangerPct))
	}
	return passCheck(c, fmt.Sprintf("%.1f GiB free", float64(free)/(1<<30)))
}

// passCheck / failCheck finish a check's result.
func passCheck(c readyCheck, detail string) readyCheck {
	c.OK, c.Detail = true, detail
	return c
}

func failCheck(c readyCheck, detail string) readyCheck {
	c.OK, c.Detail = false, detail
	return c
}
