// Release reasons for the CI runner (2.5, #132 part 2): when a job's
// lease is released, its `released` event says how the job ended and how
// long it ran, e.g. "ci job 3609 ✓ 11m02s", so the dashboard's events
// panel shows an operator why a lease went.

package runner

import (
	"fmt"
	"strconv"
	"time"
)

// releaseReason renders the reason the runner sends when it releases a
// job's lease: the job id, a glyph for the outcome (✓ success, ✗ failure,
// ↷ cancelled, - skipped) and the run duration. A cancelled job names
// itself distinctly so a replay of a shutdown reads at a glance.
func releaseReason(job *Job, result Result, d time.Duration) string {
	if result == ResultCancelled {
		return fmt.Sprintf("ci job %d cancelled", job.ID)
	}
	if result == ResultSkipped {
		return fmt.Sprintf("ci job %d skipped %s", job.ID, humanDuration(d))
	}
	glyph := "✓"
	if result == ResultFailure {
		glyph = "✗"
	}
	return fmt.Sprintf("ci job %d %s %s", job.ID, glyph, humanDuration(d))
}

// humanDuration renders a job duration compactly: HhMMmSSs past an hour,
// MmSSs past a minute, else Ss with a millisecond fraction for the short
// runs. It is the runner's own form, not Go's time.Duration string.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	switch {
	case total >= 3600:
		return strconv.FormatInt(total/3600, 10) + "h" +
			pad2(total%3600/60) + "m" + pad2(total%60) + "s"
	case total >= 60:
		return strconv.FormatInt(total/60, 10) + "m" + pad2(total%60) + "s"
	default:
		return strconv.FormatInt(total, 10) + "s"
	}
}

// pad2 renders n (0..59) as two digits.
func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}
