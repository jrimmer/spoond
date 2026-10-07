# DONE spoond-dl2: dashboard footer removed, header clock

## Changes

1. **Footer status line removed.** `assemble` no longer allocates or
   draws a last row; `statusLine`, `statusItems`, `statusItem`,
   `statusW` and `clockW` are gone (nothing else used them). Every
   readout it carried is still visible elsewhere:
   - leases — the capacity panel's running meter (`3 / 64`) and leases
     line, and the leases panel's per-lease rows;
   - hugepages — the host panel's `hugepages` meter row
     (`23.9 GiB free`, warn/danger thresholds);
   - snapshot disk — the host panel's `snapshot disk` meter row
     (`121.5 GiB free`, warn/danger thresholds);
   - units — the services panel (one row per unit with ✓/✗ and state),
     including a "+N more" fold.
   The footer's certificate entry was never drawn, so nothing was lost
   there.

2. **Header.** The centre keeps only `SPOOND · <host> · <version>`.
   The right end of the header row shows `up <dur>, <time>` (the same
   `dur`/clock format the footer used, e.g. `up 35m, 12:41:07`). The
   uptime is drawn only when it fits clear of the centred title; on a
   narrow frame it is dropped first, then the clock is dropped if even
   it would overlap. The title is never overlapped.

3. **`DASH_SERVICES` default** in `cmd/spoond-dash/dash.go` gains
   `spoond-netwatch`; `docs/operations.md` and `README.md` mention it
   and the new header clock.

4. **Tests and docs.** Golden frames (`testdata/grid-104.txt`,
   `grid-72.txt`) updated; a new `TestHeaderUptimeAndClock` covers the
   right-aligned uptime+clock, the clock-only case, and the narrow-frame
   drop. `docs/operations.md`, `README.md` and `CHANGELOG.md`
   [Unreleased] updated.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l ./...` — clean
- `go test -p 1 ./...` — all pass
