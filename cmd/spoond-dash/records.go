// The records panel (docs/plans/2026-10-13-records.md): all-time stats
// over the store's lease_history table plus the live leases. Spoond
// deletes a lease's row when it ends, so the history is the durable
// record; a live lease counts too, its end being now. Everything is
// computed from one read of the two tables and cached for a minute —
// the 2 s refresh reuses it.

package spoonddash

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultRecordsExclude is DASH_RECORDS_EXCLUDE when the environment
// does not set it: the test owners the conformance suite and the load
// generator run under, which would otherwise own every record.
const DefaultRecordsExclude = "conformance,test-consumer"

// recordsTTL is how long one computed records result is reused: the
// panel is all-time numbers, so recomputing it at most once a minute
// keeps the 2 s refresh cheap.
const recordsTTL = time.Minute

// recordsExcludeNames reads the excluded owner names
// (DASH_RECORDS_EXCLUDE, a comma list of names): unset means the
// default test owners; set but empty means count every owner.
func recordsExcludeNames() []string {
	v, ok := os.LookupEnv("DASH_RECORDS_EXCLUDE")
	if !ok {
		v = DefaultRecordsExclude
	}
	var out []string
	for _, name := range strings.Split(v, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// recLease is one lease for the records: a history row, or a live one.
// A live lease's Ended is the frame's now (its end is now).
type recLease struct {
	Owner, Image   string
	MemoryMB       int
	Created, Ended time.Time
	Lost           bool
	PeakMiB        int64 // 0 = never measured
	StartMs        int64 // 0 = unknown
	MaxNapSecs     int64 // 0 = none/unknown
}

// recPoint is one start or end event in the "at once" sweep: at the
// time, +1 to add a running lease, −1 to remove it. Ends sort before
// starts at equal times, so a lease released as another starts never
// counts both for that instant.
type recPoint struct {
	at    time.Time
	delta int
}

// records is one computed records panel: the twelve records and the
// footer's first-created date. A zero time or empty string the panel
// does not draw means the record has no value (the panel draws "—").
type records struct {
	Avail bool // a lease_history table existed (false: "no history yet")

	BusiestN  int
	BusiestAt time.Time
	// OneImage: the most leases of one image at once.
	OneImageImg string
	OneImageN   int
	OneImageAt  time.Time
	// Longest / shortest run: the duration, the image, when the lease
	// started (the drawn date) and when it ended.
	LongestDur      time.Duration
	LongestImg      string
	LongestCreated  time.Time
	LongestEnd      time.Time
	ShortestDur     time.Duration
	ShortestImg     string
	ShortestCreated time.Time
	ShortestEnd     time.Time
	// TopOwner: the owner with the most leases.
	TopOwner  string
	TopOwnerN int
	// Lifetime totals.
	LifetimeN    int
	LifetimeHrs  float64
	LifetimeGiBH float64
	// BusiestDay: the most leases created on one Pacific calendar day.
	BusyDayN int
	BusyDay  time.Time
	// BiggestFleet: the most leases one owner had running at once.
	FleetOwner string
	FleetN     int
	FleetAt    time.Time
	// MostMemory: the largest peak.
	MostMemMiB int64
	MostMemImg string
	MostMemEnd time.Time
	// NoLosses: whole days since the newest lost row, and that row.
	NoLossDays int
	LastLoss   time.Time
	// LongestNap: the largest max_nap_secs.
	LongestNap    time.Duration
	LongestNapImg string
	// FastestStart: the smallest start_ms > 0.
	FastestMs  int64
	FastestImg string
	FastestEnd time.Time
	// FirstCreated is the footer's "since <date>".
	FirstCreated time.Time
}

// computeRecords works the twelve records over one lease list. Tie
// order everywhere is the list's order (the caller reads the history
// oldest first), so the results are stable: a tie keeps the earlier
// lease's value.
func computeRecords(leases []recLease, now time.Time) records {
	var r records
	r.Avail = true

	// The "at once" sweeps: one start (+1) and end (−1) point per
	// lease, overall and grouped by image and by owner.
	all := make([]recPoint, 0, 2*len(leases))
	byImage := map[string][]recPoint{}
	byOwner := map[string][]recPoint{}
	for _, l := range leases {
		end := l.Ended
		if end.Before(l.Created) {
			end = now
		}
		all = append(all, recPoint{at: l.Created, delta: 1}, recPoint{at: end, delta: -1})
		byImage[l.Image] = append(byImage[l.Image], recPoint{at: l.Created, delta: 1}, recPoint{at: end, delta: -1})
		byOwner[l.Owner] = append(byOwner[l.Owner], recPoint{at: l.Created, delta: 1}, recPoint{at: end, delta: -1})
	}
	r.BusiestN, r.BusiestAt = sweepOnce(sortPoints(all))
	r.OneImageImg, r.OneImageN, r.OneImageAt = sweepBest(byImage)
	r.FleetOwner, r.FleetN, r.FleetAt = sweepBest(byOwner)

	// Per-lease records and the totals.
	dayCount := map[string]int{}
	ownerCount := map[string]int{}
	for _, l := range leases {
		dur := l.Ended.Sub(l.Created)
		if dur > r.LongestDur {
			r.LongestDur, r.LongestImg = dur, l.Image
			r.LongestCreated, r.LongestEnd = l.Created, l.Ended
		}
		if !l.Lost && dur >= time.Second && (r.ShortestImg == "" || dur < r.ShortestDur) {
			r.ShortestDur, r.ShortestImg = dur, l.Image
			r.ShortestCreated, r.ShortestEnd = l.Created, l.Ended
		}
		if l.PeakMiB > r.MostMemMiB {
			r.MostMemMiB, r.MostMemImg, r.MostMemEnd = l.PeakMiB, l.Image, l.Ended
		}
		if nap := time.Duration(l.MaxNapSecs) * time.Second; nap > r.LongestNap {
			r.LongestNap, r.LongestNapImg = nap, l.Image
		}
		if l.StartMs > 0 && (r.FastestMs == 0 || l.StartMs < r.FastestMs) {
			r.FastestMs, r.FastestImg, r.FastestEnd = l.StartMs, l.Image, l.Ended
		}
		if r.FirstCreated.IsZero() || l.Created.Before(r.FirstCreated) {
			r.FirstCreated = l.Created
		}
		r.LifetimeN++
		hrs := max(dur.Hours(), 0) // a clock set back must not subtract
		r.LifetimeHrs += hrs
		r.LifetimeGiBH += float64(l.MemoryMB) * hrs / 1024
		dayCount[l.Created.In(pacific).Format("2006-01-02")]++
		ownerCount[l.Owner]++
	}
	// Busiest day: the most created on one Pacific calendar day, ties
	// to the earlier day.
	for key, n := range dayCount {
		if day, err := time.ParseInLocation("2006-01-02", key, pacific); err == nil &&
			(n > r.BusyDayN || (n == r.BusyDayN && !day.IsZero() && (r.BusyDay.IsZero() || day.Before(r.BusyDay)))) {
			r.BusyDay, r.BusyDayN = day, n
		}
	}
	// Top owner: the most leases, ties to the earlier name.
	for owner, n := range ownerCount {
		if n > r.TopOwnerN || (n == r.TopOwnerN && (r.TopOwner == "" || owner < r.TopOwner)) {
			r.TopOwner, r.TopOwnerN = owner, n
		}
	}

	// No losses for: whole days since the newest lost row's end.
	for _, l := range leases {
		if l.Lost && l.Ended.After(r.LastLoss) {
			r.LastLoss = l.Ended
		}
	}
	if !r.LastLoss.IsZero() {
		r.NoLossDays = int(now.Sub(r.LastLoss).Hours() / 24)
	}
	return r
}

// sortPoints orders one sweep's points by time, ends before starts at
// equal times (−1 before +1), and returns them for chaining.
func sortPoints(points []recPoint) []recPoint {
	sort.Slice(points, func(i, j int) bool {
		if !points[i].at.Equal(points[j].at) {
			return points[i].at.Before(points[j].at)
		}
		return points[i].delta < points[j].delta
	})
	return points
}

// sweepOnce walks a sorted point list and returns the highest running
// count and the time it first reaches it.
func sweepOnce(points []recPoint) (int, time.Time) {
	cur, best := 0, 0
	var at time.Time
	for _, p := range points {
		cur += p.delta
		if cur > best {
			best, at = cur, p.at
		}
	}
	return best, at
}

// sweepBest sweeps every group of points and returns the group's key,
// its highest once-count and when that was first reached. Ties break to
// the earlier time, then the key, so the result is stable.
func sweepBest(groups map[string][]recPoint) (string, int, time.Time) {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	bestK := ""
	bestN, bestAt := 0, time.Time{}
	for _, k := range keys {
		n, at := sweepOnce(sortPoints(groups[k]))
		if n > bestN || (n == bestN && !at.IsZero() && (bestAt.IsZero() || at.Before(bestAt))) {
			bestK, bestN, bestAt = k, n, at
		}
	}
	return bestK, bestN, bestAt
}

// pacific is the timezone the records' dates and times draw in
// (America/Los_Angeles, the operator's own wall clock): a date means
// the operator's calendar day, not UTC's.
var pacific = mustPacific()

// mustPacific loads the records' display zone, falling back to the
// fixed −07:00 zone when the host has no tzdata (a scratch container):
// the panel must never refuse to draw over a missing zone file.
func mustPacific() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PST", -7*60*60)
}

// historyLeases reads the lease_history table into recLeases, oldest
// first. ok is false when the table does not exist yet (an older
// backend): "no history yet", not an error.
func historyLeases(ctx context.Context, db *sql.DB) (out []recLease, ok bool, err error) {
	rows, err := db.QueryContext(ctx, `SELECT owner, image, memory_mb, created_at, ended_at,
		outcome, peak_mib, start_ms, max_nap_secs FROM lease_history ORDER BY created_at`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var l recLease
		var created, ended, outcome string
		var memory, peak, startMs, nap int64
		if err := rows.Scan(&l.Owner, &l.Image, &memory, &created, &ended, &outcome, &peak, &startMs, &nap); err != nil {
			return nil, false, err
		}
		// RFC 3339 UTC, as the store writes it. A row whose clock could
		// not be parsed (a hand-mangled row) is skipped rather than
		// poisoning the whole panel.
		if l.Created, err = time.Parse(time.RFC3339Nano, created); err != nil {
			continue
		}
		if l.Ended, err = time.Parse(time.RFC3339Nano, ended); err != nil {
			continue
		}
		l.MemoryMB = int(memory)
		l.Lost = outcome == "lost"
		l.PeakMiB, l.StartMs, l.MaxNapSecs = peak, startMs, nap
		out = append(out, l)
	}
	return out, true, rows.Err()
}

// liveLeases reads the live leases table into recLeases, a lease's
// end being the frame's now. Only leases holding a sandbox count
// (running, suspended, recovered): a lost row is already at its end and
// a queued one has not started its run. Read like the image_uses query
// beside it: one lightweight read on the collector's read-only
// connection.
func liveLeases(ctx context.Context, db *sql.DB, names map[string]string, now time.Time) []recLease {
	rows, err := db.QueryContext(ctx, `SELECT image, owner, state, memory_mb, created_at
		FROM leases WHERE state IN ('running','suspended','recovered') ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []recLease
	for rows.Next() {
		var l recLease
		var owner, state, created string
		var memory int64
		if err := rows.Scan(&l.Image, &owner, &state, &memory, &created); err != nil {
			continue
		}
		if l.Created, err = time.Parse(time.RFC3339Nano, created); err != nil {
			continue
		}
		l.MemoryMB = int(memory)
		if l.Owner = names[owner]; l.Owner == "" {
			l.Owner = owner
		}
		l.Ended = now
		out = append(out, l)
	}
	return out
}

// excludeOwners drops the excluded owners (DASH_RECORDS_EXCLUDE) from
// a lease list. Excluded leases count nowhere in the panel.
func excludeOwners(leases []recLease) []recLease {
	exclude := make(map[string]bool)
	for _, name := range recordsExcludeNames() {
		exclude[name] = true
	}
	out := make([]recLease, 0, len(leases))
	for _, l := range leases {
		if !exclude[l.Owner] {
			out = append(out, l)
		}
	}
	return out
}

// collectRecords is the records collector's one computation: history
// plus live leases, excluded owners dropped, the records worked. ok is
// false when the catalog could not be read (the panel keeps its last
// value); avail is false when there is no lease_history table yet.
func (c *collector) collectRecords(ctx context.Context, now time.Time) (rec records, avail, ok bool) {
	db, err := sql.Open("sqlite", "file:"+c.cfg.DBPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return c.rec, c.recAvail, false
	}
	defer db.Close()
	hist, avail, err := historyLeases(ctx, db)
	if err != nil {
		return c.rec, c.recAvail, false
	}
	names := c.userNames()
	// Owner ids map to names; a name unknown to the identity store (or
	// a legacy consumer name) stands as it is.
	for i := range hist {
		if owner := names[hist[i].Owner]; owner != "" {
			hist[i].Owner = owner
		}
	}
	leases := excludeOwners(hist)
	leases = append(leases, excludeOwners(liveLeases(ctx, db, names, now))...)
	return computeRecords(leases, now), avail, true
}

// recordsFor returns the cached records, recomputing them when the
// minute since the last attempt is up. Between recomputes (and after a
// failed recompute) the panel draws the cached result.
func (c *collector) recordsFor(ctx context.Context, now time.Time) records {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	if c.recAt.IsZero() || now.Sub(c.recAt) >= recordsTTL {
		rec, avail, ok := c.collectRecords(ctx, now)
		if ok {
			c.rec, c.recAvail = rec, avail
		}
		c.recAt = now
	}
	return c.rec
}
