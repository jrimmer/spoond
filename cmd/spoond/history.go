//go:build !nohistory

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/store"
)

func init() {
	register(command{
		name: "history",
		desc: "lease-history tools (`spoond history import`)",
		run:  historyMain,
	})
}

func historyMain(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, "usage: spoond history import [flags] < journal.txt\n\n  import   read journalctl -o short-iso-precise `lease journal:` lines on\n           stdin and write one lease_history row per ended lease\n")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "import":
		return historyImport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "spoond history: unknown subcommand %q (want import)\n", args[0])
		return 2
	}
}

func historyImport(args []string) int {
	fs := flag.NewFlagSet("history import", flag.ExitOnError)
	dbPath := fs.String("db", envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db"), "SQLite database path")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "spoond history import: unexpected argument %q (input is stdin)\n", fs.Arg(0))
		return 2
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond history import: open %s: %v\n", *dbPath, err)
		return 1
	}
	defer db.Close()

	imported, skipped, err := importHistory(context.Background(), db, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond history import: %v\n", err)
		return 1
	}
	fmt.Printf("imported %d, skipped %d\n", imported, skipped)
	return 0
}

// envOr resolves an environment variable with a default. Named here
// (the other subcommands each carry their own copy) so the command's
// tests can drive it without a process environment.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// historyEntry is one lease the import reconstructs from a create line
// and its release or lost line.
type historyEntry struct {
	id       string
	endedAt  time.Time
	outcome  string // released|lost
	reason   string
	owner    string
	image    string
	memoryMB int
	peakMiB  int
}

// importHistory reads `journalctl -o short-iso-precise` lines on r and
// writes one lease_history row per lease that has a create line and an
// end line (op=release or op=lost): INSERT OR IGNORE, so a re-run — and
// a lost lease whose later release line would say released — keeps the
// first row. Unpaired lines (a create the journal rotated away, an end
// whose create is older than the journal, suspend lines, anything not a
// lease journal line) are skipped. It reports how many rows were
// inserted and how many input lines were skipped, and prints nothing.
//
// The timestamp is the line's first field (2026-10-09T16:31:02.123456Z
// — journalctl's short-iso-precise, always UTC); the parse tolerates a
// trailing timezone offset and a plain RFC 3339 stamp, so a line saved
// with a different -o still imports.
func importHistory(ctx context.Context, db *store.DB, r io.Reader) (imported, skipped int, err error) {
	creates := map[string]journalCreate{}
	var entries []historyEntry
	seenEnd := map[string]int{} // lease id → index into entries

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		stamp, body, ok := cutJournalStamp(line)
		if !ok {
			skipped++
			continue
		}
		fields := parseJournalBody(body)
		if fields == nil || fields["op"] == "" || fields["lease_id"] == "" {
			skipped++
			continue
		}
		id := fields["lease_id"]
		switch fields["op"] {
		case "create":
			creates[id] = journalCreate{at: stamp, owner: fields["owner"], image: fields["image"], memoryMB: atoiDefault(fields["memory_mib"], 0)}
		case "release", "lost":
			// A lost lease's grace can later log a release line for the
			// same id: the earlier end (the loss) is the one the backend
			// recorded, and the release's peak (the loss line has none)
			// still rides along.
			// The table's outcome is "released" or "lost"; the journal's
			// op is "release" or "lost".
			outcome := "lost"
			if fields["op"] == "release" {
				outcome = "released"
			}
			e := historyEntry{
				id:       id,
				endedAt:  stamp,
				outcome:  outcome,
				reason:   fields["reason"],
				owner:    fields["owner"],
				image:    fields["image"],
				memoryMB: atoiDefault(fields["memory_mib"], 0),
				peakMiB:  atoiDefault(fields["peak_mib"], 0),
			}
			if i, ok := seenEnd[id]; ok {
				prev := entries[i]
				if prev.endedAt.Before(e.endedAt) {
					e.endedAt, e.outcome, e.reason = prev.endedAt, prev.outcome, prev.reason
				}
				e.peakMiB = max(e.peakMiB, prev.peakMiB)
				entries[i] = e
				continue
			}
			seenEnd[id] = len(entries)
			entries = append(entries, e)
		default:
			// suspend lines and anything else the journal grows: skipped.
			skipped++
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("read stdin: %w", err)
	}

	for _, e := range entries {
		c, ok := creates[e.id]
		if !ok {
			// An end without a create in this window: unpairable.
			skipped++
			continue
		}
		inserted, err := db.InsertLeaseHistory(ctx, store.LeaseHistoryRow{
			LeaseID:   e.id,
			Owner:     e.owner,
			Image:     e.image,
			MemoryMB:  e.memoryMB,
			CreatedAt: c.at,
			EndedAt:   e.endedAt,
			Outcome:   e.outcome,
			Reason:    e.reason,
			PeakMiB:   e.peakMiB,
			// start_ms, start_kind and max_nap_secs stay 0/'': the
			// journal predates them (records from 3.8 count them).
		})
		if err != nil {
			return imported, skipped, fmt.Errorf("insert %s: %w", e.id, err)
		}
		if inserted {
			imported++
		}
		// A skip (false, nil) is a re-run finding its own row: counted as
		// neither imported nor skipped.
	}
	return imported, skipped, nil
}

// journalCreate is what a create line records: when, whose, what.
type journalCreate struct {
	at       time.Time
	owner    string
	image    string
	memoryMB int
}

// cutJournalStamp splits journalctl's short-iso-precise stamp (the
// line's first field) from the rest. ok is false when the line has no
// parseable stamp (a continuation line, a systemd notice).
func cutJournalStamp(line string) (time.Time, string, bool) {
	stamp, rest, ok := strings.Cut(strings.TrimLeft(line, " \t"), " ")
	if !ok {
		return time.Time{}, "", false
	}
	t, ok := parseJournalStamp(stamp)
	if !ok {
		return time.Time{}, "", false
	}
	return t, rest, true
}

// parseJournalStamp parses the stamp forms the import accepts:
// journalctl's short-iso-precise (2026-10-09T16:31:02.123456Z) and a
// plain RFC 3339 stamp with a numeric offset
// (2026-10-09T16:31:02+02:00), so a capture saved with another -o still
// imports.
func parseJournalStamp(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z0700",
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// parseJournalBody parses the logfmt fields of one `lease journal:`
// body: op=… lease_id=… owner=… sandbox=… image=… memory_mib=…
// [peak_mib=…] reason=…. The writer quotes a value with a space, quote,
// equals sign or control character with Go's %q; the reader accepts
// both spellings. nil when the body is not a lease journal line (no
// op=…).
func parseJournalBody(body string) map[string]string {
	body = strings.TrimPrefix(body, "lease journal: ")
	fields := map[string]string{}
	for _, tok := range splitJournalTokens(body) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		fields[k] = unquoteJournalValue(v)
	}
	if fields["op"] == "" || fields["lease_id"] == "" {
		return nil
	}
	return fields
}

// splitJournalTokens splits a journal body on unquoted spaces.
func splitJournalTokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, escaped := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// unquoteJournalValue resolves the writer's quoting: a Go-quoted token
// (the writer quotes values with spaces, quotes, equals signs or
// control characters) unquotes; a bare token stands as it is.
func unquoteJournalValue(v string) string {
	if unq, err := strconv.Unquote(v); err == nil {
		return unq
	}
	return v
}

// atoiDefault parses a whole-number journal field, def when absent or
// malformed.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
