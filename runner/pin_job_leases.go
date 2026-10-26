package runner

import (
	"fmt"
	"strings"
)

// PinJobLeases is PIN_JOB_LEASES parsed: which jobs' leases the runner
// creates pinned. A pinned lease is never paused by memory take-back
// for another owner's request (only the low-memory floor takes one, as
// its last resort), and a take-back that pauses a job lease mid-step
// cuts the step with 409 lease_busy, which the runner cannot retry.
// Keys match like LEASE_TOKEN_MAP's: a job's repository ("owner/repo")
// by its own entry first, then by its owner's; "*" pins every job. A
// nil or empty value pins nothing.
type PinJobLeases struct {
	all    bool
	keys   []string        // as configured, for the startup log
	repos  map[string]bool // lower-cased "owner/repo"
	owners map[string]bool // lower-cased "owner"
}

// ParsePinJobLeases parses PIN_JOB_LEASES: comma-separated "owner/repo"
// (that repository) or "owner" (every repository of it) keys, or a lone
// "*" (every job). Entries are trimmed and empty ones dropped; keys
// match case-insensitively. A malformed entry, a key given twice, or
// "*" beside other entries is an error naming the entry by position.
// An empty value pins nothing (nil, no error).
func ParsePinJobLeases(v string) (*PinJobLeases, error) {
	p := &PinJobLeases{repos: map[string]bool{}, owners: map[string]bool{}}
	n := 0
	for _, entry := range strings.Split(v, ",") {
		key := strings.TrimSpace(entry)
		if key == "" {
			continue
		}
		n++
		if key == "*" {
			if p.all {
				return nil, fmt.Errorf("PIN_JOB_LEASES: entry %d: * is listed twice", n)
			}
			if len(p.keys) > 0 {
				return nil, fmt.Errorf("PIN_JOB_LEASES: entry %d: * pins every job and cannot be combined with other entries", n)
			}
			p.all = true
			p.keys = append(p.keys, key)
			continue
		}
		if p.all {
			return nil, fmt.Errorf("PIN_JOB_LEASES: entry %d: * pins every job and cannot be combined with other entries", n)
		}
		owner, repo, isRepo := strings.Cut(key, "/")
		if owner == "" || (isRepo && (repo == "" || strings.Contains(repo, "/"))) || strings.ContainsAny(key, " \t=*") {
			return nil, fmt.Errorf("PIN_JOB_LEASES: entry %d: the key is not owner/repo, owner or *", n)
		}
		table := p.owners
		if isRepo {
			table = p.repos
		}
		lower := strings.ToLower(key)
		if table[lower] {
			return nil, fmt.Errorf("PIN_JOB_LEASES: entry %d: %s is listed twice", n, key)
		}
		table[lower] = true
		p.keys = append(p.keys, key)
	}
	if n == 0 {
		return nil, nil
	}
	return p, nil
}

// Keys returns every configured key as given, for the startup log.
func (p *PinJobLeases) Keys() []string {
	if p == nil {
		return nil
	}
	return p.keys
}

// Pins reports whether a job from repository ("owner/repo") leases
// pinned: "*", the repository's own entry, then its owner's.
func (p *PinJobLeases) Pins(repository string) bool {
	if p == nil {
		return false
	}
	if p.all {
		return true
	}
	repository = strings.ToLower(strings.TrimSpace(repository))
	if p.repos[repository] {
		return true
	}
	if owner, _, ok := strings.Cut(repository, "/"); ok {
		return p.owners[owner]
	}
	return false
}

// PinFor returns an Executor.PinLease for the set: true for a job whose
// repository (the github.repository context) it covers. nil for an
// empty set, so the executor pins nothing.
func (p *PinJobLeases) PinFor() func(*Job) bool {
	if p == nil {
		return nil
	}
	return func(job *Job) bool { return p.Pins(jobRepository(job)) }
}
