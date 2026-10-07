# DONE spoond-dl2: dashboard footer names the project, header centres SPOOND · host

## Changes

1. **Footer: one dim, centred line about the project.**
   `spoond · github.com/jrimmer/spoond · v2.7.0 (2026-10-07)` — name,
   URL, version and release date.
   - URL: `DASH_PROJECT_URL`, default the module's home
     (`github.com/jrimmer/spoond`), shown without its scheme and with
     the trailing slash trimmed. In the terminal it is plain dim text;
     in the page the same span is a real `<a>` (the page already draws
     holder links) pointing at the href with an `https` scheme.
   - Version: the dashboard binary's own module version
     (`debug.ReadBuildInfo` + the same `versionLabel` shortening the
     header used).
   - Release date: the build's `vcs.time` (the release commit's date) as
     `YYYY-MM-DD`; omitted when there is no `vcs.time` (a dev build).
   - Narrow frame: the URL drops first, then the release date; the name
     and version always stay.
   - Centred: `footer` calls `grid.Center` like the header title, so the
     line reads as one quiet aside across the full width.
   The old status-line readouts removed in round 1 stay removed; each is
   still visible elsewhere:
   - leases — the capacity panel's running meter (`3 / 64`) and leases
     line, and the leases panel's per-lease rows;
   - hugepages — the host panel's `hugepages` meter row
     (`23.9 GiB free`, warn/danger thresholds);
   - snapshot disk — the host panel's `snapshot disk` meter row
     (`121.5 GiB free`, warn/danger thresholds);
   - units — the services panel (one row per unit with ✓/✗ and state),
     including a "+N more" fold.
   `statusLine`, `statusItems`, `statusItem`, `statusW` and `clockW` are
   gone (nothing else used them).

2. **Header centre is `SPOOND · <host>`.** The version is not in the
   header any more (it lives in the footer, never shown twice). The right
   end of the header row keeps `up <dur>, <time>` (e.g.
   `up 35m, 12:41:07`); on a narrow frame the uptime drops before the
   clock, and neither overlaps the centred title.

3. **`DASH_SERVICES` default** in `cmd/spoond-dash/dash.go` gains
   `spoond-netwatch`; `docs/operations.md` (table row) and `README.md`
   mention it.

4. **Tests and docs.** Golden frames (`testdata/grid-104.txt`,
   `grid-72.txt`) updated. New tests: `TestHeaderUptimeAndClock` (title
   without the version, right-aligned uptime+clock, clock-only case,
   narrow-frame drop), `TestFooterProjectLine` (name/URL/version, no date
   in a test build, centred), `TestFooterProjectURL`,
   `TestFooterDropsNarrow`, `TestFooterNoDateDropsToName`,
   `TestReleaseDateFrom` (vcs.time parsing, omitted for dev) and
   `TestPageFooterProjectLink`. `docs/operations.md`, `README.md`,
   `cmd/spoond-dash/dash.go` and `CHANGELOG.md` [Unreleased] updated.
   The branch was rebased onto `origin/main` (1d075d7, the Notifications
   panel); the CHANGELOG and render.go conflicts keep both features.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — clean
- `go test -p 1 ./...` — all pass
