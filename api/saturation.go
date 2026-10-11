package api

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// The saturation gate (#136 stage 2). spoond admits by memory, but a box
// can be saturated with memory to spare: parallel snapshot work pins the
// NVMe (the 2026-10-06 NBD stalls), and CPU-heavy guests (test gates
// under -race beside several workers) overcommit the cores. While the
// host's I/O or CPU pressure (Linux PSI, 10 s average) is over its
// target, new work — a create, clone or fork — waits: 503 capacity_wait
// with Retry-After, or the admission queue for a create with "wait".
// Resumes, restores and restarts of existing leases are not gated, so a
// running job is never stalled by the gate. CPU stays out of fair shares
// (#146): this is a box-wide pause on new work, not a per-owner charge.

const (
	// DefaultSaturationIOFullPct is SATURATION_IO_FULL_PCT when unset:
	// new work waits while every task was stalled on I/O at least this
	// share of the last 10 s. #136's stress test found the stall shape
	// at 40-60 %, with 20 % a safe margin before it.
	DefaultSaturationIOFullPct = 20
	// DefaultSaturationCPUSomePct is SATURATION_CPU_SOME_PCT when unset:
	// new work waits while some task waited for a CPU at least this
	// share of the last 10 s.
	DefaultSaturationCPUSomePct = 75
	// pressureCacheTTL bounds how often the gate rereads /proc/pressure.
	pressureCacheTTL = 2 * time.Second
)

// errHostSaturated is the gate's refusal. It wraps substrate.ErrCapacity,
// so every path answers it as a waitable capacity_wait.
type errHostSaturated struct {
	resource      string // "io" or "cpu"
	pct, limitPct float64
}

func (e *errHostSaturated) Error() string {
	return fmt.Sprintf("host saturated: %s pressure %.0f%% is over %.0f%% (10 s average); new leases wait", e.resource, e.pct, e.limitPct)
}

func (e *errHostSaturated) Unwrap() error { return substrate.ErrCapacity }

// pressureGate caches the host's PSI readings for the gate.
type pressureGate struct {
	mu   sync.Mutex
	root string // /proc/pressure; tests point it at a temp dir
	// ok also remembers the last read's outcome, so an unreadable file
	// is logged once, not every pressureCacheTTL.
	at     time.Time
	ioFull float64
	cpu    float64
	ok     bool
}

// readPSIAvg10 reads the avg10 value of the "some" or "full" line of a
// PSI file (/proc/pressure/<resource>).
func readPSIAvg10(path, kind string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != kind {
			continue
		}
		for _, kv := range fields[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				return strconv.ParseFloat(v, 64)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s: no %q avg10", path, kind)
}

// hostPressure returns the cached I/O-full and CPU-some 10 s pressure,
// rereading at most every pressureCacheTTL. ok is false when the files
// cannot be read: the gate then stays open (a missing signal never stops
// work).
func (s *Service) hostPressure() (ioFull, cpuSome float64, ok bool) {
	g := &s.pressure
	g.mu.Lock()
	defer g.mu.Unlock()
	now := s.now()
	if !g.at.IsZero() && now.Sub(g.at) < pressureCacheTTL {
		return g.ioFull, g.cpu, g.ok
	}
	root := g.root
	if root == "" {
		root = "/proc/pressure"
	}
	io, ierr := readPSIAvg10(filepath.Join(root, "io"), "full")
	cpu, cerr := readPSIAvg10(filepath.Join(root, "cpu"), "some")
	wasOK := g.ok || g.at.IsZero()
	g.at, g.ioFull, g.cpu, g.ok = now, io, cpu, ierr == nil && cerr == nil
	if !g.ok {
		if wasOK {
			s.log.Printf("saturation: host pressure unreadable (io %v, cpu %v); the gate stays open until it can read it", ierr, cerr)
		}
	} else if s.metrics != nil {
		s.metrics.HostPressure.WithLabelValues("io").Set(io)
		s.metrics.HostPressure.WithLabelValues("cpu").Set(cpu)
	}
	return g.ioFull, g.cpu, g.ok
}

// checkSaturation is the gate: nil when new work may start, else an
// *errHostSaturated (a waitable capacity refusal). A target of 0 turns
// that check off; both 0 (the zero ServiceConfig) is no gate at all.
// quiet skips the refusal metric: the admission queue's retries of one
// waiting create, and the pool refill, are not new refusals.
func (s *Service) checkSaturation(quiet bool) error {
	ioLim, cpuLim := s.cfg.SaturationIOFullPct, s.cfg.SaturationCPUSomePct
	if ioLim <= 0 && cpuLim <= 0 {
		return nil
	}
	io, cpu, ok := s.hostPressure()
	if !ok {
		return nil
	}
	var err *errHostSaturated
	switch {
	case ioLim > 0 && io >= ioLim:
		err = &errHostSaturated{resource: "io", pct: io, limitPct: ioLim}
	case cpuLim > 0 && cpu >= cpuLim:
		err = &errHostSaturated{resource: "cpu", pct: cpu, limitPct: cpuLim}
	default:
		return nil
	}
	if s.metrics != nil && !quiet {
		s.metrics.SaturationRefusals.WithLabelValues(err.resource).Inc()
	}
	return err
}

// saturationDetail names the gate's refusal for the queue's `queued`
// event; "" for any other refusal.
func saturationDetail(err error) string {
	var sat *errHostSaturated
	if errors.As(err, &sat) {
		return fmt.Sprintf("host saturated (%s %.0f%%)", sat.resource, sat.pct)
	}
	return ""
}
