# DONE spoond-58e6: i/o stall meter aligned with the host meters

## What changed

The dashboard's i/o stall meter (PSI `io full` avg60) has warn/bad
levels of 5/15 by default, far below 100, so on a 0-100 % bar its
warning tick sat at the far left while every other host meter's tick
landed on cell 12. `cmd/spoond-dash/render.go` now gives a host meter an
optional display scale (`hostRow.scaled`, `meterSegs(..., scaled)`):

- `meterFill` maps 0..warn to 0..80 % of the bar, warn..bad to
  80..100 %, and danger or more to the full bar.
- The `╎` tick sits at `int(0.80*meterBarW)` = cell 12, the same cell
  the 75/80 % warn levels of the other meters land on.
- Colour is still chosen from the real `pct` against warn/bad, so
  `TestIOMeterStyle` is unchanged.
- The right-hand text keeps the true value.

Only the i/o stall meter is scaled. cpu, `<dev> busy`, memory,
hugepages, snapshot disk and root disk still use the plain pct/100 bar.

## Tick alignment of the other host meters

On the sample frame every host meter's `╎` lands on cell 12:

| meter | warn | tick cell |
|---|---|---|
| cpu | 75 | 12 |
| i/o stall | 5 (scaled) | 12 |
| `nvme0n1 busy` | 80 | 12 |
| memory | 80 | 12 |
| hugepages | 80 | 12 |
| snapshot disk | 80 | 12 |
| root disk | 75 | 12 |

None needed a change. `TestIOStallScale` asserts the i/o stall tick
equals the memory meter's and that every meter above shares that cell.

## Wording

The PSI term "full" is gone from the viewer-facing text, the number
unchanged:

- meter value: `0.4% stalled` (was `0.4% full`),
- notification: `disk i/o stalled 15% of the last 60 s` (was
  `disk i/o stalled: full pressure 15% over 60 s`),
- label stays `i/o stall`; the `DASH_IO_FULL_WARN_PCT` /
  `DASH_IO_FULL_BAD_PCT` variable names are unchanged.

## Tests

- `TestIOStallScale` — the i/o stall tick is at the memory meter's
  column (and every other host meter's); a value at warn fills exactly
  up to the tick; a value at bad fills the bar; 0.4 % still shows one
  filled cell; the value text reads `0.4% stalled`; the env threshold
  names still parse.
- `TestIOHostRowsDrawn` asserts the new `0.0% stalled` text.
- `TestIOPressureNotice` asserts the new `disk i/o stalled 15% of the
  last 60 s` text.
- `TestIOMeterStyle` still passes unchanged (ok/warn/bad per real
  value).
- Golden frames `testdata/grid-104.txt` and `testdata/grid-72.txt`
  updated for the new bar and text.
- `docs/operations.md` dashboard section and the comment example at
  `render.go` updated; CHANGELOG `[Unreleased]` gains a Changed entry.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 ./cmd/spoond-dash/...` — pass
- `go test -p 2 ./...` — pass
- The new and existing meter tests also pass as the non-root `user`
  with `HOME`/`GOCACHE`/`GOTMPDIR` under temp dirs; they use
  `t.TempDir()` and read no `/work`, `/run/honey` or `/opt/honey` path.

## origin/main

`git fetch origin` showed nothing new on `origin/main` since the branch
base (`69db1ac`, the merge of `work/spoond-hfko`), so there is no new
step type, provider, restart, cancel or retry path to reconcile.
