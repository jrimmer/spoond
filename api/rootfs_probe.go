package api

import (
	"context"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Rootfs liveness probe (spoond-5ca, incident 2026-10-06). The kernel NBD
// connections backing some guests' root disks can die (a host disk stall
// past the kernel ceiling); those guests then get EIO on every uncached
// read, execs answer HTTP 500 "exec failed", and spoond otherwise keeps
// their leases running forever. Every ROOTFS_PROBE_SECS a cheap exec
// reads one block of the guest's root block device bypassing the page
// cache, at a random offset. A transport failure, a timeout or an
// "Input/output error" is a failure; after rootfsProbeFailuresThreshold
// consecutive failures the sandbox is treated as crashed and goes
// through the same per-lease recovery as the crash reconcile (from the
// last checkpoint, or lost).

const (
	// DefaultRootfsProbeSecs is how often the probe runs against running
	// leases when ROOTFS_PROBE_SECS is unset. 0 disables it.
	DefaultRootfsProbeSecs = 120
	// rootfsProbeTimeout bounds one probe exec. A probe that does not
	// answer within it is a failure: the guest is the only vantage point
	// from which a dead root disk is visible.
	rootfsProbeTimeout = 10 * time.Second
	// rootfsProbeFailuresThreshold is how many consecutive failures turn
	// a sandbox into a crashed one.
	rootfsProbeFailuresThreshold = 3
)

// rootfsProbe is the probe script. It reads one 4096-byte block of the
// guest's root block device at a random offset with O_DIRECT
// (iflag=direct), so the page cache cannot answer the read and the check
// reaches the actual device. findmnt names the mounted root device in
// the Debian-based base images; when it is missing or does not name a
// block device the script falls back to /dev/vda, the E2B root device.
// status=none keeps dd quiet on success; on EIO it writes the error to
// stderr, which the probe looks for.
const rootfsProbe = `src=$(findmnt -no SOURCE / 2>/dev/null || true)
case "$src" in
  /dev/*) dev=$src ;;
  *) dev=/dev/vda ;;
esac
[ -b "$dev" ] || dev=/dev/vda
dd if="$dev" of=/dev/null bs=4096 count=1 skip=$((RANDOM*8)) iflag=direct status=none`

// rootfsProbeOutcome classifies one probe.
type rootfsProbeOutcome int

const (
	// rootfsProbeOK: the read succeeded (or the exec failed for a reason
	// that says nothing about the root disk, e.g. dd is missing). The
	// guest answered and is alive.
	rootfsProbeOK rootfsProbeOutcome = iota
	// rootfsProbeEIO: the block device answered an I/O error. This is
	// unambiguous guest-level evidence, even if every lease reports it.
	rootfsProbeEIO
	// rootfsProbeTransport: the exec did not answer (transport failure or
	// timeout). On its own across every lease it points at the
	// orchestrator, not the guests.
	rootfsProbeTransport
)

// rootfsProbeFailure is a lease's consecutive probe-failure count. The
// sandbox id is kept so a replacement sandbox (a recovery) starts with a
// clean count.
type rootfsProbeFailure struct {
	sandboxID string
	count     int
}

// runRootfsProbeLoop runs the rootfs liveness probe every
// rootfsProbeSecs. It returns immediately when the probe is disabled.
func (s *Service) runRootfsProbeLoop(ctx context.Context) {
	if s.rootfsProbeSecs <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(s.rootfsProbeSecs) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.probeRootfsLeases(ctx)
		}
	}
}

// probeRootfsLeases runs one probe pass over the running leases. It
// skips the admin drain, busy leases (checkpoint, restart, suspend in
// flight) and leases with a successful exec inside the last
// rootfsProbeSecs. When every probe in the pass fails with a transport
// error, the substrate itself is unreachable: nothing is counted or
// recovered, and the condition is logged once per outage.
func (s *Service) probeRootfsLeases(ctx context.Context) {
	if s.rootfsProbeSecs <= 0 || s.draining.Load() {
		return
	}
	now := s.now()

	// Snapshot the last-success times before taking the store lock:
	// countRootfsFailure takes rootfsProbeMu and then the store lock, so
	// this path must never hold the store lock while taking rootfsProbeMu.
	aliveAt := s.rootfsAliveSnapshot()

	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.released || !l.live() || l.busy {
			continue
		}
		if ok, seen := aliveAt[l.ID]; seen && now.Sub(ok) < time.Duration(s.rootfsProbeSecs)*time.Second {
			// A recent successful exec already proved the guest is
			// alive; probing anyway only burns an exec.
			continue
		}
		targets = append(targets, l)
	}
	s.store.mu.Unlock()
	if len(targets) == 0 {
		s.clearRootfsAllFailed()
		return
	}

	outcomes := make([]rootfsProbeOutcome, len(targets))
	for i, l := range targets {
		if ctx.Err() != nil {
			return
		}
		outcomes[i] = s.probeRootfsOnce(ctx, l)
	}

	// If every target failed without a single I/O error, the
	// orchestrator is unreachable, not the guests: log once and change
	// nothing. A guest that answers with an I/O error has proven the
	// orchestrator is reachable, so a mixed pass still acts on the
	// affected leases.
	allTransport := len(targets) > 0
	for _, o := range outcomes {
		if o != rootfsProbeTransport {
			allTransport = false
		}
	}
	if allTransport {
		if !s.rootfsAllFailedLogged() {
			s.log.Printf("rootfs probe: every probe across %d running lease(s) failed; treating it as the orchestrator being unreachable and taking no action", len(targets))
			s.setRootfsAllFailedLogged()
		}
		return
	}
	s.clearRootfsAllFailed()

	for i, l := range targets {
		switch outcomes[i] {
		case rootfsProbeOK:
			s.recordRootfsAlive(l.ID)
			s.resetRootfsFailures(l.ID)
		case rootfsProbeEIO, rootfsProbeTransport:
			s.countRootfsFailure(ctx, l)
		}
	}
}

// probeRootfsOnce runs the probe against one lease and classifies the
// result.
func (s *Service) probeRootfsOnce(parent context.Context, l *Lease) rootfsProbeOutcome {
	ctx, cancel := context.WithTimeout(parent, rootfsProbeTimeout)
	defer cancel()
	res, err := s.sub.Exec(ctx, l.SandboxID, substrate.ExecRequest{
		Args:    []string{"/bin/sh", "-c", rootfsProbe},
		Timeout: rootfsProbeTimeout,
	})
	if err != nil {
		return rootfsProbeTransport
	}
	if res.ExitCode == 0 {
		return rootfsProbeOK
	}
	if isIOError(res.Stderr) || isIOError(res.Stdout) {
		return rootfsProbeEIO
	}
	// A non-zero exit for any other reason (dd missing, bad argv) says
	// nothing about the root disk: the guest answered, so it is alive.
	return rootfsProbeOK
}

// isIOError reports whether s carries an "Input/output error", the
// kernel's EIO text. The comparison is case-insensitive and ignores
// surrounding whitespace.
func isIOError(s string) bool {
	return strings.Contains(strings.ToLower(s), "input/output error")
}

// countRootfsFailure records one probe failure for a lease and, at the
// threshold, recovers the sandbox like a crash.
func (s *Service) countRootfsFailure(ctx context.Context, l *Lease) {
	s.rootfsProbeMu.Lock()
	f := s.rootfsProbeFails[l.ID]
	if f == nil || f.sandboxID != l.SandboxID {
		f = &rootfsProbeFailure{sandboxID: l.SandboxID}
		s.rootfsProbeFails[l.ID] = f
	}
	f.count++
	count := f.count
	s.rootfsProbeMu.Unlock()

	if s.metrics != nil {
		s.metrics.RootfsProbeFailuresTotal.Inc()
	}
	s.log.Printf("rootfs probe: lease %s sandbox %s failed (%d/%d)", l.ID, l.SandboxID, count, rootfsProbeFailuresThreshold)
	if count < rootfsProbeFailuresThreshold {
		return
	}
	s.recoverDeadRootfs(ctx, l)
}

// recoverDeadRootfs treats a lease whose root disk answered I/O errors
// as a crashed sandbox: it emits the lost event that names the cause,
// deletes the dead sandbox through the substrate first, then runs the
// shared per-lease recovery (from the last checkpoint, or lost). It
// takes the lease's busy flag so no other lifecycle operation races it,
// and skips a lease that is no longer running or already busy.
func (s *Service) recoverDeadRootfs(parent context.Context, l *Lease) {
	s.store.mu.Lock()
	if l.busy || l.released || !l.live() {
		s.store.mu.Unlock()
		s.resetRootfsFailures(l.ID)
		return
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)

	s.log.Printf("rootfs probe: lease %s root disk unreadable (I/O errors); recovering from the last checkpoint", l.ID)
	if s.metrics != nil {
		s.metrics.RootfsDeadTotal.Inc()
	}
	// Detached from the pass's context: a cancelled loop must not cut a
	// recovery short and turn a recoverable lease into a lost one.
	ctx, cancel := context.WithTimeout(context.Background(), crashTestTimeout)
	defer cancel()
	// The marker goes first: a stream reader sees why the recovery
	// happened before the recovered/lost event that follows.
	s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, "root disk unreadable (I/O errors)")

	if err := s.sub.Delete(ctx, l.SandboxID); err != nil {
		s.log.Printf("rootfs probe: lease %s delete sandbox %s: %v", l.ID, l.SandboxID, err)
	}
	s.deleteSandboxRow(l.SandboxID)
	s.recoverOneLease(ctx, l)
	s.forgetRootfs(l.ID)
}

// recordRootfsAlive records a successful exec on a lease. A recent
// success makes the next probe pass skip the lease.
func (s *Service) recordRootfsAlive(leaseID string) {
	if leaseID == "" {
		return
	}
	s.rootfsProbeMu.Lock()
	s.rootfsProbeOK[leaseID] = s.now()
	s.rootfsProbeMu.Unlock()
}

// rootfsAliveSnapshot copies the last-success map so a caller can read
// it without holding rootfsProbeMu across other locks.
func (s *Service) rootfsAliveSnapshot() map[string]time.Time {
	s.rootfsProbeMu.Lock()
	defer s.rootfsProbeMu.Unlock()
	out := make(map[string]time.Time, len(s.rootfsProbeOK))
	for id, t := range s.rootfsProbeOK {
		out[id] = t
	}
	return out
}

// resetRootfsFailures clears a lease's consecutive failure count (after
// a success or a fresh sandbox). The last-success mark is left alone:
// the caller decides whether a recent exec still proves liveness.
func (s *Service) resetRootfsFailures(leaseID string) {
	s.rootfsProbeMu.Lock()
	delete(s.rootfsProbeFails, leaseID)
	s.rootfsProbeMu.Unlock()
}

// forgetRootfs cleans up a released lease's probe state.
func (s *Service) forgetRootfs(leaseID string) {
	s.rootfsProbeMu.Lock()
	delete(s.rootfsProbeFails, leaseID)
	delete(s.rootfsProbeOK, leaseID)
	s.rootfsProbeMu.Unlock()
}

// rootfsAllFailedLogged reports whether the "every probe failed" line
// was already logged for the current outage.
func (s *Service) rootfsAllFailedLogged() bool {
	s.rootfsProbeMu.Lock()
	defer s.rootfsProbeMu.Unlock()
	return s.rootfsProbeAllFailedLogged
}

// setRootfsAllFailedLogged records that the outage line was logged.
func (s *Service) setRootfsAllFailedLogged() {
	s.rootfsProbeMu.Lock()
	s.rootfsProbeAllFailedLogged = true
	s.rootfsProbeMu.Unlock()
}

// clearRootfsAllFailed resets the outage-logged guard once a pass sees
// at least one non-transport result.
func (s *Service) clearRootfsAllFailed() {
	s.rootfsProbeMu.Lock()
	s.rootfsProbeAllFailedLogged = false
	s.rootfsProbeMu.Unlock()
}
