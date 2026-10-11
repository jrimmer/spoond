# One validated configuration surface (#137) — 3.6 design

Status: settled by orch-1, 2026-10-11 PT (owner: "go for it"; spoond
stays straightforward to use and manage).

## Decision: keep the env files, validate them

spoond's daemons are configured by systemd `EnvironmentFile=`s under
`/etc/spoond`. That stays the one configuration surface: it is what
systemd already understands, `spoond config show` now explains it (where
each value came from, whether the running backend has it), and `spoond
doctor` flags unknown and repeated keys. A second format (TOML/YAML)
would add a translation layer and a migration without fixing the
incidents behind #137, which were a typo'd or stale key and a value
found wrong only after a restart.

What is missing is validation and keeping secrets out of the
environment:

1. **C1 — typed settings, checked at startup.** `settings.Known` gains a
   type per setting (int, bool, duration, percent, URL, address, list,
   string) and an owner binary. The backend validates every setting it
   reads that is set, before doing anything else, and refuses to start
   with one message naming every problem (`MAX_TTL_SECS="2h": not an
   integer`). Unset settings keep their defaults. Unknown keys stay a
   `spoond doctor` warning, not a startup failure (the process
   environment also holds systemd's own variables).
2. **C2 — secrets from files.** Every secret setting (`ADMIN_TOKEN`,
   `METRICS_TOKEN`, `EVENTS_TOKEN`, `CONSUMER_TOKENS`, `GATEWAY_TOKEN`,
   `BOOTSTRAP_TOKEN`, `LLM_UPSTREAM_KEY`, `PROXY_AUTH_SECRET`, …) may be
   given as `<NAME>_FILE=/path`: the backend reads the file (trailing
   newline trimmed) at startup. Setting both forms is a startup error.
   A file readable by group or others is a startup warning. `spoond
   config show` shows the path for a `_FILE` setting (never the
   content), and `spoond doctor` checks the file exists and is 0600.
3. **Docs**: an operations section "Moving a secret to a file" (create
   the file 0600, replace `NAME=` with `NAME_FILE=`, restart, check with
   `spoond config show`).

Out of scope: a new config format, reloading without a restart.
