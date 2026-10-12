package api

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v3/identity"
)

// Deprecated-surface groundwork for 4.0 (#139): the /api/sandboxes path
// alias and keyless LLM access under LLM_OPEN_LEGACY are the two things
// the next major release removes. Removal is decided on data, so every
// request served through either gets the RFC 8594 deprecation headers
// (the same pair api/users.go sets for a deprecated request field), one
// `spoond_deprecated_requests_total` sample and — at most once an hour
// per caller — a log line naming the caller (never a token).

const (
	depKindsandboxesAlias = "sandboxes_alias"
	depKindLLMKeyless     = "llm_keyless"
)

// apiRewrittenKey marks, in the request context, that the path was
// rewritten from /api/leases onto the registered /api/sandboxes routes:
// the primary path family, so the alias deprecation must not fire on it.
type apiRewrittenKey struct{}

// isAPISandboxesPath reports whether p is on the /api/sandboxes lease
// API (a whole-segment match, so /api/sandboxesXY is not one).
func isAPISandboxesPath(p string) bool {
	return p == apiSandboxPathPrefix || strings.HasPrefix(p, apiSandboxPathPrefix+"/")
}

// deprecationNoticeWindow is how often the same caller may trip the log
// line again: one line per caller per hour keeps a polling client from
// eating the log.
const deprecationNoticeWindow = time.Hour

// deprecationLogger throttles the per-caller notice to one line per
// window. The key is the caller identity (user id or legacy consumer
// id) — never a token, and never the path: paths carry lease ids, so
// the map must stay bounded by callers, not requests. Each kind (the
// Server's alias gate, the LLM gateway's keyless gate) holds its own
// logger so the two notices do not share a window.
type deprecationLogger struct {
	mu     sync.Mutex
	last   map[string]time.Time // caller -> last notice
	window time.Duration
}

// pruneOver sweeps callers whose notice has fully aged out once the map
// grows past this many entries; callers arrive under valid tokens, so
// this mostly matters for long-lived deployments with churn.
const pruneOver = 4096

func newDeprecationLogger() *deprecationLogger {
	return &deprecationLogger{last: map[string]time.Time{}, window: deprecationNoticeWindow}
}

// allow reports whether a notice for this caller is due now, recording
// the stamp when it is.
func (d *deprecationLogger) allow(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.last[key]; ok && now.Sub(t) < d.window {
		return false
	}
	d.last[key] = now
	if len(d.last) > pruneOver {
		for k, t := range d.last {
			if now.Sub(t) >= d.window {
				delete(d.last, k)
			}
		}
	}
	return true
}

// setDeprecatedHeaders writes the RFC 8594 pair on the response: the
// Deprecation flag and a Link naming the successor route. The lease API
// answer itself is unchanged.
func setDeprecatedHeaders(w http.ResponseWriter, successorPath string) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `<`+successorPath+`>; rel="successor-version"`)
}

// aliasSuccessorPath maps a /api/sandboxes path onto its /api/leases
// twin for the successor Link (the inverse of rewriteLeasePath). The
// result is percent-escaped: it goes into a header before auth, and a
// raw `>` from the request path would close the <…> and let the caller
// add Link entries of their own.
func aliasSuccessorPath(p string) string {
	return (&url.URL{Path: apiLeasePathPrefix + strings.TrimPrefix(p, apiSandboxPathPrefix)}).EscapedPath()
}

// markDeprecatedAliasHeaders sets the RFC 8594 headers on a request
// still on the /api/sandboxes alias. It runs in Handler before auth, on
// the path the client asked for, so even a refused request learns the
// surface goes away. A request rewritten from /api/leases (or the
// events token's documented route) carries the apiRewrittenKey marker
// and is skipped, so the primary path never reports the alias.
func markDeprecatedAliasHeaders(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(apiRewrittenKey{}) == true {
		return
	}
	setDeprecatedHeaders(w, aliasSuccessorPath(r.URL.Path))
}

// callerLabel renders the logged caller identity: the identity user id
// when the caller authenticated as one, else the owner id (the legacy
// consumer id). Never the token.
func callerLabel(u *identity.User, owner string) string {
	if u != nil {
		return u.ID
	}
	return owner
}

// noteDeprecatedAlias counts and logs a request that reached the lease
// API on its /api/sandboxes spelling (the alias, not a /api/leases path
// rewritten onto it). Runs in authMiddleware after the caller is
// authenticated — the counter measures real usage, and the notice needs
// the caller identity; the headers have already gone on before auth.
func (s *Server) noteDeprecatedAlias(r *http.Request, u *identity.User, owner string) {
	s.metrics.DeprecatedReqs.WithLabelValues(depKindsandboxesAlias).Inc()
	if s.depLog.allow(owner, time.Now()) {
		s.svc.log.Printf("deprecated /api/sandboxes request (removed in 4.0); use %s: caller=%s path=%q", aliasSuccessorPath(r.URL.Path), callerLabel(u, owner), r.URL.Path)
	}
}

// noteDeprecatedKeylessLLM flags an /llm/ request served only because
// LLM_OPEN_LEGACY kept a keyless owner open: the counter sample and the
// throttled log line. The gateway carries no owner token, so the
// identity is the lease's owner id.
func (g *llmGateway) noteDeprecatedKeylessLLM(leaseID, owner string) {
	if g.metrics != nil {
		g.metrics.DeprecatedReqs.WithLabelValues(depKindLLMKeyless).Inc()
	}
	if g.depLog.allow(owner, time.Now()) {
		g.log.Printf("deprecated keyless LLM access under LLM_OPEN_LEGACY (removed in 4.0); the lease owner must configure an LLM key: caller=%s lease=%s", owner, leaseID)
	}
}
