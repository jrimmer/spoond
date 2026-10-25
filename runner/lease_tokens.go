package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jrimmer/spoond/v4/settings"
)

// LeaseTokenMap is LEASE_TOKEN_MAP parsed: which lease token a job from
// a repository, or from any repository of an owner, leases with. Fair
// shares count every lease against the identity that created it, so
// with one LEASE_TOKEN every repository's CI is one owner — nearly
// always the one furthest over its slice, whose leases memory take-back
// pauses first. A mapped project's jobs lease as its own identity
// instead. A job whose repository matches nothing uses LEASE_TOKEN.
type LeaseTokenMap struct {
	// idents are the distinct tokens, in the order first configured.
	idents []LeaseIdentity
	// repos and owners map a lower-cased "owner/repo" or "owner" key to
	// its token's index in idents.
	repos  map[string]int
	owners map[string]int
}

// LeaseIdentity is one distinct token of a LeaseTokenMap and the keys
// (as configured) that lease with it.
type LeaseIdentity struct {
	Token string
	Keys  []string
}

// ParseLeaseTokenMap parses LEASE_TOKEN_MAP: comma-separated KEY=PATH
// entries, KEY an "owner/repo" (that repository) or an "owner" (every
// repository of it), PATH the absolute path of a file holding the token
// — never the token itself. Entries are trimmed and empty ones dropped;
// keys match case-insensitively. Each file is read by LEASE_TOKEN_FILE's
// rules (settings.ReadValueFile through readFile), and a file readable by
// group or others is a warning, as LEASE_TOKEN_FILE's is. A malformed
// entry, a key given twice, or an unreadable or empty file is an error
// naming the entry by its key; no error or warning carries a token. An
// empty value is an empty map: every job uses LEASE_TOKEN.
func ParseLeaseTokenMap(v string, readFile func(string) ([]byte, error)) (*LeaseTokenMap, []string, error) {
	m := &LeaseTokenMap{repos: map[string]int{}, owners: map[string]int{}}
	var warns []string
	byToken := map[string]int{}
	n := 0
	for _, entry := range strings.Split(v, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		n++
		key, path, ok := strings.Cut(entry, "=")
		if !ok {
			// The entry is not echoed: a token pasted in place of a
			// KEY=PATH pair must not reach the journal.
			return nil, nil, fmt.Errorf("LEASE_TOKEN_MAP: entry %d is not KEY=/path/to/token-file", n)
		}
		key, path = strings.TrimSpace(key), strings.TrimSpace(path)
		owner, repo, isRepo := strings.Cut(key, "/")
		if owner == "" || (isRepo && (repo == "" || strings.Contains(repo, "/"))) || strings.ContainsAny(key, " \t") {
			return nil, nil, fmt.Errorf("LEASE_TOKEN_MAP: entry %d: key %q is not owner/repo or owner", n, key)
		}
		if !filepath.IsAbs(path) {
			// Not echoed either: a relative "path" may well be a token.
			return nil, nil, fmt.Errorf("LEASE_TOKEN_MAP: %s: the value must be the absolute path of a token file, not the token", key)
		}
		lower := strings.ToLower(key)
		table := m.owners
		if isRepo {
			table = m.repos
		}
		if _, dup := table[lower]; dup {
			return nil, nil, fmt.Errorf("LEASE_TOKEN_MAP: %s is mapped twice", key)
		}
		token, err := settings.ReadValueFile(path, readFile)
		if err != nil {
			return nil, nil, fmt.Errorf("LEASE_TOKEN_MAP: %s: %w", key, err)
		}
		if w := settings.FileModeWarning(path); w != "" {
			warns = append(warns, "LEASE_TOKEN_MAP: "+key+": "+w)
		}
		// Two files holding one token are one identity: one client per
		// worker, one sweep.
		i, seen := byToken[token]
		if !seen {
			i = len(m.idents)
			byToken[token] = i
			m.idents = append(m.idents, LeaseIdentity{Token: token})
		}
		m.idents[i].Keys = append(m.idents[i].Keys, key)
		table[lower] = i
	}
	return m, warns, nil
}

// Keys returns every mapped key as configured, grouped by identity, for
// the startup log.
func (m *LeaseTokenMap) Keys() []string {
	if m == nil {
		return nil
	}
	var keys []string
	for _, id := range m.idents {
		keys = append(keys, id.Keys...)
	}
	return keys
}

// Identities returns the map's distinct tokens with the keys mapped to
// each.
func (m *LeaseTokenMap) Identities() []LeaseIdentity {
	if m == nil {
		return nil
	}
	return m.idents
}

// lookup returns the identity index for a repository ("owner/repo"):
// the repository's own entry first, then its owner's.
func (m *LeaseTokenMap) lookup(repository string) (int, bool) {
	if m == nil || len(m.idents) == 0 {
		return 0, false
	}
	repository = strings.ToLower(strings.TrimSpace(repository))
	if i, ok := m.repos[repository]; ok {
		return i, true
	}
	if owner, _, ok := strings.Cut(repository, "/"); ok {
		if i, ok := m.owners[owner]; ok {
			return i, true
		}
	}
	return 0, false
}

// TokenFor returns the token a job from repository leases with, and
// false when the map does not cover it (the job uses LEASE_TOKEN).
func (m *LeaseTokenMap) TokenFor(repository string) (string, bool) {
	i, ok := m.lookup(repository)
	if !ok {
		return "", false
	}
	return m.idents[i].Token, true
}

// Router returns an Executor.SandboxFor for the map: it builds one
// provider per distinct token with newProvider, at once, and picks a
// job's by the job's repository (the github.repository context, the one
// actions/checkout clones). A job the map does not cover gets nil, so
// the executor's own Sandbox (LEASE_TOKEN) serves it. Call it once per
// worker, like the worker's own lease client: a provider labels the
// next lease it creates, so no two workers may share one. nil for an
// empty map.
func (m *LeaseTokenMap) Router(newProvider func(token string) SandboxProvider) func(*Job) SandboxProvider {
	if m == nil || len(m.idents) == 0 {
		return nil
	}
	providers := make([]SandboxProvider, len(m.idents))
	for i, id := range m.idents {
		providers[i] = newProvider(id.Token)
	}
	return func(job *Job) SandboxProvider {
		i, ok := m.lookup(jobRepository(job))
		if !ok {
			return nil
		}
		return providers[i]
	}
}

// jobRepository is the job's "owner/repo": the github.repository
// context, which actions/checkout clones.
func jobRepository(job *Job) string {
	if job == nil {
		return ""
	}
	return job.Context["repository"]
}

// SweepTarget is one lease identity the startup sweep covers, with the
// name its errors carry (a setting name and keys, never the token).
type SweepTarget struct {
	Name    string
	Sweeper LeaseSweeper
}

// MultiSweeper sweeps several lease identities as one LeaseSweeper: the
// runner's leases live under LEASE_TOKEN and every LEASE_TOKEN_MAP
// token, and a previous process may have left orphans under any of them.
// Each identity's sweep is its own (it deletes only runner job leases,
// by their comment, so a project's other leases under the same token
// stay); they run concurrently, so a slow one does not use up the
// others' share of the caller's deadline. The count is the total, and a
// failed sweep does not stop the rest: the error joins each failure,
// named by its target.
type MultiSweeper []SweepTarget

// SweepOrphans implements LeaseSweeper.
func (ms MultiSweeper) SweepOrphans(ctx context.Context, keep func(id string) bool) (int, error) {
	counts := make([]int, len(ms))
	errs := make([]error, len(ms))
	var wg sync.WaitGroup
	for i, t := range ms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := t.Sweeper.SweepOrphans(ctx, keep)
			counts[i] = n
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", t.Name, err)
			}
		}()
	}
	wg.Wait()
	total := 0
	for _, n := range counts {
		total += n
	}
	return total, errors.Join(errs...)
}
