package spoonddash

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Snapshot is one frame of the dashboard: everything a viewer sees,
// computed once per interval and shared by every connected page.
type Snapshot struct {
	At      string `json:"at"`
	Version string `json:"version"` // orchestrator version label
	Err     string `json:"err"`     // last scrape problem, if any

	// Leases and sandboxes.
	Leases  int            `json:"leases"`
	ByState map[string]int `json:"byState"`
	// Burst is the live leases (running, suspended, recovered) in the
	// burst class, counted in the store: the hugepages burst holds now.
	// It is deliberately not the same set the leases panel's ·b marker
	// shows, which is the stored class of the displayed rows, lost
	// rows included (#128 part 2).
	Burst int `json:"burst"`
	// Preempted is the live leases suspended by preemption (#128 part
	// 3), counted in the store: burst leases the resume queue will bring
	// back when capacity allows, shown by the leases table's preempted
	// mark.
	Preempted int `json:"preempted"`
	Queued    int `json:"queued"`
	// QueuedOldest is the age in seconds of the oldest create waiting for
	// admission (#129 part 1); 0 when nothing waits.
	QueuedOldest float64 `json:"queuedOldest"`
	Granted      int     `json:"granted"` // cumulative leases granted
	Swept        int     `json:"swept"`
	Running      int     `json:"running"` // sandboxes on the node
	Limit        int     `json:"limit"`   // node sandbox limit
	Shares       int     `json:"shares"`
	Users        int     `json:"users"`
	BuildsBusy   int     `json:"buildsBusy"`

	// Rates and latencies over the last interval.
	ReqPerSec     float64 `json:"reqPerSec"`
	CreatesPerMin float64 `json:"createsPerMin"` // leases created in the last minute
	// Mean substrate create and resume time over the last hour, in ms;
	// -1 when there were none (shown as "–", never as a misleading 0).
	CreateMs   float64 `json:"createMs"`
	ResumeMs   float64 `json:"resumeMs"`
	FwConns    int     `json:"fwConns"` // active egress firewall connections
	AuthFails  int     `json:"authFails"`
	Quota      int     `json:"quota"` // quota rejections, cumulative
	Throttled  int     `json:"throttled"`
	Capacity   int     `json:"capacity"` // admission refusals, cumulative
	BuildFails int     `json:"buildFails"`

	// Host.
	CPUPct      float64 `json:"cpuPct"`
	Load1       float64 `json:"load1"`
	Cores       int     `json:"cores"`
	MemUsedPct  float64 `json:"memUsedPct"`
	MemTotalGiB float64 `json:"memTotalGiB"` // host memory outside the hugepage pool
	MemUsedGiB  float64 `json:"memUsedGiB"`
	HugeUsedPct float64 `json:"hugeUsedPct"`
	HugeFreeGiB float64 `json:"hugeFreeGiB"`
	DiskUsedPct float64 `json:"diskUsedPct"`
	DiskFreeGiB float64 `json:"diskFreeGiB"`
	VCPUAlloc   int     `json:"vcpuAlloc"`
	MemAllocGiB float64 `json:"memAllocGiB"`

	// I/O pressure (PSI, /proc/pressure/io) and the snapshot disk's
	// write throughput and busy share (/proc/diskstats, a delta between
	// collections). IOAvail is false on a kernel without PSI: the
	// pressure item is then hidden rather than drawn as a calm zero.
	IOAvail     bool    `json:"ioAvail"`
	IOSome10    float64 `json:"ioSome10"`   // some, avg10 (%)
	IOSome60    float64 `json:"ioSome60"`   // some, avg60 (%)
	IOFull10    float64 `json:"ioFull10"`   // full, avg10 (%)
	IOFull60    float64 `json:"ioFull60"`   // full, avg60 (%)
	DiskDevice  string  `json:"diskDevice"` // the device the storage lives on ("" when unknown)
	DiskWriteMB float64 `json:"diskWriteMB"`
	DiskBusyPct float64 `json:"diskBusyPct"`
	// BackendUp is how long the spoond backend has run (from
	// spoond_backend_start_time_seconds); 0 when /metrics does not say.
	BackendUp   time.Duration `json:"-"`
	RootUsedPct float64       `json:"rootUsedPct"` // the root filesystem, not the snapshot store
	RootFreeGiB float64       `json:"rootFreeGiB"`

	Down int `json:"down"` // units not active

	// GCDeleted is the builds the GC has deleted, summed over
	// spoond_gc_deleted_total's labels. Shown next to the GC's mode;
	// left out of the frame while it is 0.
	GCDeleted int `json:"gcDeleted"`

	// GCMode labels the snapshot GC: "delete" (GC_DELETE=1, or the
	// backend has actually deleted something) or "dry-run".
	GCMode string `json:"gcMode"`

	// KeptBuilds and KeptBuildsBytes are the kept checkpoints of live
	// leases and their disk bytes (#126, from spoond_kept_builds and
	// spoond_kept_builds_bytes). The host panel's GC row appends them
	// when N > 0; KeptDiskPct is kept bytes over the snapshot disk's
	// size, for the Notifications panel.
	KeptBuilds      int     `json:"keptBuilds"`
	KeptBuildsBytes int64   `json:"keptBuildsBytes"`
	KeptDiskPct     float64 `json:"keptDiskPct"`

	// PinnedIdle is the count of pinned leases whose last API activity
	// passed PINNED_IDLE_NOTICE_DAYS (FS5, from leases.pinned_idle_since
	// <> ''): visibility only, for the Notifications panel's one
	// aggregate message. Nothing is paused, unpinned or released.
	PinnedIdle int `json:"pinnedIdle"`

	// Rendered as HTML element patches, not sent as signals.
	Services []Service   `json:"-"`
	Rows     []LeaseRow  `json:"-"`
	Images   []ImageRow  `json:"-"`
	Events   []EventLine `json:"-"` // the events panel, newest first
}

// EventLine is one line of the events panel: text plus the grid style
// it is drawn with ("warn" for lost and held-lease events, "dim" for
// released ones, "text" for the rest, "bad" for a scrape problem).
type EventLine struct {
	Text  string
	Style string
}

// dashEvent is one lease event as the events panel shows it: when (the
// event's own clock, HH:MM:SS), the type, the lease id and what the
// tail column names (holder, then comment, then owner). Detail carries
// the event's own note; the panel does not draw it, but it is kept so
// the tests can tell one stream's events apart.
type dashEvent struct {
	At                     time.Time
	Type, LeaseID, Subject string
	Detail                 string
}

// eventBuffer is the collector's rolling window of lease events, fed by
// its one SSE subscription, shown by the events panel (newest first).
type eventBuffer struct {
	mu     sync.Mutex
	events []dashEvent // oldest first, at most maxEvents kept
}

const maxEvents = 50

// add appends one event, keeping the buffer at most maxEvents long.
func (b *eventBuffer) add(ev dashEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	if len(b.events) > maxEvents {
		b.events = b.events[len(b.events)-maxEvents:]
	}
}

// newest returns up to n events, newest first.
func (b *eventBuffer) newest(n int) []dashEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]dashEvent, 0, n)
	for i := len(b.events) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, b.events[i])
	}
	return out
}

// eventStyle is the grid style an event is drawn with: warn for lost,
// held-lease actions and idle suspensions, and for a release or gc pass
// whose detail names a failure (the CI runner's "✗", the GC's stale-build
// failure); ok for a gc pass that deleted something (spoond's own
// maintenance); dim for releases, text for the rest.
func eventStyle(ev dashEvent) string {
	switch ev.Type {
	case "lost", "held_action", "job_lost", "idle_suspended", "user_deleted":
		return "warn"
	case "gc":
		// A pass that failed a stale building row is maintenance that
		// went wrong, not a successful reclaim (spoond-rzz).
		if strings.Contains(ev.Detail, "failed") {
			return "warn"
		}
		return "ok"
	case "released":
		if strings.Contains(ev.Detail, "✗") {
			return "warn"
		}
		return "dim"
	default:
		return "text"
	}
}

// jobExitedStyle picks the job_exited line's style: a non-zero exit is
// drawn in the warning colour (2.6, #135), a zero exit like any other
// event.
func jobExitedStyle(detail string) string {
	if strings.HasPrefix(detail, "exit 0") {
		return "text"
	}
	return "warn"
}

// eventSubject picks the tail column: the lease's holder, else its
// comment (a CI job lease has neither holder nor name but carries the
// job it runs), else the owner the event carries. A lease-less event
// (the catalog gc) has no subject at all, so it names spoond. The rows
// are the live lease table the same tick built; a lease that has left
// it (released) falls through to the event's owner.
func eventSubject(ev dashEvent, rows []LeaseRow, names map[string]string) string {
	if ev.LeaseID == "" {
		return "spoond"
	}
	for _, r := range rows {
		if r.ID == ev.LeaseID {
			if r.Holder != "" {
				return r.Holder
			}
			if r.Comment != "" {
				return r.Comment
			}
			break
		}
	}
	// The fallback is the event's owner, an identity id (u-…) for a
	// person or agent: show its name, as the leases table does (#127).
	if n := names[ev.Subject]; n != "" {
		return n
	}
	return ev.Subject
}

// Service is a systemd unit and its state.
type Service struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// LeaseRow is one live lease for the table. Holder/HolderURL are the
// lease's plain holder label and link (no lifecycle effect since FS5);
// Pinned marks a pinned lease (the state cell and the holder column
// show it). LastAction/LastActionAt record the last automatic action
// ("rule/action", e.g. "idle_suspend/suspend_idle"). Comment is the
// lease's own note: the holder column shows it, dim, on a CI job lease
// with no holder and no name (e.g. "forgejo: example.com/site #218").
// Burst is the lease's admission class (#128 part 2): the state cell
// shows it as "·b". Preempted marks a burst lease suspended by
// preemption (#128 part 3): the state cell shows it as "·p".
// IdleSuspended marks a persistent lease suspended by its own
// idle_suspend threshold (2.5, #129 part 2): the state cell shows it as
// "·i".
type LeaseRow struct {
	ID, Image, Owner, State, Policy, Name, Comment string
	Burst                                          bool
	Preempted                                      bool
	IdleSuspended                                  bool
	Pinned                                         bool
	Holder, HolderURL                              string
	LastAction                                     string
	LastActionAt                                   time.Time
	Age, Left                                      string
}

// ImageRow is one catalog image for the table.
type ImageRow struct {
	Name, Updated string
	VCPU, MemMB   int
	Live          int
	Uses          int // lifetime lease grants
}

type collector struct {
	cfg    Config
	client *http.Client
	now    func() time.Time // the clock rates are computed against; tests set it

	prevCounters map[string]float64 // cumulative values from the last scrape
	prevAt       time.Time
	samples      []sample  // cumulative create counters, newest last, at most an hour
	prevCPU      [2]uint64 // busy, total jiffies

	events *eventBuffer // lease events from the SSE subscription

	// eventsClient has no overall timeout: the subscription is
	// long-lived and bounded by its context instead (the scrape client's
	// 5 s would kill the stream every five seconds).
	eventsClient *http.Client

	// diskTotal remembers the snapshot disk's size from the last fromHost
	// statfs (0 until it succeeds): the kept-bytes percentage (#126) is
	// a metrics value over a host value, so it is computed after both
	// sources have run.
	diskTotal uint64

	// prevDisk is the snapshot disk's /proc/diskstats counters at the
	// last collection, so the write rate and busy share are a delta; it
	// is only valid (prevDiskOK) once a sample has been read.
	prevDisk   diskSample
	prevDiskAt time.Time
	prevDiskOK bool

	// pressurePath and diskstatsPath are the /proc files the I/O readout
	// comes from; tests point them at fixtures.
	pressurePath, diskstatsPath string

	rowsMu  sync.Mutex
	lastRow []LeaseRow // the lease table of the last collect tick
}

// lastRows returns the rows set by the last fromDB: the holder and name
// lookup the events panel's tail column reads. Nil before the first
// collect tick.
func (c *collector) lastRows() []LeaseRow {
	c.rowsMu.Lock()
	defer c.rowsMu.Unlock()
	return c.lastRow
}

func newCollector(cfg Config) *collector {
	return &collector{
		cfg: cfg,
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: cfg.MetricsServerName},
		}},
		prevCounters:  map[string]float64{},
		now:           time.Now,
		pressurePath:  "/proc/pressure/io",
		diskstatsPath: "/proc/diskstats",
		events:        &eventBuffer{},
		eventsClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: cfg.MetricsServerName},
		}},
	}
}

// eventsURL is the lease events stream URL: the metrics URL's scheme
// and host (the same backend), /api/leases/events on the path.
func eventsURL(metricsURL string) string {
	u, err := url.Parse(metricsURL)
	if err != nil || u.Host == "" {
		return "https://127.0.0.1:8890/api/leases/events"
	}
	u.Path = "/api/leases/events"
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

// collect builds a snapshot. A failing source fills Err and leaves its
// fields at zero; the rest of the frame still renders.
func (c *collector) collect(ctx context.Context) Snapshot {
	now := c.now()
	s := Snapshot{At: now.Format("15:04:05"), ByState: map[string]int{}}
	var errs []string

	if fams, err := c.scrape(ctx); err != nil {
		errs = append(errs, "metrics: "+err.Error())
	} else {
		c.fromMetrics(&s, fams, now)
	}
	if err := c.fromHost(&s); err != nil {
		errs = append(errs, "host: "+err.Error())
	}
	if err := c.fromDB(&s, now); err != nil {
		errs = append(errs, "db: "+err.Error())
	}
	s.Services = c.services(ctx)
	for _, svc := range s.Services {
		if svc.State != "active" {
			s.Down++
		}
	}
	s.Events = c.eventLines(now)
	// Kept bytes as a share of the snapshot disk (#126): the
	// Notifications panel's "kept checkpoints use X% of the snapshot
	// disk". The disk
	// total comes from fromHost's statfs; with no total (statfs failed)
	// the strip stays off.
	if s.KeptBuildsBytes > 0 && c.diskTotal > 0 {
		s.KeptDiskPct = float64(s.KeptBuildsBytes) / float64(c.diskTotal) * 100
	}
	s.Err = strings.Join(errs, "; ")
	if s.Err != "" {
		// A scrape problem heads the events panel: the rest of the frame
		// may look calm while a source is quietly missing.
		s.Events = append([]EventLine{{Text: "scrape: " + s.Err, Style: "bad"}}, s.Events...)
	}
	return s
}

func (c *collector) scrape(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.MetricsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.MetricsToken)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	// The orchestrator passthrough follows a marker line; the text parser
	// treats it as a comment, so one parse covers both sections.
	p := expfmt.NewTextParser(model.UTF8Validation) // a zero TextParser panics
	return p.TextToMetricFamilies(resp.Body)
}

// value sums every sample of a family (gauge, counter or untyped), or
// returns the summed sample count/sum of a histogram via hist.
func value(f *dto.MetricFamily) float64 {
	if f == nil {
		return 0
	}
	var v float64
	for _, m := range f.GetMetric() {
		switch {
		case m.Gauge != nil:
			v += m.GetGauge().GetValue()
		case m.Counter != nil:
			v += m.GetCounter().GetValue()
		case m.Untyped != nil:
			v += m.GetUntyped().GetValue()
		}
	}
	return v
}

func hist(f *dto.MetricFamily) (sum, count float64) {
	if f == nil {
		return 0, 0
	}
	for _, m := range f.GetMetric() {
		if h := m.GetHistogram(); h != nil {
			sum += h.GetSampleSum()
			count += float64(h.GetSampleCount())
		}
	}
	return sum, count
}

func byLabel(f *dto.MetricFamily, label string) map[string]int {
	out := map[string]int{}
	if f == nil {
		return out
	}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == label {
				out[l.GetValue()] += int(m.GetGauge().GetValue())
			}
		}
	}
	return out
}

func (c *collector) fromMetrics(s *Snapshot, fams map[string]*dto.MetricFamily, now time.Time) {
	g := func(name string) float64 { return value(fams[name]) }
	s.Leases = int(g("spoond_leases_active"))
	s.ByState = byLabel(fams["spoond_leases"], "state")
	s.Queued = int(g("spoond_leases_queued"))
	s.QueuedOldest = g("spoond_leases_queued_oldest_seconds")
	s.Granted = int(g("spoond_leases_total"))
	s.Swept = int(g("spoond_lease_swept_total"))
	s.Running = int(g("spoond_node_running_sandboxes"))
	s.Limit = int(g("orchestrator_sandbox_limit"))
	s.Shares = int(g("spoond_shares_active"))
	s.Users = int(g("spoond_identity_users"))
	s.BuildsBusy = int(g("spoond_builds_in_flight"))
	s.FwConns = int(g("orchestrator_tcpfirewall_connections_active"))
	s.AuthFails = int(g("spoond_auth_failures_total"))
	s.Quota = int(g("spoond_quota_exceeded_total"))
	s.Throttled = int(g("spoond_auth_throttled_total"))
	s.Capacity = int(g("spoond_capacity_rejections_total"))
	s.BuildFails = int(g("spoond_builds_failed_total"))
	s.VCPUAlloc = int(g("orchestrator_sandbox_cpu_allocated"))
	s.MemAllocGiB = round1(g("orchestrator_sandbox_memory_allocated") / (1 << 30))
	// The GC's deleted-build counter, summed over its labels, feeds both
	// the mode (an actual deletion means deletion is on) and the host
	// panel's lifetime count.
	s.GCDeleted = int(value(fams["spoond_gc_deleted_total"]))
	// Kept checkpoints (#126): the pin count and their disk bytes; the
	// percentage of the snapshot disk they fill feeds the Notifications
	// panel (disk total comes from fromHost's statfs).
	s.KeptBuilds = int(g("spoond_kept_builds"))
	s.KeptBuildsBytes = int64(g("spoond_kept_builds_bytes"))
	if st := g("spoond_backend_start_time_seconds"); st > 0 {
		s.BackendUp = c.now().Sub(time.Unix(0, int64(st*1e9)))
	}

	// The GC's mode: a configured GC_DELETE=1 or an actually deleted
	// build means deletion is on; otherwise the GC is in its dry-run
	// default (it logs candidates but frees nothing).
	if os.Getenv("GC_DELETE") == "1" || s.GCDeleted > 0 {
		s.GCMode = "delete"
	} else {
		s.GCMode = "dry-run"
	}
	if f := fams["orchestrator_status"]; f != nil {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "version" {
					s.Version = l.GetValue()
				}
			}
		}
	}

	// Request rate: the delta since the last scrape.
	cur := map[string]float64{"req": g("spoond_http_requests_total")}
	if !c.prevAt.IsZero() {
		if dt := now.Sub(c.prevAt).Seconds(); dt > 0 {
			s.ReqPerSec = round1(max(cur["req"]-c.prevCounters["req"], 0) / dt)
		}
	}
	c.prevCounters, c.prevAt = cur, now

	// Creates and resumes happen a few times an hour, so their rate and
	// means come from windows over an hour of cumulative samples, not
	// from one scrape interval (where they would read 0 almost always).
	fs, fn := histBy(fams["spoond_create_duration_seconds"], "resume", "false")
	rs, rn := histBy(fams["spoond_create_duration_seconds"], "resume", "true")
	c.samples = append(c.samples, sample{at: now, freshSum: fs, freshN: fn, resumeSum: rs, resumeN: rn})
	for len(c.samples) > 1 && now.Sub(c.samples[0].at) > time.Hour {
		c.samples = c.samples[1:]
	}
	minute, hour := c.since(now.Add(-time.Minute)), c.samples[0]
	last := c.samples[len(c.samples)-1]
	s.CreatesPerMin = max(last.freshN+last.resumeN-minute.freshN-minute.resumeN, 0)
	s.CreateMs, s.ResumeMs = -1, -1
	if n := last.freshN - hour.freshN; n > 0 {
		s.CreateMs = round1((last.freshSum - hour.freshSum) / n * 1000)
	}
	if n := last.resumeN - hour.resumeN; n > 0 {
		s.ResumeMs = round1((last.resumeSum - hour.resumeSum) / n * 1000)
	}
}

// sample is one scrape's cumulative create/resume histogram totals.
type sample struct {
	at                                   time.Time
	freshSum, freshN, resumeSum, resumeN float64
}

// since returns the oldest sample taken at or after t (the newest if
// none is that recent). A counter reset (backend restart) can make a
// window negative; callers clamp at 0.
func (c *collector) since(t time.Time) sample {
	for _, smp := range c.samples {
		if !smp.at.Before(t) {
			return smp
		}
	}
	return c.samples[len(c.samples)-1]
}

// histBy sums a histogram's sample sum and count over the series whose
// label name has the given value.
func histBy(f *dto.MetricFamily, name, value string) (sum, count float64) {
	if f == nil {
		return 0, 0
	}
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == name && l.GetValue() == value {
				if h := m.GetHistogram(); h != nil {
					sum += h.GetSampleSum()
					count += float64(h.GetSampleCount())
				}
			}
		}
	}
	return sum, count
}

func (c *collector) fromHost(s *Snapshot) error {
	now := c.now()
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	busy, total, cores, err := cpuJiffies()
	if err != nil {
		return err
	}
	s.Cores = cores
	if c.prevCPU[1] != 0 && total > c.prevCPU[1] {
		s.CPUPct = round1(float64(busy-c.prevCPU[0]) / float64(total-c.prevCPU[1]) * 100)
	}
	c.prevCPU = [2]uint64{busy, total}

	mem, err := meminfo()
	if err != nil {
		return err
	}
	memGauges(s, mem)
	var st syscall.Statfs_t
	if err := syscall.Statfs(c.cfg.StoragePath, &st); err == nil && st.Blocks > 0 {
		s.DiskFreeGiB = round1(float64(st.Bavail) * float64(st.Bsize) / (1 << 30))
		s.DiskUsedPct = round1(float64(st.Blocks-st.Bfree) / float64(st.Blocks) * 100)
		c.diskTotal = st.Blocks * uint64(st.Bsize)
	}
	if err := syscall.Statfs("/", &st); err == nil && st.Blocks > 0 {
		s.RootFreeGiB = round1(float64(st.Bavail) * float64(st.Bsize) / (1 << 30))
		s.RootUsedPct = round1(float64(st.Blocks-st.Bfree) / float64(st.Blocks) * 100)
	}
	c.fromIO(s, now)
	return nil
}

// fromIO fills the I/O pressure and snapshot-disk throughput fields. A
// kernel without PSI (no /proc/pressure/io) leaves IOAvail false; a
// missing diskstats or an unknown device leaves the disk fields empty.
// Neither is an error: the rows are simply not drawn.
func (c *collector) fromIO(s *Snapshot, now time.Time) {
	if b, err := os.ReadFile(c.pressurePath); err == nil {
		if some10, some60, full10, full60, ok := parsePressure(b); ok {
			s.IOSome10, s.IOSome60 = some10, some60
			s.IOFull10, s.IOFull60 = full10, full60
			s.IOAvail = true
		}
	}
	device := c.diskDevice()
	if device == "" {
		return
	}
	b, err := os.ReadFile(c.diskstatsPath)
	if err != nil {
		return
	}
	smp, ok := parseDiskstats(b, device)
	if !ok {
		return
	}
	s.DiskDevice = device
	if c.prevDiskOK {
		if dt := now.Sub(c.prevDiskAt).Seconds(); dt > 0 {
			s.DiskWriteMB = round1(float64(smp.writeSectors-c.prevDisk.writeSectors) * 512 / dt / 1e6)
			s.DiskBusyPct = clampPct(round1(float64(smp.ioMs-c.prevDisk.ioMs) / (dt * 1000) * 100))
		}
	}
	c.prevDisk, c.prevDiskAt, c.prevDiskOK = smp, now, true
}

// diskDevice is the block device the snapshot disk lives on:
// DASH_DISK_DEVICE when set, else the device backing the storage path's
// mount. The whole disk is named (nvme0n1), not a partition on it.
func (c *collector) diskDevice() string {
	if c.cfg.DiskDevice != "" {
		return c.cfg.DiskDevice
	}
	return deviceForPath(c.cfg.StoragePath)
}

// parsePressure parses /proc/pressure/io: the `some` and `full` lines,
// each carrying avg10 and avg60 as percentages. ok is false when no
// recognised line was found (a kernel without PSI has no such file at
// all, so the caller sees a read error instead).
func parsePressure(b []byte) (some10, some60, full10, full60 float64, ok bool) {
	var haveSome, haveFull bool
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		var avg10, avg60 float64
		var bad bool
		for _, kv := range f[1:] {
			k, v, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			switch k {
			case "avg10":
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					avg10 = x
				} else {
					bad = true
				}
			case "avg60":
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					avg60 = x
				} else {
					bad = true
				}
			}
		}
		if bad {
			continue
		}
		switch f[0] {
		case "some":
			some10, some60, haveSome = avg10, avg60, true
		case "full":
			full10, full60, haveFull = avg10, avg60, true
		}
	}
	return some10, some60, full10, full60, haveSome || haveFull
}

// parseDiskstats returns the device's cumulative write-sector and
// busy-ms counters from /proc/diskstats (fields 10 and 13, 1-based:
// sectors written, ms doing I/O). ok is false when the device is not on
// the line or its counters do not parse.
func parseDiskstats(b []byte, device string) (diskSample, bool) {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 14 || f[2] != device {
			continue
		}
		ws, err1 := strconv.ParseUint(f[9], 10, 64)  // sectors written
		im, err2 := strconv.ParseUint(f[12], 10, 64) // ms doing I/O
		if err1 != nil || err2 != nil {
			return diskSample{}, false
		}
		return diskSample{writeSectors: ws, ioMs: im}, true
	}
	return diskSample{}, false
}

// diskSample is one device's cumulative /proc/diskstats counters.
type diskSample struct {
	writeSectors uint64
	ioMs         uint64
}

// clampPct keeps a busy share inside 0..100 (the diskstats ticks can
// outrun the wall clock on a multi-queue device, and a negative delta
// across a counter reset must not draw as negative).
func clampPct(v float64) float64 {
	return min(max(v, 0), 100)
}

// deviceForPath names the whole block device backing path's filesystem
// via sysfs, or "" when it cannot be resolved (a missing path, a
// non-block mount such as tmpfs).
func deviceForPath(path string) string {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return ""
	}
	major, minor := devMajorMinor(uint64(st.Dev))
	return blockDevice("/sys", major, minor)
}

// blockDevice resolves a major:minor under sysfs to its block device
// name. A partition (one carrying a `partition` attribute) reports its
// parent whole disk instead.
func blockDevice(sysfs string, major, minor uint32) string {
	p, err := filepath.EvalSymlinks(filepath.Join(sysfs, "dev", "block", fmt.Sprintf("%d:%d", major, minor)))
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(p, "partition")); err == nil {
		return filepath.Base(filepath.Dir(p))
	}
	return filepath.Base(p)
}

// devMajorMinor unpacks a Linux dev_t into its major and minor numbers
// (the glibc encoding the kernel hands back through statfs).
func devMajorMinor(dev uint64) (major, minor uint32) {
	major = uint32((dev>>8)&0xfff) | uint32((dev>>32)&0xfffff000)
	minor = uint32(dev&0xff) | uint32((dev>>12)&0xffffff00)
	return major, minor
}

// eventSubjectMax caps the events panel's subject column (holder,
// comment or owner); longer subjects end in ….
const eventSubjectMax = 24

// eventPanelRows is how many events the panel shows.
const eventPanelRows = 5

// eventLines renders the buffered lease events as the panel's lines,
// newest first, at most eventPanelRows of them:
//
//	HH:MM:SS  <type padded to 14>  <lease id 10>  <subject 32>  <detail>
//
// (the detail is the event's own text, build ids shortened)
//
// (the type and subject columns are sized to the events shown, the
// subject at most eventSubjectMax, so the columns line up and the detail
// keeps the room.)
//
// where subject is the holder, else the comment, else the owner.
// Without DASH_EVENTS_TOKEN the collector never subscribed to anything,
// and the panel says so instead of looking broken.
func (c *collector) eventLines(now time.Time) []EventLine {
	if c.cfg.EventsToken == "" {
		return []EventLine{{Text: "events need DASH_EVENTS_TOKEN", Style: "dim"}}
	}
	evs := c.events.newest(eventPanelRows)
	lines := make([]EventLine, 0, len(evs))
	rows := c.lastRows()
	names := c.userNames()
	// The type and subject columns are as wide as the events shown need
	// (the subject at most eventSubjectMax, cut with …), so the detail —
	// the part that varies most — gets the rest of the row.
	typeW, subjW := 0, 0
	for _, ev := range evs {
		if ev.Type == "gap" {
			continue
		}
		typeW = max(typeW, len([]rune(ev.Type)))
		subjW = max(subjW, len([]rune(eventSubject(ev, rows, names))))
	}
	subjW = min(subjW, eventSubjectMax)
	for _, ev := range evs {
		// Local time, like the header's clock on the same frame.
		at := ev.At.In(now.Location()).Format("15:04:05")
		if ev.Type == "gap" {
			// The stream skipped events (a reconnect past the backend's
			// buffer): say so instead of drawing an empty row.
			lines = append(lines, EventLine{Text: at + "  ┄ events missed while reconnecting", Style: "warn"})
			continue
		}
		text := strings.TrimRight(fmt.Sprintf("%s  %-*s  %-10s  %-*s  %s", at, typeW, ev.Type, ev.LeaseID,
			subjW, ellipsize(eventSubject(ev, rows, names), subjW), shortBuildIDs(ev.Detail)), " ")
		style := eventStyle(ev)
		if ev.Type == "job_exited" {
			style = jobExitedStyle(ev.Detail)
		}
		lines = append(lines, EventLine{Text: text, Style: style})
	}
	return lines
}

func cpuJiffies() (busy, total uint64, cores int, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu ") {
			fs := strings.Fields(line)[1:]
			for i, v := range fs {
				n, _ := strconv.ParseUint(v, 10, 64)
				total += n
				if i != 3 && i != 4 { // idle, iowait
					busy += n
				}
			}
		} else if strings.HasPrefix(line, "cpu") {
			cores++
		}
	}
	return busy, total, cores, sc.Err()
}

// memGauges fills the Memory and Hugepages gauges from /proc/meminfo
// values (kB, except the HugePages_* page counts).
func memGauges(s *Snapshot, mem map[string]uint64) {
	// Guest memory comes from the hugepage pool, which the kernel counts
	// as used whether or not a lease holds it. So Memory is the host's
	// own memory outside the pool, and Hugepages is lease capacity:
	// a microVM reserves its full size from the pool when it starts and
	// faults pages in as the guest touches them, so reserved-but-untouched
	// pages (HugePages_Rsvd, still inside HugePages_Free) are taken too.
	pageKB := mem["Hugepagesize"]
	poolKB := mem["HugePages_Total"] * pageKB
	if t := mem["MemTotal"] - poolKB; mem["MemTotal"] > poolKB {
		used := t - min(mem["MemAvailable"], t)
		s.MemTotalGiB = round1(float64(t) / (1 << 20))
		s.MemUsedGiB = round1(float64(used) / (1 << 20))
		s.MemUsedPct = round1(float64(used) / float64(t) * 100)
	}
	if t := mem["HugePages_Total"]; t > 0 {
		free := mem["HugePages_Free"] - min(mem["HugePages_Rsvd"], mem["HugePages_Free"])
		s.HugeUsedPct = round1(float64(t-free) / float64(t) * 100)
		s.HugeFreeGiB = round1(float64(free*pageKB) / (1 << 20))
	}
}

func meminfo() (map[string]uint64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) > 0 {
			out[k], _ = strconv.ParseUint(f[0], 10, 64)
		}
	}
	return out, nil
}

// streamEvents holds the one SSE subscription to the backend's
// /api/leases/events: it authenticates with the events-only
// EVENTS_TOKEN (DASH_EVENTS_TOKEN), resumes with Last-Event-ID so a
// reconnect replays exactly what was missed, backs off when the server
// refuses it, and appends everything it receives to the collector's own
// buffer — the one the events panel reads. One subscription per
// process, for the dashboard's lifetime.
func (c *collector) streamEvents(ctx context.Context, eventsURL, token string) {
	const retry = 5 * time.Second // the back-off ceiling
	backoff := time.Second
	lastID := ""
	for ctx.Err() == nil {
		err := c.streamEventsOnce(ctx, eventsURL, token, &lastID)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = time.Second // a clean end: start the climb over
		} else {
			backoff = min(backoff*2, retry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// streamEventsOnce runs one connection to the event stream until it
// ends, updating lastID as id lines arrive (a reconnect sends it as
// Last-Event-ID and the stream replays from there).
func (c *collector) streamEventsOnce(ctx context.Context, eventsURL, token string, lastID *string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if *lastID != "" {
		req.Header.Set("Last-Event-ID", *lastID)
	}
	resp, err := c.eventsClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("events: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ev *dashEvent
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		switch {
		case line == "": // the blank line ends one event block
			if ev != nil {
				c.events.add(*ev)
				ev = nil
			}
		case strings.HasPrefix(line, "id:"):
			*lastID = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "event:"):
			if ev == nil {
				ev = &dashEvent{}
			}
			ev.Type = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if ev == nil {
				ev = &dashEvent{}
			}
			var payload struct {
				At      time.Time `json:"at"`
				LeaseID string    `json:"lease_id"`
				Owner   string    `json:"owner"`
				Detail  string    `json:"detail"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &payload) == nil {
				ev.At = payload.At
				ev.LeaseID = shortID(payload.LeaseID)
				ev.Subject = payload.Owner
				ev.Detail = payload.Detail
			}
		}
	}
	return sc.Err()
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// fromDB reads live leases and the image catalog, read-only.
func (c *collector) fromDB(s *Snapshot, now time.Time) error {
	db, err := sql.Open("sqlite", "file:"+c.cfg.DBPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return err
	}
	defer db.Close()
	names := c.userNames()

	rows, err := db.Query(`SELECT id, image, owner, state, net_policy, name, comment, created_at, expires_at, persistent,
		holder, holder_url, pinned, last_action, last_action_at, class, preempted_at
		FROM leases ORDER BY created_at DESC LIMIT 40`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r LeaseRow
		var owner, created, expires string
		var comment string
		var holder, holderURL, lastAction, lastActionAt string
		var persistent, pinned int
		var class string
		var preemptedAt string
		if err := rows.Scan(&r.ID, &r.Image, &owner, &r.State, &r.Policy, &r.Name, &comment, &created, &expires, &persistent,
			&holder, &holderURL, &pinned, &lastAction, &lastActionAt, &class, &preemptedAt); err != nil {
			return err
		}
		r.Comment = comment
		r.Owner = names[owner]
		if r.Owner == "" {
			r.Owner = owner
		}
		if r.Policy == "" {
			r.Policy = "restricted"
		}
		r.Burst = class == "burst"
		r.Pinned = pinned == 1
		// A preempted lease shows the "·p" mark only while it is live
		// (suspended, running or recovered). A lost row keeps its
		// preempted_at in the store but must not claim to be waiting for
		// a resume, and the leases table marks lost rows on their own.
		r.Preempted = preemptedAt != "" && (r.State == "suspended" || r.State == "running" || r.State == "recovered")
		// An idle-suspended lease shows the "·i" mark while it is
		// suspended and no preemption claims the row (2.5, #129 part 2).
		r.IdleSuspended = r.State == "suspended" && preemptedAt == "" &&
			lastAction == "idle_suspend/suspend_idle"
		if len(r.ID) > 10 {
			r.ID = r.ID[:10]
		}
		r.Age = since(now, created)
		if persistent == 1 {
			r.Left = "∞"
		} else {
			r.Left = until(now, expires)
		}
		r.Holder, r.HolderURL = holder, holderURL
		r.LastAction = lastAction
		if t, err := time.Parse(time.RFC3339Nano, lastActionAt); err == nil {
			r.LastActionAt = t
		}
		s.Rows = append(s.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Lifetime grants per image. The table arrives with the backend's
	// migration 0005; until then the column reads 0.
	uses := map[string]int{}
	if u, err := db.Query(`SELECT image, uses FROM image_uses`); err == nil {
		for u.Next() {
			var image string
			var n int
			if u.Scan(&image, &n) == nil {
				uses[image] = n
			}
		}
		u.Close()
	}

	// Live leases per image, from the rows just read (running and
	// suspended leases hold an image's build).
	live := map[string]int{}
	for _, r := range s.Rows {
		if r.State == "running" || r.State == "suspended" {
			live[r.Image]++
		}
	}

	c.rowsMu.Lock()
	c.lastRow = s.Rows
	c.rowsMu.Unlock()

	// The capacity panel's burst count (#128 part 2): every live lease
	// in the burst class, counted in the store — not over the ≤40 rows
	// the leases panel displays, where an older burst lease would go
	// missing. The same live set the per-image counts read (running,
	// suspended and recovered hold or will hold hugepages).
	if err := db.QueryRow(`SELECT COUNT(*) FROM leases
		WHERE class = 'burst' AND state IN ('running','suspended','recovered')`).Scan(&s.Burst); err != nil {
		s.Burst = 0
		return fmt.Errorf("count burst leases: %w", err)
	}
	// The preempted count (#128 part 3): live leases the resume queue is
	// holding until capacity allows, wherever they sit in the row window.
	if err := db.QueryRow(`SELECT COUNT(*) FROM leases
		WHERE preempted_at != '' AND state IN ('running','suspended','recovered')`).Scan(&s.Preempted); err != nil {
		s.Preempted = 0
		return fmt.Errorf("count preempted leases: %w", err)
	}
	// The pinned-idle count (FS5): pinned leases the backend has flagged
	// (pinned_idle_since set). Visibility only; the Notifications panel
	// shows one aggregate message.
	if err := db.QueryRow(`SELECT COUNT(*) FROM leases
		WHERE pinned = 1 AND pinned_idle_since != ''`).Scan(&s.PinnedIdle); err != nil {
		s.PinnedIdle = 0
		return fmt.Errorf("count pinned-idle leases: %w", err)
	}

	imgs, err := db.Query(`SELECT name, vcpu, memory_mb, updated_at FROM images WHERE current_build_id != '' ORDER BY name`)
	if err != nil {
		return err
	}
	defer imgs.Close()
	for imgs.Next() {
		var r ImageRow
		var updated string
		if err := imgs.Scan(&r.Name, &r.VCPU, &r.MemMB, &updated); err != nil {
			return err
		}
		r.Updated = since(now, updated) + " ago"
		r.Live = live[r.Name]
		r.Uses = uses[r.Name]
		s.Images = append(s.Images, r)
	}
	return imgs.Err()
}

// userNames maps user ids to names from the identity store. Only id and
// name are read; credential hashes in the same file are ignored.
func (c *collector) userNames() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(c.cfg.UsersFile)
	if err != nil {
		return out
	}
	type user struct{ ID, Name string }
	add := func(raw json.RawMessage) bool {
		var list []user
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			for _, u := range list {
				out[u.ID] = u.Name
			}
			return true
		}
		var byID map[string]user
		if json.Unmarshal(raw, &byID) == nil {
			for id, u := range byID {
				if u.ID == "" {
					u.ID = id
				}
				out[u.ID] = u.Name
			}
			return len(out) > 0
		}
		return false
	}
	// {"users": [...]} or {"users": {id: user}}, or the bare list or map.
	var doc struct {
		Users json.RawMessage `json:"users"`
	}
	if json.Unmarshal(b, &doc) == nil && len(doc.Users) > 0 && add(doc.Users) {
		return out
	}
	add(b)
	return out
}

func (c *collector) services(ctx context.Context) []Service {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := make([]Service, 0, len(c.cfg.Services))
	args := append([]string{"is-active"}, c.cfg.Services...)
	b, _ := exec.CommandContext(ctx, "systemctl", args...).Output() // non-zero when any unit is down
	states := strings.Fields(string(b))
	for i, name := range c.cfg.Services {
		st := "unknown"
		if i < len(states) {
			st = states[i]
		}
		out = append(out, Service{Name: strings.TrimSuffix(name, ".service"), State: st})
	}
	return out
}

func since(now time.Time, ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "?"
	}
	return dur(now.Sub(t))
}

func until(now time.Time, ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "?"
	}
	if t.Before(now) {
		return "due"
	}
	return dur(t.Sub(now))
}

func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// uuidRe matches a build id (a UUID) inside an event detail.
var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// shortBuildIDs shortens the build ids in an event detail to their first
// 8 characters, so "paused into build 1ede0933-20dc-…" fits the panel.
func shortBuildIDs(detail string) string {
	return uuidRe.ReplaceAllStringFunc(detail, func(id string) string { return id[:8] })
}
