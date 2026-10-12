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
//	lease journal: op=<op> lease_id=<id> owner=<owner> sandbox=<sandbox> image=<image> memory_mib=<n> [peak_mib=<n>] reason=<reason>
//
// The peak_mib field rides only the release line (#130 step 1), between
// memory_mib and reason: the measured peak the lease's VM held, when it
// was measured at all.
//
// A value with a space, quote, equals sign or control character is
// quoted with Go's %q so the fields stay parseable; the reason is the
// only field that normally needs it.

package api

import (
	"fmt"
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
	if s == nil || s.log == nil || l == nil {
		return
	}
	s.journalPrint(op, l, reason, "")
}

// journalLeaseRelease writes the release line, the one op that carries
// the measured-memory field (#130 step 1): peakMib is the lease's
// measured peak in whole MiB (ceil — the same rendering the lease row's
// memory_peak_mb uses), read before the sampler dropped the entry, and
// hasPeak gates the field, written between memory_mib and reason so the
// documented parse order (operations.md) holds. Without a measurement
// (the sampler off — no OTEL_PROM_URL — or a lease that never got a
// sample) hasPeak is false and the line reads exactly as before.
func (s *Service) journalLeaseRelease(l *Lease, reason string, peakMib int, hasPeak bool) {
	peakField := ""
	if hasPeak {
		peakField = fmt.Sprintf("peak_mib=%d ", peakMib)
	}
	s.journalPrint(journalOpRelease, l, reason, peakField)
}

// journalPrint prints one lease journal line. peakField is empty, or
// the `peak_mib=<n>` field with its trailing space, which is why it
// lands between memory_mib and reason.
func (s *Service) journalPrint(op string, l *Lease, reason, peakField string) {
	if s == nil || s.log == nil || l == nil {
		return
	}
	s.log.Printf("lease journal: op=%s lease_id=%s owner=%s sandbox=%s image=%s memory_mib=%d %sreason=%s",
		journalValue(op), journalValue(l.ID), journalValue(l.Owner),
		journalValue(l.SandboxID), journalValue(l.Image), l.MemoryMB,
		peakField, journalValue(reason))
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
// verbatim because it is the most useful thing on the line, and the
// owner-named API deletion.
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
	case "paused_expired":
		return "paused_expired"
	default:
		return reason
	}
}

// journalHeldReleaseReason and its held-rule tokens are removed with
// the held-lease rules (FS5).

// journalSuspendReason names why a lease was suspended: the structured
// automatic reason when there is one (idle_suspend, preempt or
// resume_failed), else "drain" for the admin drain and "hand" for an
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
