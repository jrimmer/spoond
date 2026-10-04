package spoonddash

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
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
	Leases     int            `json:"leases"`
	ByState    map[string]int `json:"byState"`
	Queued     int            `json:"queued"`
	Granted    int            `json:"granted"` // cumulative leases granted
	Swept      int            `json:"swept"`
	Running    int            `json:"running"` // sandboxes on the node
	Limit      int            `json:"limit"`   // node sandbox limit
	Shares     int            `json:"shares"`
	Users      int            `json:"users"`
	BuildsBusy int            `json:"buildsBusy"`

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
	UptimeH     float64 `json:"uptimeH"`
	RootUsedPct float64 `json:"rootUsedPct"` // the root filesystem, not the snapshot store
	RootFreeGiB float64 `json:"rootFreeGiB"`

	Down int `json:"down"` // units not active

	// GCDeleted is the builds the GC has deleted, summed over
	// spoond_gc_deleted_total's labels. Shown next to the GC's mode;
	// left out of the frame while it is 0.
	GCDeleted int `json:"gcDeleted"`

	// GCMode labels the snapshot GC: "delete" (GC_DELETE=1, or the
	// backend has actually deleted something) or "dry-run".
	GCMode string `json:"gcMode"`

	// CertNotAfter is the served TLS pair's expiry (DASH_TLS_CERT); zero
	// when the dashboard serves plain HTTP. Only the banner reads it.
	CertNotAfter time.Time `json:"-"`

	// Rendered as HTML element patches, not sent as signals.
	Services []Service   `json:"-"`
	Rows     []LeaseRow  `json:"-"`
	Images   []ImageRow  `json:"-"`
	Events   []EventLine `json:"-"` // the events panel, newest first
}

// EventLine is one line of the events panel: text plus the grid style
// it is drawn with ("dim" for journal noise, "warn" for automatic
// held-lease actions, "bad" for a scrape problem).
type EventLine struct {
	Text  string
	Style string
}

// Service is a systemd unit and its state.
type Service struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// LeaseRow is one live lease for the table. Holder/HolderURL/HoldState
// name what holds the lease (HoldState "" when unheld, "active" or
// "lapsed"); LastAction/LastActionAt record the last automatic
// held-lease action ("rule/action", e.g. "idle/suspend_idle").
type LeaseRow struct {
	ID, Image, Owner, State, Policy, Name string
	Holder, HolderURL, HoldState          string
	LastAction                            string
	LastActionAt                          time.Time
	Age, Left                             string
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
}

func newCollector(cfg Config) *collector {
	return &collector{
		cfg: cfg,
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: cfg.MetricsServerName},
		}},
		prevCounters: map[string]float64{},
		now:          time.Now,
	}
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
	c.readActivity(&s, now)
	c.readCert(&s)
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
	s.GCDeleted = int(value(fams["spoond_gc_deleted_total"]))
	// The GC's mode: a configured GC_DELETE=1 or an actually deleted
	// build means deletion is on; otherwise the GC is in its dry-run
	// default (it logs candidates but frees nothing).
	if os.Getenv("GC_DELETE") == "1" || value(fams["spoond_gc_deleted_total"]) > 0 {
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
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			up, _ := strconv.ParseFloat(f[0], 64)
			s.UptimeH = round1(up / 3600)
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
	}
	if err := syscall.Statfs("/", &st); err == nil && st.Blocks > 0 {
		s.RootFreeGiB = round1(float64(st.Bavail) * float64(st.Bsize) / (1 << 30))
		s.RootUsedPct = round1(float64(st.Blocks-st.Bfree) / float64(st.Blocks) * 100)
	}
	return nil
}

// readCert stamps the served TLS pair's expiry for the banner: a
// certificate within 30 days of expiring needs a person before basic
// auth starts failing in browsers. A missing or unreadable file is not
// an error here (the dashboard then serves plain HTTP or keeps the last
// good frame); the server itself reports real certificate problems.
func (c *collector) readCert(s *Snapshot) {
	if c.cfg.TLSCert == "" {
		return
	}
	b, err := os.ReadFile(c.cfg.TLSCert)
	if err != nil {
		return
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return
	}
	if crt, err := x509.ParseCertificate(blk.Bytes); err == nil {
		s.CertNotAfter = crt.NotAfter
	}
}

// readActivity fills the events panel: one line per automatic
// held-lease action recorded on a live lease (last_action, newest
// first), then the backend's last journal lines (journalctl, the way
// services uses systemctl). A host without journald — CI — just
// contributes the held-lease lines.
func (c *collector) readActivity(s *Snapshot, now time.Time) {
	type heldAt struct {
		line EventLine
		at   time.Time
	}
	var held []heldAt
	for _, r := range s.Rows {
		if r.LastAction == "" || r.LastActionAt.IsZero() {
			continue
		}
		held = append(held, heldAt{
			line: EventLine{Text: fmt.Sprintf("held lease %s: %s (%s ago)", r.ID, r.LastAction, dur(now.Sub(r.LastActionAt))), Style: "warn"},
			at:   r.LastActionAt,
		})
	}
	sort.Slice(held, func(i, j int) bool { return held[i].at.After(held[j].at) })
	for i, h := range held {
		if i == 4 {
			break
		}
		s.Events = append(s.Events, h.line)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	unit := c.cfg.ActivityUnit
	if unit == "" {
		unit = "spoond-backend"
	}
	out, err := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", "8", "--no-pager", "--quiet", "-o", "short-iso").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// "2026-10-04T07:19:00+00:00 spoond-backend[123]: message" — keep
		// the time of day and the message.
		f := strings.SplitN(line, " ", 3)
		if len(f) < 3 {
			continue
		}
		when := f[0]
		if i := strings.IndexByte(when, 'T'); i >= 0 {
			when = when[i+1:]
		}
		s.Events = append(s.Events, EventLine{Text: when + " " + f[2], Style: "dim"})
	}
	if n := len(s.Events); n > 8 {
		s.Events = s.Events[:8]
	}
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

// fromDB reads live leases and the image catalog, read-only.
func (c *collector) fromDB(s *Snapshot, now time.Time) error {
	db, err := sql.Open("sqlite", "file:"+c.cfg.DBPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return err
	}
	defer db.Close()
	names := c.userNames()

	rows, err := db.Query(`SELECT id, image, owner, state, net_policy, name, created_at, expires_at, persistent,
		holder, holder_url, hold_expires_at, last_action, last_action_at
		FROM leases ORDER BY created_at DESC LIMIT 40`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r LeaseRow
		var owner, created, expires string
		var holder, holderURL, holdExpires, lastAction, lastActionAt string
		var persistent int
		if err := rows.Scan(&r.ID, &r.Image, &owner, &r.State, &r.Policy, &r.Name, &created, &expires, &persistent,
			&holder, &holderURL, &holdExpires, &lastAction, &lastActionAt); err != nil {
			return err
		}
		r.Owner = names[owner]
		if r.Owner == "" {
			r.Owner = owner
		}
		if r.Policy == "" {
			r.Policy = "restricted"
		}
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
		if holder != "" {
			// The API's hold_state: a held lease with no hold_expires_at is
			// a lapsed hold (it ran out unrenewed and stays held).
			if holdExpires == "" {
				r.HoldState = "lapsed"
			} else {
				r.HoldState = "active"
			}
		}
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
