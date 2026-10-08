package api

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Lease secrets (#80): an optional {name: value} object on lease create
// and on exec, delivered as files inside the guest — never as
// environment variables or argv. Before anything runs, the backend
// mounts a tmpfs at /run/secrets (mode 0700, owned by the user exec
// runs as) when it is not mounted yet and writes every secret as
// /run/secrets/<name>, mode 0600, through the substrate's file API.
//
// Create-time secrets stay for the lease's life and are re-written
// after a resume, restart or crash recovery (a fresh sandbox never had
// the tmpfs; after a snapshot resume the rewrite is a no-op refresh).
// The tmpfs is guest memory, so a pause, checkpoint, fork or clone
// snapshot carries the files with it. Exec-time secrets are written before the
// command and removed after it finishes. The values live in this
// process's memory only: never in the store, logs, error strings or
// metrics, and never returned by any endpoint — a backend restart
// loses them, so callers re-send them on their next exec.

const (
	// secretsDir is where a lease's secrets appear inside the guest.
	secretsDir = "/run/secrets"

	// maxSecretsPerRequest bounds how many secrets one create or exec
	// request may carry.
	maxSecretsPerRequest = 32

	// maxSecretsTotalBytes bounds the combined size of a request's
	// secret values (64 KiB).
	maxSecretsTotalBytes = 64 << 10

	// secretsMode is the mode every secret file gets: owner-only, so
	// other guest users cannot read them.
	secretsMode os.FileMode = 0o600

	// secretsStageTimeout bounds the mount exec and the file writes of
	// one staging pass.
	secretsStageTimeout = 30 * time.Second
)

// secretNameRe constrains secret names: they become file names under
// /run/secrets, so only [A-Za-z0-9_.-] up to 64 characters is accepted
// (no path separators, nothing that escapes the directory).
var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// secretMountScript mounts a 0700 tmpfs owned by the exec user at
// /run/secrets when that is not already a tmpfs mount. It runs inside
// the guest (as the user exec runs as, so uid/gid resolve to the exec
// user's) and is idempotent, so concurrent staging passes are safe.
const secretMountScript = `grep -q ' /run/secrets tmpfs ' /proc/mounts || ` +
	`{ mkdir -p /run/secrets && mount -t tmpfs -o mode=0700,uid=$(id -u),gid=$(id -g) tmpfs /run/secrets; }`

// secretsScrubScript removes every entry under /run/secrets and then
// lists what is left. It backs the named-snapshot save's pre-checkpoint
// scrub (2.7, #83 B1): it touches the guest directly, so it removes
// secret files a backend restart no longer remembers. The save aborts
// when the listing after =LEFT= is not empty. A missing directory is not
// an error: a lease that never had secrets must save (R1). The wildcards
// cover dotfiles and dotdot-prefixed names (`..x`). The listing before
// =LEFT= lets the backend report how many files it removed that it did
// not know about (R4).
const secretsScrubScript = `[ -d /run/secrets ] || exit 0
ls -A /run/secrets 2>/dev/null
rm -f /run/secrets/* /run/secrets/.[!.]* /run/secrets/..?* 2>/dev/null
echo =LEFT=
ls -A /run/secrets 2>/dev/null`

// scrubSeparator splits the scrub script's stdout: the removed entries
// before it, the entries left after it.
const scrubSeparator = "=LEFT="

// parseScrubOutput splits a scrub script's stdout into the entries it
// saw before the removal and those left afterwards. A missing marker
// (and any output before it) is treated as a removed list.
func parseScrubOutput(out string) (removed, left []string) {
	lines := strings.Split(out, "\n")
	i := 0
	for ; i < len(lines); i++ {
		if lines[i] == scrubSeparator {
			i++
			break
		}
		if lines[i] != "" {
			removed = append(removed, lines[i])
		}
	}
	for ; i < len(lines); i++ {
		if lines[i] != "" {
			left = append(left, lines[i])
		}
	}
	return removed, left
}

// validateSecrets checks one request's secrets against the name,
// count and total-size limits. It returns a copy (nil when the input
// is empty). Errors name the offending secret name or the sizes —
// never a value.
func validateSecrets(secrets map[string]string) (map[string]string, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if len(secrets) > maxSecretsPerRequest {
		return nil, fmt.Errorf("too many secrets: %d (max %d)", len(secrets), maxSecretsPerRequest)
	}
	out := make(map[string]string, len(secrets))
	total := 0
	for name, value := range secrets {
		if !secretNameRe.MatchString(name) {
			return nil, fmt.Errorf("invalid secret name %q: must match [A-Za-z0-9_.-]{1,64}", name)
		}
		total += len(value)
		out[name] = value
	}
	if total > maxSecretsTotalBytes {
		return nil, fmt.Errorf("secrets total %d bytes exceeds the %d byte limit", total, maxSecretsTotalBytes)
	}
	return out, nil
}

// secretPath is a secret's guest path.
func secretPath(name string) string { return secretsDir + "/" + name }

// sortedSecretNames returns the names in a stable order so staging and
// cleanup are deterministic.
func sortedSecretNames(secrets map[string]string) []string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// stageSecrets mounts the secrets tmpfs (idempotent) and writes every
// secret as /run/secrets/<name>, mode 0600, via substrate.WriteFile.
// Errors carry secret names (they are file names anyway), never values.
func (s *Service) stageSecrets(ctx context.Context, sandboxID string, secrets map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, secretsStageTimeout)
	defer cancel()
	if err := s.mountSecretsDir(ctx, sandboxID); err != nil {
		return err
	}
	for _, name := range sortedSecretNames(secrets) {
		if err := s.sub.WriteFile(ctx, sandboxID, secretPath(name), []byte(secrets[name]), secretsMode); err != nil {
			return fmt.Errorf("write secret %s: %w", name, err)
		}
	}
	return nil
}

// mountSecretsDir makes sure /run/secrets is a 0700 tmpfs owned by the
// exec user, mounting it when it is not.
func (s *Service) mountSecretsDir(ctx context.Context, sandboxID string) error {
	res, err := s.sub.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args:    []string{"/bin/bash", "-c", secretMountScript},
		Timeout: secretsStageTimeout,
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", secretsDir, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("mount %s: exit %d: %s", secretsDir, res.ExitCode, tailStr(res.Stderr, 200))
	}
	return nil
}

// removeSecrets deletes the named secret files. Best effort: the command
// they were staged for has already finished, so failures are logged
// (names only) and never fail it. It runs on a detached context so the
// files do not linger when the caller disconnects mid-exec.
func (s *Service) removeSecrets(sandboxID string, names []string) {
	ctx, cancel := context.WithTimeout(context.Background(), secretsStageTimeout)
	defer cancel()
	for _, name := range names {
		if err := s.sub.Remove(ctx, sandboxID, secretPath(name), false); err != nil {
			s.log.Printf("exec: remove secret %s: %v", name, err)
		}
	}
}

// setCreateSecrets records a lease's create-time secrets. Memory only:
// they never reach the store, so a backend restart loses them and the
// caller re-sends them on its next exec.
func (s *Service) setCreateSecrets(leaseID string, secrets map[string]string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if s.createSecrets == nil {
		s.createSecrets = map[string]map[string]string{}
	}
	s.createSecrets[leaseID] = secrets
}

// createSecretsFor returns a copy of a lease's create-time secrets
// (nil when it has none).
func (s *Service) createSecretsFor(leaseID string) map[string]string {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if len(s.createSecrets[leaseID]) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.createSecrets[leaseID]))
	for name, value := range s.createSecrets[leaseID] {
		out[name] = value
	}
	return out
}

// clearCreateSecrets drops a lease's create-time secrets.
func (s *Service) clearCreateSecrets(leaseID string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	delete(s.createSecrets, leaseID)
}

// scrubAllSecrets removes everything under /run/secrets through a
// guest exec and verifies the directory is empty afterwards. Unlike the
// in-memory name lists, it does not rely on what this backend process
// remembers, so a secret staged before a backend restart is still
// removed (2.7, #83 B1). A non-empty listing (or a failed exec) is an
// error: the caller must abort the save rather than checkpoint a
// captured secret. It returns how many removed entries were not in
// known, so the caller can warn about secrets this backend lost track of
// (R4).
func (s *Service) scrubAllSecrets(ctx context.Context, sandboxID string, known map[string]bool) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, secretsStageTimeout)
	defer cancel()
	res, err := s.sub.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args:    []string{"/bin/bash", "-c", secretsScrubScript},
		Timeout: secretsStageTimeout,
	})
	if err != nil {
		return 0, fmt.Errorf("scrub %s: %w", secretsDir, err)
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("scrub %s: exit %d: %s", secretsDir, res.ExitCode, tailStr(res.Stderr, 200))
	}
	removed, left := parseScrubOutput(res.Stdout)
	if len(left) > 0 {
		// Names only: the listing is file names, never values.
		return 0, fmt.Errorf("scrub %s: %s remains", secretsDir, strings.Join(left, " "))
	}
	unknown := 0
	for _, name := range removed {
		if !known[name] {
			unknown++
		}
	}
	return unknown, nil
}

// secretsGate serialises a named-snapshot save's secret scrub against
// exec and background-job secret staging on the same lease (2.7, #83
// B2). A save holds it from its secrets check through the checkpoint
// and the create-time re-stage; while a save holds it an exec or job
// that wants to stage answers 409 lease_busy. While a staging runs, a
// save refuses (secrets_in_use). It is memory-only, like the secrets
// themselves.
type secretsGate struct {
	mu      sync.Mutex
	saving  map[string]int
	staging map[string]int
}

// beginSave claims the lease for a save. It returns false when a save
// or a secret staging already holds the lease.
func (g *secretsGate) beginSave(leaseID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.saving == nil {
		g.saving = map[string]int{}
		g.staging = map[string]int{}
	}
	if g.saving[leaseID] > 0 || g.staging[leaseID] > 0 {
		return false
	}
	g.saving[leaseID]++
	return true
}

func (g *secretsGate) endSave(leaseID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.saving[leaseID] <= 1 {
		delete(g.saving, leaseID)
	} else {
		g.saving[leaseID]--
	}
}

// beginStaging records an exec or job about to stage secrets. It returns
// false while a save holds the lease.
func (g *secretsGate) beginStaging(leaseID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.saving == nil {
		g.saving = map[string]int{}
		g.staging = map[string]int{}
	}
	if g.saving[leaseID] > 0 {
		return false
	}
	g.staging[leaseID]++
	return true
}

func (g *secretsGate) endStaging(leaseID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.staging[leaseID] <= 1 {
		delete(g.staging, leaseID)
	} else {
		g.staging[leaseID]--
	}
}

// markExecSecretsStaged records that a synchronous exec has exec-time
// secrets staged on the lease right now (2.7, #83). A named snapshot
// save refuses while the count is positive, and scrubs the names even if
// a save raced the check. unmarkExecSecretsStaged drops one such
// staging.
func (s *Service) markExecSecretsStaged(leaseID string, names []string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if s.stagedExecSecrets == nil {
		s.stagedExecSecrets = map[string]int{}
	}
	s.stagedExecSecrets[leaseID]++
	if s.stagedExecSecretNames == nil {
		s.stagedExecSecretNames = map[string][]string{}
	}
	s.stagedExecSecretNames[leaseID] = append(s.stagedExecSecretNames[leaseID], names...)
}

func (s *Service) unmarkExecSecretsStaged(leaseID string, names []string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if s.stagedExecSecrets[leaseID] <= 1 {
		delete(s.stagedExecSecrets, leaseID)
		delete(s.stagedExecSecretNames, leaseID)
	} else {
		s.stagedExecSecrets[leaseID]--
		// Drop one staging's names (the first occurrence of each).
		left := s.stagedExecSecretNames[leaseID]
		for _, n := range names {
			for i, got := range left {
				if got == n {
					left = append(left[:i], left[i+1:]...)
					break
				}
			}
		}
		s.stagedExecSecretNames[leaseID] = left
	}
}

// hasStagedExecSecrets reports whether a synchronous exec holds
// exec-time secrets on the lease right now.
func (s *Service) hasStagedExecSecrets(leaseID string) bool {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	return s.stagedExecSecrets[leaseID] > 0
}

// hasRunningJobWithSecrets reports whether a lease has a live background
// job that staged exec-time secrets (2.7, #83): such a job's files are
// under /run/secrets while it runs, so a save would capture them.
func (s *Service) hasRunningJobWithSecrets(leaseID string) bool {
	s.secretsMu.Lock()
	live := map[string]bool{}
	for jobID := range s.liveJobSecrets {
		live[jobID] = true
	}
	s.secretsMu.Unlock()
	if len(live) == 0 {
		return false
	}
	// The in-memory running count is the fast path; the rows decide
	// which of those jobs belong to this lease and still run.
	if !s.hasRunningJob(leaseID) {
		return false
	}
	rows, err := s.db.ListJobs(context.Background(), leaseID)
	if err != nil {
		// Unknowable is not "safe to capture": a live job with secrets
		// would be captured. Refuse.
		s.log.Printf("snapshot: list jobs of %s: %v", leaseID, err)
		return true
	}
	for _, r := range rows {
		if r.State == "running" && live[r.JobID] {
			return true
		}
	}
	return false
}

// restageCreateSecrets re-writes a lease's create-time secrets after
// the sandbox came back (resume, restart, crash recovery): a fresh
// sandbox never had the tmpfs, and a rewrite after a snapshot resume is
// harmless.
// Failure is logged and otherwise ignored — the lease still runs, and
// the caller can re-send secrets on its next exec.
func (s *Service) restageCreateSecrets(ctx context.Context, l *Lease, what string) {
	secrets := s.createSecretsFor(l.ID)
	if len(secrets) == 0 {
		return
	}
	if err := s.stageSecrets(ctx, l.SandboxID, secrets); err != nil {
		s.log.Printf("%s %s: re-stage lease secrets: %v", what, l.ID, err)
	}
}

// restageSecrets re-writes the named create-time secrets after an
// exec-time secret with the same name was removed: the shadowed file
// keeps the lease's value like every other create-time secret (#80).
// Best effort, like every post-command cleanup.
func (s *Service) restageSecrets(sandboxID string, names []string, create map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), secretsStageTimeout)
	defer cancel()
	for _, name := range names {
		value, ok := create[name]
		if !ok {
			continue
		}
		if err := s.sub.WriteFile(ctx, sandboxID, secretPath(name), []byte(value), secretsMode); err != nil {
			s.log.Printf("exec: re-stage secret %s: %v", name, err)
		}
	}
}

// knownSecretNames is the set of secret file names this backend knows
// on a lease: its create-time names plus any exec-time or job names
// currently staged. A scrub that removes a file outside this set is one
// this backend lost track of (R4); the union keeps a still-known
// exec/job file from being miscounted as forgotten (Q3).
func (s *Service) knownSecretNames(leaseID string, create map[string]string) map[string]bool {
	known := make(map[string]bool, len(create))
	for name := range create {
		known[name] = true
	}
	s.secretsMu.Lock()
	for _, name := range s.stagedExecSecretNames[leaseID] {
		known[name] = true
	}
	for _, names := range s.liveJobSecrets {
		for _, name := range names {
			known[name] = true
		}
	}
	s.secretsMu.Unlock()
	return known
}

// deferSecretRemoval records exec-time secret names a finishing job
// could not remove because a named-snapshot save held the lease's
// secrets gate. The save drains them (drainDeferredSecretRemovals)
// before its create-time re-stage, so the final source state is exactly
// the create-time secret set and no exec-time file remains (Q1).
func (s *Service) deferSecretRemoval(leaseID string, names []string) {
	if len(names) == 0 {
		return
	}
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	s.pendingSecretRemovals[leaseID] = append(s.pendingSecretRemovals[leaseID], names...)
}

// drainDeferredSecretRemovals removes and returns the exec-time secret
// names a finishing job deferred while a save held the gate (Q1).
func (s *Service) drainDeferredSecretRemovals(leaseID string) []string {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	names := s.pendingSecretRemovals[leaseID]
	delete(s.pendingSecretRemovals, leaseID)
	return names
}

// clearPendingSecretRemovals drops a lease's deferred secret-removal
// list. A released lease's sandbox and its secret files are gone, so the
// list can never be drained and would otherwise leak one entry per
// released lease (spoond-966 L1).
func (s *Service) clearPendingSecretRemovals(leaseID string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	delete(s.pendingSecretRemovals, leaseID)
}
