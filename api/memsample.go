package api

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Measured memory (#130 R1): the orchestrator's OTEL collector serves
// per-sandbox gauges `e2b_sandbox_ram_used{...,sandbox_id="…"}` and
// `e2b_sandbox_ram_total{...}` (bytes; an OTEL exporter that adds unit
// suffixes names them `…_bytes`, which is accepted too) in its
// Prometheus text output
// (the same document OTEL_PROM_URL already feeds the /metrics
// passthrough). A background sampler reads that document every
// memSampleInterval (30 s), keeps the two families' per-sandbox values
// in memory only, and maps them onto running leases through
// l.SandboxID. Series for sandboxes that are gone are ignored and
// released leases' entries are dropped; nothing here hits the
// substrate or the store.

const (
	// memSampleInterval is how often the sampler fetches the
	// orchestrator metrics. A field on Service (memSampleInterval) so
	// tests shrink it.
	memSampleInterval = 30 * time.Second
	// ramUsedMetric and ramTotalMetric are the two metric families the
	// sampler parses; every other line is skipped.
	ramUsedMetric  = "e2b_sandbox_ram_used"
	ramTotalMetric = "e2b_sandbox_ram_total"
	// sandboxIDLabel is the label series carry that names the sandbox.
	sandboxIDLabel = "sandbox_id"
	// memSampleHTTPTimeout bounds one metrics fetch; a stuck collector
	// must not hold the sampler.
	memSampleHTTPTimeout = 5 * time.Second
	// memSampleMaxBytes caps how much of the metrics document the
	// sampler reads; a bigger document is a hostile collector, not a
	// real one.
	memSampleMaxBytes = 8 << 20
	// sizeHintSamples is the sample count that makes a lease's peak
	// "measured enough to advise on": 10 ticks at the sample interval —
	// five minutes with the default 30 s.
	sizeHintSamples = 10
	// sizeHintMargin multiplies the measured peak to choose a preset: a
	// lease whose VM ever asked for more than the preset's headroom
	// would thrash at it.
	sizeHintMargin = 1.25
)

// memSample is what the sampler keeps for one live lease: its latest
// measured used bytes and the highest seen since the lease's current VM
// started (reset when l.SandboxID changes, e.g. recovery or replace),
// plus the total the sandbox reported when last seen, and how many
// fresh used values this VM has contributed (the size_hint floor).
type memSample struct {
	usedBytes  float64
	peakBytes  float64
	totalBytes float64
	hasTotal   bool
	samples    int
	sandboxID  string
}

// memSampler holds the per-lease measured memory behind the lease
// API's memory_used_mb/memory_peak_mb (#130 R1). Guarded by its own
// mutex; entries are dropped when the lease is released.
type memSampler struct {
	mu      sync.Mutex
	entries map[string]memSample // by lease id
}

func newMemSampler() *memSampler {
	return &memSampler{entries: map[string]memSample{}}
}

// record applies one fetch's per-sandbox values to the sampler's state:
// for every running lease whose sandbox appeared, update latest/peak and
// count a fresh sample; entries of leases without a value this tick keep
// their last numbers (a series lagging one tick is normal). A lease
// whose SandboxID changed starts a new peak. Released leases are
// dropped, as are entries for which nothing is left to show.
func (ms *memSampler) record(leases map[string]*Lease, usedBySandbox, totalBySandbox map[string]float64) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for id, l := range leases {
		if l.released || !l.live() {
			delete(ms.entries, id)
			continue
		}
		if l.SandboxID == "" {
			continue
		}
		e := ms.entries[id]
		if e.sandboxID != l.SandboxID {
			e = memSample{sandboxID: l.SandboxID} // new VM: reset the peak
		}
		used, usedOK := usedBySandbox[l.SandboxID]
		total, totalOK := totalBySandbox[l.SandboxID]
		if usedOK {
			e.usedBytes = used
			if used > e.peakBytes {
				e.peakBytes = used
			}
			e.samples++
		}
		if totalOK {
			e.totalBytes = total
			e.hasTotal = true
		}
		if usedOK || totalOK {
			ms.entries[id] = e
		} else if e.samples == 0 && !e.hasTotal {
			delete(ms.entries, id)
		}
	}
}

// forget drops one lease's entry (release paths call this).
func (ms *memSampler) forget(leaseID string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	delete(ms.entries, leaseID)
}

// peakRelease takes the lease's measured peak at release time and drops
// the entry in the same step (#130 step 1): one sampler lock, no window a
// concurrent tick could forget the entry through, no leftover entry
// should the release fail after reading. A lease with no recorded
// sample answers ok=false (the sampler off — no OTEL_PROM_URL — or a
// lease that never got a sample).
func (ms *memSampler) peakRelease(leaseID string) (float64, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	e, ok := ms.entries[leaseID]
	delete(ms.entries, leaseID)
	return e.peakBytes, ok && e.samples > 0
}

// latest reports the lease's latest measured used bytes.
func (ms *memSampler) latest(leaseID string) (float64, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	e, ok := ms.entries[leaseID]
	return e.usedBytes, ok && e.samples > 0
}

// peak reports the lease's high-water mark since its current VM started.
func (ms *memSampler) peak(leaseID string) (float64, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	e, ok := ms.entries[leaseID]
	return e.peakBytes, ok && e.samples > 0
}

// entry returns a copy of the lease's entry; zero and ok=false when
// the lease has none (the caller checks samples for freshness).
func (ms *memSampler) entry(leaseID string) (memSample, bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	e, ok := ms.entries[leaseID]
	return e, ok
}

// parseOpenMetrics pulls the two ram families out of a Prometheus
// text document: sample lines only, labels parsed with the usual
// comma-separated quoted-value form and escapes, values as floats
// (including exponent form), NaN and ±Inf skipped, other families
// ignored. A line of a shape it does not understand is skipped, not
// an error: an unknown enrichment of the document must not silence
// every lease.
func parseOpenMetrics(body []byte) (used, total map[string]float64) {
	used, total = map[string]float64{}, map[string]float64{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Only `name{labels} value [timestamp]` lines are interesting
		// (both families always carry labels and a timestamp in the
		// collector's output; a bare sample without an id says nothing
		// about a sandbox).
		i := strings.LastIndex(line, "} ")
		if i < 0 {
			continue
		}
		fields := strings.Fields(line[i+2:])
		if len(fields) == 0 || len(fields) > 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		name := strings.TrimSpace(line[:i+1])
		lb := strings.Index(name, "{")
		if lb < 0 || !strings.HasSuffix(name, "}") {
			continue
		}
		sid := parseLabels(name[lb+1 : len(name)-1])[sandboxIDLabel]
		if sid == "" {
			continue
		}
		fam := name[:lb]
		switch fam {
		case ramUsedMetric, ramUsedMetric + "_bytes":
			used[sid] = value
		case ramTotalMetric, ramTotalMetric + "_bytes":
			total[sid] = value
		}
	}
	return used, total
}

// parseLabels splits a label block `a="1",b="2"` into a map. Values use
// Prometheus escaping: \\ → \, \" → ", \n → newline.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	eq := strings.Index(s, "=")
	for eq >= 0 {
		key := strings.TrimSpace(s[:eq])
		rest := strings.TrimSpace(s[eq+1:])
		if len(rest) == 0 || rest[0] != '"' {
			// Not a quoted label: skip to the next comma.
			if c := strings.Index(rest, ","); c >= 0 {
				s = rest[c+1:]
				eq = strings.Index(s, "=")
				continue
			}
			return out
		}
		rest = rest[1:]
		var val []byte
		end := -1
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if c == '\\' && i+1 < len(rest) {
				i++
				switch rest[i] {
				case 'n':
					val = append(val, '\n')
				case 't':
					val = append(val, '\t')
				default:
					val = append(val, rest[i])
				}
				continue
			}
			if c == '"' {
				end = i
				break
			}
			val = append(val, c)
		}
		if end < 0 {
			return out
		}
		out[key] = string(val)
		if c := strings.Index(rest[end+1:], ","); c >= 0 {
			s = rest[end+1+c+1:]
			eq = strings.Index(s, "=")
			continue
		}
		break
	}
	return out
}

// memSamplerFetch is the sampler's source: an HTTP GET of the
// OTEL_PROM_URL document in production. A field on Service so tests
// install a stub without an HTTP server.
type memSamplerFetch func(ctx context.Context) ([]byte, error)

// memSampleHTTPGetter returns a fetch of the orchestrator's Prometheus
// document at url with the given client. A non-200 answer is an error
// carrying the status and up to 200 bytes of the body.
func memSampleHTTPGetter(url string, client *http.Client) memSamplerFetch {
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return io.ReadAll(io.LimitReader(resp.Body, memSampleMaxBytes))
	}
}

// startMemSampler runs the memory sampler loop until ctx is done. It
// samples once at startup and then on the interval, the way the
// usage-history loop runs. A fetch or parse failure logs once and keeps
// the last values.
func (s *Service) startMemSampler(ctx context.Context) {
	s.sampleMemOnce(ctx)
	t := time.NewTicker(s.memSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampleMemOnce(ctx)
		}
	}
}

// sampleMemOnce fetches and records one pass; consecutive failures
// log once and keep the last values (the failure flag clears on the
// next success).
func (s *Service) sampleMemOnce(ctx context.Context) {
	body, err := s.memSamplerFetch(ctx)
	if err != nil {
		if !s.memSampleFailed {
			s.log.Printf("mem sample: fetch orchestrator metrics: %v", err)
			s.memSampleFailed = true
		}
		return
	}
	s.memSampleFailed = false
	used, total := parseOpenMetrics(body)
	// One snapshot of the live leases under the store lock, released
	// before the sampler's own lock: no lock order with the store, and
	// the sampler never waits on anything while holding its lock.
	s.store.mu.Lock()
	live := make(map[string]*Lease, len(s.store.leases))
	for id, l := range s.store.leases {
		if !l.released && l.live() {
			live[id] = l
		}
	}
	s.store.mu.Unlock()
	// Only leases the document says anything about pass through: series
	// of sandboxes that are gone, and other hosts' sandboxes, are
	// invisible.
	for id, l := range live {
		if _, ok := used[l.SandboxID]; !ok {
			if _, ok := total[l.SandboxID]; !ok {
				delete(live, id)
			}
		}
	}
	s.memSamples.record(live, used, total)
}
