# DONE spoond-orfv: `spoond config show` + GET /api/admin/config (#137)

Branch `work/spoond-orfv`. What each setting is, where it came from, and
whether the running backend has it — kept simple and read-only.

## Changes

1. **`GET /api/admin/config`** (`api/config.go`, route in
   `api/server.go`): every name in `settings.Known` read with
   `os.LookupEnv` in the backend process, answered as
   `{"started_at":"<RFC3339>","settings":[{"name","set":bool,"value"}]}`
   in `settings.Known`'s sorted order. `started_at` is the server's
   construction time (`Server.startedAt`), so a caller can tell whether
   the backend restarted since the env changed. An unset name is
   `set:false, value ""`. A name carrying `TOKEN`, `KEY`, `SECRET`,
   `PASSWORD`, `HASH` or `SEED` (case-insensitive) is masked to
   `(hidden)` when set — the value never crosses the wire. Auth is
   `adminOK` in `api/admin.go` like the other `/api/admin` routes
   (`ADMIN_TOKEN` or an admin user's token; no identity store + no
   configured token → 404). The masking rule lives in
   `settings.HideIfSecret` so the endpoint and the CLI share it.
2. **`spoond config show`** (`cmd/spoond-config`, registered as
   `spoond config` in `cmd/spoond/config.go` with a `noconfig` build
   tag; help list and doc comment updated in `cmd/spoond/main.go`):
   runs `systemctl show <unit> -p EnvironmentFiles -p Environment`,
   parses the `EnvironmentFiles=... (ignore_errors=no)` entries and the
   `Environment=K=V K2=V2` line, reads each file in order, and merges
   per systemd.exec(5) — `Environment=` applied first, every
   `EnvironmentFile=` in listed order after it, so a file's value
   overrides the unit line and a later file overrides an earlier one
   (the order is pinned by `TestUnitEnvMergeOrder`). It then GETs
   `<api>/api/admin/config` with the merged env's own `ADMIN_TOKEN`
   (default api: the merged env's `BIND_ADDR`, else
   `https://127.0.0.1:8890`; TLS on a loopback address skips
   verification, matching how the drain hook reaches the local
   backend). Prints NAME / VALUE / SOURCE / RUNNING: SOURCE is the file
   path, `unit Environment=` or `default`; RUNNING is `same`,
   `differs — restart to apply`, `not set in running backend`, `?` when
   the API could not be fetched, or `n/a` for a name the backend does
   not read. A unit-set key that is not in `settings.Known` gets a
   trailing `(unknown setting)` note (the same typo class
   `settings.CheckEnvFile` warns about). Secrets are masked in every
   output, including `--json`; a raw secret value appears nowhere.
   Exit 0 with the table or the JSON document (an unreachable backend
   still exits 0, every RUNNING cell `?`), 1 when the unit env cannot
   be resolved (`systemctl` error, or a unit with no environment).
3. **Tests** — the API test pins the auth matrix (404 unconfigured /
   401 wrong+missing / 200 admin token and admin user's token / 401
   non-admin), the sorted settings.Known coverage, masking of
   secret-valued names, set/unset following LookupEnv and the
   RFC3339 started_at (`api/config_test.go`). The CLI tests
   (`cmd/spoond-config/env_test.go`) inject the systemctl runner, the
   file reader and the fetcher, so no test runs a real systemctl: merge
   order, unreadable-file skip, EnvironmentFiles/Environment and
   env-line parsing, the table columns with the leak-guards (raw
   secrets asserted absent), the drift footer, the unreachable-?
   shape, --json with running embedded or running_error, and the real
   fetcher over httptest (token header + 401). `settings` gains
   `TestHideIfSecret` covering every marker, case-insensitivity, even
   the SEED-only name.
4. **Docs** — `docs/operations.md` (next to the doctor env-file
   paragraph): what config show resolves, the masking rule, the
   columns, the flags. `docs/api.md` admin section: the new route in
   the table and the extended auth sentence. `CHANGELOG.md`
   [Unreleased] Added.

## Mutation checks

- Merge order flipped (files first, then `Environment=`) — caught by
  `TestUnitEnvMergeOrder`.
- Files not parsed at all — caught.
- "Later file wins" flipped to first-wins in `apply` — caught.
- SEED marker dropped from the masking rule — caught by
  `TestHideIfSecret`.
- Masking dropped in the endpoint — caught by TestAdminConfigBody.
- `set:true` hardwired — caught.
- started_at blanked — caught.
- Route unregistered — caught.
- classify always `same` — caught by TestRunTable/TestRunUnsetDefaults.
- unknown-note suppressed — caught.
- drift flag hardwired false — caught by the footer count.
- unreachable exit flipped to 1 — caught by TestRunUnreachable.
- raw secret printed in the table/JSON — caught by the leak-guards.

## Gates

- `GOMAXPROCS=2 go test ./api ./settings ./cmd/... -count=1 -p=2` —
  clean (also run as a non-root user with a home/cache outside the
  lease's paths).
- `go vet ./...` — clean.
- `gofmt -l .` — clean.
