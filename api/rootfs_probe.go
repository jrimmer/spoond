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
	// rootfsProbeDuration bounds one probe exec. A probe that does not
	// answer within it is a failure: the guest is the only vantage point
	// from which a dead root disk is visible.
	rootfsProbeDuration = 10 * time.Second
	// rootfsProbeFailuresThreshold is how many consecutive failures turn
	// a sandbox into a crashed one.
	rootfsProbeFailuresThreshold = 3
)

// rootfsProbe is the probe script. It reads one 4096-byte block of the
// guest's root block device at a pseudo-random offset with O_DIRECT
// (iflag=direct), so the page cache cannot answer the read and the check
// reaches the actual device. findmnt names the mounted root device in
// the Debian-based base images; when it is missing or does not name a
// block device the script falls back to /dev/vda, the E2B root device.
// status=none keeps dd quiet on success; on EIO it writes the error to
// stderr, which the probe looks for.
//
// The offset comes from the kernel's /dev/urandom rather than $RANDOM:
// the base images run the script under dash, which has no RANDOM
// variable and would silently make every probe read block 0.
const rootfsProbe = `src=$(findmnt -no SOURCE / 2>/dev/null || true)
case "$src" in
  /dev/*) dev=$src ;;
  *) dev=/dev/vda ;;
esac
[ -b "$dev" ] || dev=/dev/vda
rnd=$(od -An -N2 -tu2 </dev/urandom 2>/dev/null | tr -d ' ')
case "$rnd" in
  ''|*[!0-9]*) rnd=0 ;;
esac
dd if="$dev" of=/dev/null bs=4096 count=1 skip=$(( (rnd % 32768) * 8 )) iflag=direct status=none`

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
	// rootfsProbeTimeout: the probe did not finish in time. The
	// substrate answered (envd kills the hung process and returns exit
	// 124) or the probe's context expired, but the guest did not read
	// its root device in time: a slow disk, logged and metered but never
	// counted toward recovery (see probeRootfsLeases).
	rootfsProbeTimeout
	// rootfsProbeTransport: the exec did not answer at all (transport
	// failure). On its own across every lease it points at the
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
// probe interval. It returns immediately when the probe is disabled.
func (s *Service) runRootfsProbeLoop(ctx context.Context) {
	every := s.rootfsProbeEvery()
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
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
// probe interval. When every probe in the pass fails with a transport
// error, the substrate itself is unreachable: nothing is counted or
// recovered, and the condition is logged once per outage.
func (s *Service) probeRootfsLeases(ctx context.Context) {
	every := s.rootfsProbeEvery()
	if every <= 0 || s.draining.Load() {
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
		if ok, seen := aliveAt[l.ID]; seen && now.Sub(ok) < every {
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
		if s.draining.Load() {
			// The drain began after this pass snapshotted its targets.
			// Stop running probes (and, below, stop counting): the
			// recovery takes the drain's write side, but there is no
			// point spending a 10 s exec on a node that is going down.
			return
		}
		outcomes[i] = s.probeRootfsOnce(ctx, l)
	}

	// If every target failed at the transport, the orchestrator is
	// unreachable, not the guests: log once and change nothing. A guest
	// that answers with an I/O error or a timeout has proven the
	// orchestrator is reachable, so a mixed pass still acts on the
	// affected leases. With a single running lease an all-transport
	// pass is indistinguishable from an orchestrator outage, so a lone
	// guest whose agent died is left to the crash reconcile and the
	// exec route's own errors rather than being marked lost here.
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
		if s.draining.Load() {
			// The admin drain started mid-pass: stop touching leases.
			return
		}
		switch outcomes[i] {
		case rootfsProbeOK:
			s.recordRootfsAlive(l.ID)
			s.resetRootfsFailures(l.ID)
		case rootfsProbeTimeout:
			// A slow disk is not a dead one. Since e2b-runtime P7 the
			// kernel lets a stalled NBD request wait up to 360 s instead
			// of failing it, so a probe can time out on a disk that will
			// recover; recovering from the last checkpoint would throw
			// away the lease's newer work. A disk that really dies past
			// that ceiling answers EIO, which counts. Timeouts are logged
			// and metered only, and they neither count nor reset.
			s.log.Printf("rootfs probe: lease %s probe timed out (slow disk?); not counted toward recovery", l.ID)
			if s.metrics != nil {
				s.metrics.RootfsProbeFailuresTotal.Inc()
			}
		case rootfsProbeEIO, rootfsProbeTransport:
			s.countRootfsFailure(ctx, l)
		}
	}
}

// probeRootfsOnce runs the probe against one lease and classifies the
// result.
func (s *Service) probeRootfsOnce(parent context.Context, l *Lease) rootfsProbeOutcome {
	ctx, cancel := context.WithTimeout(parent, rootfsProbeDuration)
	defer cancel()
	res, err := s.sub.Exec(ctx, l.SandboxID, substrate.ExecRequest{
		Args:    []string{"/bin/sh", "-c", rootfsProbe},
		Timeout: rootfsProbeDuration,
	})
	if err != nil {
		// A substrate that returns the probe's own deadline as an error
		// is still a timeout (some backends cancel the stream when the
		// context fires). A cancelled parent means the loop is stopping,
		// not that the guest failed, so it stays a transport result.
		if parent.Err() == nil && ctx.Err() == context.DeadlineExceeded {
			return rootfsProbeTimeout
		}
		return rootfsProbeTransport
	}
	if res.ExitCode == 0 {
		return rootfsProbeOK
	}
	if isIOError(res.Stderr) || isIOError(res.Stdout) {
		return rootfsProbeEIO
	}
	if rootfsProbeTimedOut(res) {
		// The substrate killed a probe that outlived its timer and
		// returned exit 124 rather than an error. A dead root disk that
		// presents as a hang lands here, so it must count.
		return rootfsProbeTimeout
	}
	// A non-zero exit for any other reason (dd missing, bad argv) says
	// nothing about the root disk: the guest answered, so it is alive.
	// Log it so a probe that silently degraded to a no-op (a base image
	// without dd, or a device that rejects O_DIRECT) is visible rather
	// than invisible.
	s.log.Printf("rootfs probe: lease %s sandbox %s exit=%d without an I/O error; treating the guest as alive (stderr=%q)",
		l.ID, l.SandboxID, res.ExitCode, tailStr(res.Stderr, 200))
	return rootfsProbeOK
}

// rootfsProbeTimedOut reports whether an exec result is the substrate's
// own timeout marker. The e2b backend kills a process that outlives its
// timer and returns exit code 124 with a "[spoond] exec timed out" line
// and a nil error, so a hung probe can arrive as an ordinary non-zero
// result; the wording check also catches other substrates that report a
// timeout the same way.
func rootfsProbeTimedOut(res substrate.ExecResult) bool {
	if res.ExitCode == 124 {
		return true
	}
	text := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	return strings.Contains(text, "timed out") || strings.Contains(text, "timeout")
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
	// The admin drain and this recovery are mutually exclusive. Holding
	// the read side across the whole recovery means a drain either
	// completes before the recovery starts or waits for it, and the
	// drain takes the write side around SetDraining and
	// draining.Store(true). That closes the gap a pass-length check
	// leaves open: a drain that begins mid-pass cannot overlap a
	// recovery for a lease it has not paused yet (spoond-5ca).
	s.drainGate.RLock()
	if s.draining.Load() {
		s.drainGate.RUnlock()
		// The drain is pausing this lease (or about to): its own execs
		// are the liveness evidence now. Drop the count rather than
		// acting on it out of turn.
		s.resetRootfsFailures(l.ID)
		return
	}
	s.store.mu.Lock()
	if l.busy || l.released || !l.live() {
		s.store.mu.Unlock()
		s.drainGate.RUnlock()
		// The lease was already being checkpointed, restarted or
		// recovered: that operation's own execs are live evidence that
		// the sandbox is reachable, so the accumulated count is dropped
		// rather than acted on out of turn.
		s.resetRootfsFailures(l.ID)
		return
	}
	l.busy = true
	s.store.mu.Unlock()
	// endBusy runs before the gate is released (defer LIFO), so a drain
	// that acquires the write side next cannot see the lease busy for an
	// operation that has already finished.
	defer s.drainGate.RUnlock()
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
	// happened before the recovered/lost event that follows. The reason is
	// stamped on the lease now, so a lost lease answers with the root-disk
	// cause even though the recovery event below carries its own detail.
	s.store.mu.Lock()
	setLostReason(l, "root disk unreadable (I/O errors)")
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, "root disk unreadable (I/O errors)")

	if err := s.sub.Delete(ctx, l.SandboxID); err != nil {
		s.log.Printf("rootfs probe: lease %s delete sandbox %s: %v", l.ID, l.SandboxID, err)
	}
	s.deleteSandboxRow(l.SandboxID)
	s.recoverOneLease(ctx, l)
	// If the recovery marked the lease lost, a version it started from is
	// no longer in use and retention may drop it now (S5).
	if l.State == "lost" {
		s.rerunSnapshotRetention(ctx, l)
	}
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
