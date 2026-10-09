// Lease journal (spoond-puqp): one line per lease create, release, lost
// and suspend (including preemption), so a lease-to-sandbox mapping and
// the reason a lease went can be reconstructed from journalctl alone.
//
// Incident 2026-10-08: the runner deleted CI leases, the dashboard's
// 50-event ring rolled over and the lease rows were gone, so neither
// which sandbox a lease held nor why it was released was recoverable
// afterwards. The journal line is the durable record: it names the lease
// id, the owner id (never a token), the sandbox id, the image and the
// admission class, plus a reason. It carries no secret material.
//
// The line is logfmt-shaped so an operator can grep it:
//
//	lease journal: op=<op> lease_id=<id> owner=<owner> sandbox=<sandbox> image=<image> class=<class> reason=<reason>
//
// A value with a space, quote, equals sign or control character is
// quoted with Go's %q so the fields stay parseable; the reason is the
// only field that normally needs it.

package api

import (
	"strconv"
	"strings"
)

// Lease journal operations, one per line.
const (
	journalOpCreate  = "create"
	journalOpRelease = "release"
	journalOpLost    = "lost"
	journalOpSuspend = "suspend"
)

// journalLease writes one lease journal line. It is called from the
// lifecycle paths after their state change has settled, so the line
// names the lease as it stood: the sandbox id is present even when the
// release or loss is about to delete the sandbox. A nil lease writes
// nothing.
func (s *Service) journalLease(op string, l *Lease, reason string) {
	if s == nil || l == nil {
		return
	}
	s.log.Printf("lease journal: op=%s lease_id=%s owner=%s sandbox=%s image=%s class=%s reason=%s",
		journalValue(op), journalValue(l.ID), journalValue(l.Owner),
		journalValue(l.SandboxID), journalValue(l.Image), journalValue(leaseClassRow(l)),
		journalValue(reason))
}

// journalValue renders one journal field: bare when it is a simple
// token, quoted otherwise so a value containing a space, quote, equals
// sign or control character cannot be mistaken for a new field.
func journalValue(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " \t\n\r\"=") {
		return strconv.Quote(v)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return strconv.Quote(v)
		}
	}
	return v
}

// journalCreateReason names how a lease came to exist. The plain create,
// a named-snapshot start, a clone and a fork each read distinctly, so a
// journal reader can tell a CI create from a branched one without the
// lease row.
func journalCreateReason(snapshot, clonedFrom, forkedFrom string) string {
	switch {
	case snapshot != "":
		return "snapshot " + snapshot
	case clonedFrom != "":
		return "clone of " + clonedFrom
	case forkedFrom != "":
		return "fork of " + forkedFrom
	default:
		return "new"
	}
}

// journalReleaseReason maps the human release reason to the journal's
// canonical one. The event keeps its free text; the journal prefers a
// short, stable token so a release can be counted and grepped, except
// for a caller-supplied reason (a CI job names itself), which is kept
// verbatim because it is the most useful thing on the line.
func journalReleaseReason(l *Lease, reason string) string {
	switch reason {
	case "deleted through the API":
		// An owner (or an impersonated user) deleted the lease; name the
		// owner, not the token the caller authenticated with.
		return "deleted via API by " + l.Owner
	case "TTL expired":
		return "ttl"
	case lostReleaseReason:
		return "lost grace expired"
	case userDeleteReason:
		return "user deleted"
	case "released by a held-lease rule":
		return journalHeldReleaseReason(l)
	default:
		return reason
	}
}

// journalHeldReleaseReason names which held-lease rule released the
// lease, read from the last_action the rule stamped just before the
// release: critical disk pressure is "disk" and the stale rule (a
// suspension that stayed untouched) is "idle". Anything else is the
// generic held release.
func journalHeldReleaseReason(l *Lease) string {
	switch {
	case strings.HasPrefix(l.LastAction, heldRuleCritical+"/"):
		return "disk"
	case strings.HasPrefix(l.LastAction, heldRuleStale+"/"):
		return "idle"
	case strings.HasPrefix(l.LastAction, heldRuleExpiry+"/"):
		return "hold_lapsed"
	default:
		return "held rule"
	}
}

// journalSuspendReason names why a lease was suspended: the structured
// automatic reason when there is one (idle, idle_suspend, hold_lapsed,
// pressure, preempt), else "drain" for the admin drain and "hand" for an
// owner's own suspend.
func journalSuspendReason(pol suspendPolicy, drained bool) string {
	if pol.reason != "" {
		return pol.reason
	}
	if drained {
		return "drain"
	}
	return "hand"
}
