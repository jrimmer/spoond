package api

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
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
