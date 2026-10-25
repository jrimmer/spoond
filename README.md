# spoond

Fast, isolated, ephemeral compute for people and agents: a lease API in
front of **Firecracker microVMs run by E2B's orchestrator**. Consumers
request a sandbox, run work in it, and release it. Every sandbox starts
as a memory-snapshot restore, so starts are warm, and fork, pause/resume
and checkpoint are native. On top: an SSH gateway, an HTTP proxy, an LLM
gateway, an MCP server for agents, a Forgejo Actions runner and a
read-only dashboard.

![The spoond dashboard: capacity, host, throughput, leases, images, services, records, refusals and events panels](docs/images/dashboard.png)

```
spoond (this repo, one Go binary)          e2b-orchestrator (our fork of
┌──────────────────────────────────┐       e2b-dev/runtime, patch queue)
│ lease API            :8890       │ gRPC  ┌──────────────────────────────┐
│ HTTP proxy, LLM gateway :8891    │ ────▶ │ Firecracker microVM lifecycle│
│ SSH gateway + ctl    :2222       │ :5008 │ memory snapshots (UFFD), NBD │
│ dashboard (read-only) :8893      │       │ rootfs, netns per sandbox,   │
│ MCP server for agents            │ envd  │ egress firewall, templates   │
│ Forgejo Actions runner           │ ────▶ │ envd in every guest (:49983) │
│ SQLite state (leases, catalog)   │ :5007 └──────────────────────────────┘
└──────────────────────────────────┘
your Dockerfiles → docker → local registry → E2B templates (spoond images)
```

spoond is a generic microVM utility: it takes no position on how agents
work, and its clients drive it through the lease API. Since 2.0 it runs
on E2B's orchestrator; forkd, the 1.x substrate, is gone. The design,
decisions and per-unit specs are in
[docs/plans/2026-09-30-e2b-substrate/](docs/plans/2026-09-30-e2b-substrate/00-README.md),
and every change is in [CHANGELOG.md](CHANGELOG.md).

## What it gives you

- **Lease API**: `POST /api/leases` (image, size, TTL, persistent,
  network policy, exposed ports, holder, secrets), then exec, stream
  (WebSocket PTY), keepalive, suspend/resume,
  restart (warm or cold), checkpoint, restore, fork, clone, tag, comment,
  delete. Auth via bearer
  tokens (`CONSUMER_TOKENS=token=owner,...`) or per-user identity tokens.
  Leases and the image catalog persist in SQLite, so a backend restart
  loses nothing. On every lease:
  - **Files**: `/api/leases/{id}/files/{path}` puts, gets, stats, makes
    and removes files in the guest, up to 256 MiB per file (a `Range`
    header reads just a slice, so larger files read in pieces), and lists a
    directory with `?list=1` — a bounded, gitignore-aware walk (depth
    first, 50000 entries / 4 MiB of entries / 10 s by default, ignored
    directories pruned without being walked). `GET
    …/files/{dir}?watch=1&recursive=true|false` streams the directory's
    changes as Server-Sent Events: one `ready` event, then `change`
    events (paths coalesced within 100 ms), and finally one terminal
    `error` event — `lease_suspended`, `lease_lost` or `lease_gone` (or
    `watch_ended` for any other end) — when the lease suspends, is lost
    or is released, after which the stream ends. A watch holds an
    exec/stream slot but is not activity:
    an idle lease with an open watch still suspends on schedule.
  - **Background jobs**: `POST /api/leases/{id}/exec` with
    `"background": true` starts a tracked job (202 + `job_id`);
    `GET …/jobs/{job}?wait=<s>` long-polls to its exit, and
    `…/jobs/{job}/output` follows stdout/stderr by byte range, while
    `job_started`/`job_exited`/`job_lost` ride the event stream.
    `DELETE …/jobs/{job}` removes a finished job's record and its guest
    files at once. An exec can ask for its own `retention_secs`
    (shortening `JOB_RETENTION_SECS`) and `output_max_bytes`: a stream
    that grows past the cap has its front punched out
    (`fallocate --punch-hole`) between polls and only the tail is kept,
    reported as `skipped` with absolute offsets intact. Job records
    outlive their lease: after it ends, its owner still reads how each
    job ended (`"lease_ended": true`), and a job running at the end reads
    `lost` with the guest's last stderr, so a worker that stopped early
    and released its own lease leaves a trace
    ([example](docs/usage.md#long-running-work-background-jobs-and-whats-left-when-a-lease-ends)).
  - **Crash test**: on a host with `CRASH_TEST=1`, `POST
    /api/leases/{id}/crash-test` sends the caller's own lease through
    crash recovery (back from its last checkpoint with a new generation,
    or lost without one), so a client can test how it survives a crash.
  - **Guest port dial**: `GET /api/leases/{id}/ports/{port}/dial` opens
    raw TCP to any port in the guest over a WebSocket (a database's own
    protocol, a debugger, a REPL), under every network policy.
  - **Secrets as files**: a `secrets` object on create or exec becomes
    0600 files under `/run/secrets` on a guest tmpfs, never environment
    variables, argv, the store or logs.
  - **Lease-scoped self token**: `"self_token": true` on create puts a
    token, the lease id and (when `SELF_TOKEN_API_URL` is set) the API
    URL under `/run/secrets` (`spoond-lease-token`, `spoond-lease-id`,
    `spoond-api-url`). The token releases or keeps alive exactly its own
    lease and nothing else; every other call answers `403
    lease_token_scope`. It stays the same across a resume, restart,
    recovery or snapshot save, so the guest can read it once; fork and
    clone children get their own. Only its sha256 is stored; it dies with
    the lease. With `"self_token_llm": true` as well, the same token stands
    in for the owner's LLM key at the LLM gateway, on that lease only, so
    the owner's key never has to enter the guest
    ([example](docs/usage.md#from-inside-the-sandbox-without-the-owners-key)).
  - **Event stream**: `GET /api/leases/events` (or one lease's
    `/events`) streams lifecycle events over SSE, with resumable
    positions.
  - **Generations**: a `generation` counter, also in
    `/run/spoond/generation` in the guest, bumped when the guest's memory
    did not continue (crash recovery, a cold or non-persistent restart, a
    restore), so a client can tell its processes were restored.
  - **Checkpoints on request**: no periodic checkpoints unless the lease
    asks (`checkpoint_interval`); `POST /checkpoint` any time, with
    `keep: true` to pin it (at most `MAX_KEPT_PER_LEASE`, default 4,
    within the owner's fair disk slice), and `POST /restore` puts the lease
    back to a kept checkpoint in place. `restart?mode=cold` gives the lease a fresh
    guest from its image, keeping its id.
  - **Named snapshots**: `POST /api/leases/{id}/snapshots` saves a live
    lease as a named, versioned checkpoint that outlives it (`name` or
    `name@v`); a later lease starts from it with `"snapshot":
    "spoond/warm@3"` (the version's memory, a new lease id and generation
    `1`). Saves are idempotent by an `idempotency_key`, the `/run/secrets`
    files are scrubbed before the checkpoint, and a name keeps its last
    `SNAPSHOT_KEEP_VERSIONS` versions. `GET`/`DELETE
    /api/named-snapshots/{name}[@v]` list, show and delete them. An
    owner may grant a name to another user
    (`/api/named-snapshot-grants`), who starts from it with
    `"snapshot_owner"`; only versions saved from a lease that never had
    a secret can be shared, and a lease started from a snapshot can
    restore back to it. On the
    source, spoond writes `/run/spoond/last-save`; on a lease started
    from a snapshot it writes `/run/spoond/lease-id`,
    `/run/spoond/generation` and `/run/spoond/started-from`, so a
    restored process can tell which side it is on — see
    [api.md](docs/api.md#identity-in-a-restored-guest).
  - **Holders and pins**: `holder` and `holder_url` are plain labels
    saying what holds a lease (a CI job, an orchestrator's run,
    someone's scratch work). A **pinned** lease (`pinned` on create, or
    `PUT/DELETE /api/leases/{id}/pin`) is never paused or deleted by
    spoond before its own expiry; take-back touches only unpinned
    leases, and every paused lease is released 30 days after its pause
    date by one clock. See
    [operations.md](docs/operations.md#pins-and-the-paused-release-clock).
- **Multi-user tenancy**: people and agents are first-class identities,
  with per-user SSH keys, per-user tokens, a per-user `max_ttl`, an
  equal fair share of memory and snapshot disk among active owners (no
  per-owner caps since 3.0), admin roles, lease sharing with expiry,
  per-user LLM gateway keys, per-user egress ceilings (the most network
  access an identity's leases may have, with a dry run that lists what a
  ceiling would cut; [example](docs/usage.md#capping-what-an-agent-can-reach)),
  and per-user proxy hostnames (`<label>.<user>.sandbox.example`). See
  [docs/security.md](docs/security.md) and the [Users & identity API
  section](docs/api.md#users--identity).
- **Fair shares**: every owner holding a running or paused lease gets
  an equal, floating 1/N slice of memory and snapshot disk (N counts
  active owners only — an identity that never took a lease does not
  split the box, and a lost lease never counts); an owner may borrow
  past it while the box has room. When a request does not fit, spoond
  takes room back from the biggest borrower (highest usage over
  slice): memory take-back
  pauses that owner's least recently used unpinned lease, and disk
  take-back releases their oldest unpinned paused lease. A shortage
  answers `503 capacity_wait` with `Retry-After`; a request take-back
  cannot serve (every candidate pinned, or the caller is itself the
  biggest borrower) answers `429 box_full`. `GET /api/usage` shows an
  owner's slice and ratio, lease and job counts, its creates waiting for
  room with their place in the queue, and its recent refusals and
  take-backs; `GET /api/usage/history` keeps 7 days of 15-minute
  samples. `GET /api/fair-shares` (admin) shows every owner's. A create may **wait** for room (`"wait": <seconds>`, up to
  `MAX_ADMIT_WAIT_SECS`), served lowest ratio first; a persistent lease
  may set `idle_suspend` to give its memory back when idle. See
  [upgrading-3.0.md](docs/upgrading-3.0.md).
- **Per-lease sizes**: a create picks its VM's size by preset
  (`"size": "large"`, listed by `GET /api/sizes`) or by `vcpu` and
  `memory_mb`; the image gives the default and a minimum. Sizes with a
  warm build (the image default and the presets an image lists in
  `warm_sizes`) start in milliseconds from a memory snapshot; any other
  size boots in about a second (`"start": "cold"`). Memory is the lease's
  charge for fair shares; vCPU is never charged. A lease keeps its size
  through pause, resume, clone, fork, restore and recovery, and a chosen
  size survives restarts too.
- **Resume on use**: a suspended lease, whatever suspended it
  (`idle_suspend`, take-back or its owner), resumes
  on its holder's next work call: exec, exec stream, files, guest dial,
  the proxy, jobs, the LLM gateway, a network change or a prompt. A
  resume that finds no room answers `503` `capacity_wait` with
  `Retry-After` and the lease stays suspended; no failed resume marks a
  lease lost. Automatic suspends name their reason on the lease and its
  `suspended` event.
- **Images**: spoond ships none. Any Dockerfile that copies in spoond's
  guest init ([images/README.md](images/README.md)) is built into an E2B
  template by `spoond images build <name>` (or `--all`); `spoond images
  describe --all` copies a manifest's capability and description onto
  images already built, for `/api/capabilities`. Every guest
  resolves names through the LAN resolver only and carries a container
  marker, so container tools such as kaniko work inside sandboxes.
- **SSH gateway**: `ssh new@sandbox.example` auto-creates a persistent
  sandbox; `ssh <lease-id>@sandbox.example` re-attaches; friendly names
  after `tag`. Sessions land in a tmux session. The operator names the
  images: `GATEWAY_SSH_IMAGES` (images with sshd), `GATEWAY_DEFAULT_IMAGE`
  (what a bare `new` creates) and `GATEWAY_IMAGE_ALIASES` (short names
  such as `dev=my-dev-image`).
- **Control plane over SSH**: `ssh ctl@sandbox.example "ls"` (pretty
  table by default; `--json` for raw). Verbs: `new`, `create`, `ls`,
  `stat`, `rm`, `keepalive`, `suspend`, `resume`, `restart`, `cp`
  (clone), `snapshot save|ls|show|rm`, `tag`, `comment`, `exec`,
  `share`, `ssh-key` (admin), `whoami`, `shelly`, `prompt`.
- **HTTP proxy**: `<lease-id>.sandbox.example` and `<id>-<port>`
  public URLs for sandbox web servers (Caddy fronts TLS).
- **LLM gateway**: per-lease OpenAI-compatible endpoint
  (`/llm/<lease-id>/openai/chat/completions`); upstream keys stay on
  the host, never inside sandboxes.
- **Agent access**: `spoond mcp`, an MCP stdio server with
  shell/read_file/write_file/edit_file/list_files/status tools, so any
  MCP-capable agent can work inside a lease.
- **Per-sandbox network policy**: `none` | `lan` | `internet` |
  `restricted` (with an egress allowlist), enforced by the orchestrator's
  egress firewall. `internet` and `lan` guests may call the lease API
  (CI jobs lease databases from inside their sandbox); `restricted` and
  `none` may not. A `restricted` allowlist takes IPs, CIDRs, domains and
  lease references, and an admin's egress ceiling bounds what an
  identity can ever allow.
- **Forgejo Actions runner**: `spoond runner` leases sandboxes as CI
  workers. `IMAGE_MAP` maps `runs-on` labels to images (`DEFAULT_IMAGE`
  for the rest), and a restart picks up new `RUNNER_LABELS`
  ([adding a label](docs/ci-jobs.md#adding-a-label)). A checkout step tests the commit that triggered the run (or
  the step's `with: ref:`). On a full node a job waits for admission and retries instead
  of failing, and a job's `timeout-minutes` is enforced as in GitHub
  Actions (execution from sandbox creation), capped by
  `RUNNER_JOB_TIMEOUT` (default 6h, the whole job).
- **Dashboard**: `spoond dash` and `spoond top`, below.
- **Observability**: `/metrics` (Prometheus) covers the backend, the
  orchestrator (via an OpenTelemetry collector) and leases per state and
  per image. It needs an admin token or the scrape-only `METRICS_TOKEN`.
  Every lease create, release, loss and suspend also writes one line to
  the backend's journal (the lease journal), so `journalctl` alone can
  say what happened to a lease and why.
  `GET /readyz` answers uptime monitors such as Gatus (orchestrator,
  database, disk and hugepage checks; an example config is in
  [docs/operations.md](docs/operations.md#uptime-monitoring-gatus)).
- **Notifications**: with `NOTIFY_WEBHOOKS` set, the backend pushes what
  needs a person (a lost lease, a box_full refusal, a paused lease
  nearing its release, a pinned-idle notice, a unit down,
  disk or hugepages past their levels, a failed GC, a stale backup) to ntfy, Slack/Discord or any JSON
  receiver, with hourly dedupe, resolved messages, retries and a rate
  limit. `spoond notify test` checks the setup; see
  [docs/operations.md](docs/operations.md#notifications-to-webhooks).

## Error codes

Every error on the files, exec and jobs routes carries a stable,
machine-readable `code` beside the free-form `error` message — the same
shape on the lease create/exec/stream routes' refusals and on the exec
stream's WebSocket error frames too. Code is the
contract: programs branch on it. The message is diagnostic only: it can
change between releases. Codes follow the errno names where a POSIX
class fits (the e2b substrate's file layer is a filesystem) and
snake_case verbs where it does not. HTTP statuses are unchanged by
this and remain the first-class signal.

| Code | Status | Meaning | Caller effect |
|---|---|---|---|
| `ENOENT` | 404 | no such file or path (files; also `GET …/stat` of a file) | fix the path |
| `ENOTDIR` | 409 | a file sits in a path's way | fix the path |
| `EISDIR` | 409 | the target is a directory (read or write over one) | use the directory ops |
| `EEXIST` | 409 | the wrong type exists at the path (mkdir over a file) | remove or pick a new name |
| `ENOTEMPTY` | 409 | non-recursive remove of a non-empty directory | retry with `?recursive=1` |
| `EACCES` | 403 | the substrate refused a file operation on permissions | check the file's mode/owner in the guest (e2b's `GET` content read has no refusal reason and answers `EIO`) |
| `EFBIG` | 413 | file or request body over the size cap | send less |
| `EINVAL` | 400/404 | invalid path, `op`, `mode`, signal, stream or job-output parameter | fix the request |
| `EIO` | 400/502 | a request body's bytes could not be read (400), or a substrate read/write failure that is none of the above (502) | retry; investigate if persistent |

| Code | Status | Meaning | Caller effect |
|---|---|---|---|
| `bad_request` | 400 | request body failed validation (exec/jobs/secrets) | fix the request |
| `invalid_size` | 400 | create body's size fields are unusable | fix `memory_mb`/`disk_mb`/`vcpu` |
| `not_found` | 404 | the lease is not the caller's (or unknown) | check the lease id and token |
| `owner_deleted` | 403 | a resume-on-use refusal on the files, exec, stream and jobs routes: the owner's identity was removed while the request was in flight | stop; the owner is gone |
| `lease_suspended` | 409 | a non-work path hit a suspended lease | resume the lease first |
| `lease_busy` | 409 | a checkpoint or other lifecycle operation is in flight | retry after `Retry-After` |
| `lease_lost` | 410 | the sandbox is gone with nothing in flight | create a new lease (data is lost) |
| `lease_not_live` | 409 | the lease has no live sandbox for the operation | start or resume the lease |
| `cannot_start` | 409 | the lease failed to reach a running sandbox | inspect the lease's `failed_state` |
| `save_in_progress` | 409 | a named-snapshot save already holds the lease | retry after `Retry-After` |
| `not_secret_free` | 409 | a snapshot grant of a lease that had secrets | share only secret-free leases |
| `secrets_in_use` | 409 | a background job with staged secrets blocks the save | retry after the job exits |
| `snapshot_in_use` | 409 | a live lease is started from the named snapshot | stop (or delete) that lease first |
| `snapshot_limit` | 409 | the owner's `MAX_NAMED_SNAPSHOTS` cap is reached | delete a named snapshot first |
| `scrub_failed` | 500 | the save could not scrub `/run/secrets` | inspect the guest; retry the save |
| `kept_budget` | 409 | too many kept checkpoints for the lease | unpin or delete a kept one |
| `image_mismatch` | 409 | snapshot start with a mismatching `image` | omit `image` or use the snapshot's |
| `path_removed` | 409 | a create idempotency replay raced its DELETE | create a fresh lease id |
| `box_full` | 429 | the host's box is full and take-back freed nothing | wait; poll the lease list |
| `capacity_wait` | 503 | no room for the operation right now | retry after `Retry-After` |
| `substrate_unavailable` | 503 | the substrate could not confirm the sandbox state | retry after `Retry-After` |
| `too_busy` | 429 | the caller's per-owner exec/stream/file-transfer/dial cap is reached (identity overrides raised via the quota route) | retry once one of the caller's own calls ends (no `Retry-After`) |
| `too_many_jobs` | 429 | the lease is at its running background-job cap | wait for a job to exit |
| `spawn_error` | 500 | the stream's command or a background job could never start (agent unreachable) | retry; inspect the sandbox |
| `EIO` | 500 | a sync exec, or a stream after its start, failed in the transport (whether the command ran is unknown) | check for its effects before re-running |
| `job_not_found` | 404 | unknown job id, or the job belongs to another lease | check `job_id` |
| `job_not_running` | 409 | signal of a finished job, or its pid never appeared | read the record's `state` |
| `job_running` | 409 | delete of a job that is still running | signal it, wait for it to exit, then delete |
| `timeout` | 200 | a sync exec was ended by the substrate's own time limit (beside `exit: 124`, from the result's timeout flag, never from the output) | re-run with a longer `timeout` |
| `internal` | 500 | an unexpected backend or store failure | retry once; report if persistent |

The full per-route tables (including the statuses' history) are in
[docs/api.md](docs/api.md); `GET /api/capabilities` self-describes the
deployment.

## Dashboard

`spoond dash` is a read-only, live view of one spoond host in the
browser; `spoond top` draws the same view in a terminal. It shows
capacity and host health (CPU, memory, hugepages, disks), throughput,
the live leases, the image catalog, the systemd units, all-time records
from the lease history, refusals and failures, and the newest lease
events. A notifications panel appears above the rest only when the host
needs a person, such as a unit that is down or a snapshot disk past its
danger level. The view refreshes every 2 seconds. Problems with a single
lease are not shown there: spoond tells the lease's owner through the
lease event stream and the API.

On a host it runs as the `spoond-dash` unit on **:8893** (HTTPS, with
basic auth unless `DASH_AUTH=off`). Its data access is read-only:
`/metrics` through the scrape-only `METRICS_TOKEN` (which the lease API
refuses), the lease event stream through the events-only `EVENTS_TOKEN`,
the SQLite catalog opened read-only, user names from the identity store,
`/proc` and systemd.

`spoond install` sets it up: user `admin`, the generated password in
`/etc/spoond/dash-password`, and the tokens in `/etc/spoond/dash.env`
(every setting is listed in `cmd/spoond-dash/dash.go`). To change the
password, `spoond dash hash 'the-password'` prints the bcrypt hash for
`DASH_PASSWORD_HASH`.

## Install

### Will your box fit?

| Needs | Check on the box |
|---|---|
| x86-64 with KVM: bare metal, or a VM with nested virtualisation on | `uname -m` says `x86_64`; `ls /dev/kvm` exists |
| 16 GiB of memory or more (most of it becomes sandbox memory) | `free -g` |
| Debian 13 (other distributions warn; kernel 6.1 or newer) | `cat /etc/os-release`; `uname -r` |
| 100 GiB free for the snapshot store: a directory (XFS with reflink is best) or a spare disk | `df -h /` (or the disk or directory you'll pass as `--store-dir` or `--store-disk`) |
| Ports 8890, 8891, 2222, 8893, 5000, 5007 and 5008 free | `ss -ltn` |
| Internet access during the install | |

Or let the installer check for you; this phase only reads:

```bash
sudo ./spoond install check
```

[docs/install.md](docs/install.md#requirements) has the details, and how
much memory goes to sandboxes.

### Quick start

One command installs it:

```bash
sudo ./spoond install --host-name sandbox.example.com --admin-key ~/.ssh/id_ed25519.pub
```

It sets up the host, the E2B orchestrator and spoond's services, creates
the first admin user and ends with `spoond doctor`; the images leases
run are yours to build ([images/README.md](images/README.md)); Docker Compose (`deploy/docker/`) runs the same stack in
containers. [docs/install.md](docs/install.md) has both.

## Status

**v4.1.0** (2026-10-25) is the current release. spoond installs on any
Debian 13 host with KVM in one command (`spoond install`), or with
Docker from `deploy/docker/`, using the release's artifacts; see
[docs/install.md](docs/install.md). Since 4.1 it installs bare: it ships
no sandbox images and assumes none, so the operator builds the images
the host runs ([images/README.md](images/README.md)). 4.1 also keeps a
lease's job records after the lease ends, lets a lease's own token stand
in for its owner's LLM key, adds per-user egress ceilings, and makes a
runner restart pick up new labels; hosts moving from 4.0 read the 4.1.0
section of [CHANGELOG.md](CHANGELOG.md). It shares a host fairly: every active
owner gets an equal, floating slice of memory and snapshot disk, and
spoond pauses a lease on its own before the host runs out of memory.
4.0 removes what 3.x deprecated: the `/api/sandboxes` path, keyless LLM
access and `hold_ttl`, and the Go module path is now `/v4`. Clients
moving from 3.x read the 4.0.0 section of [CHANGELOG.md](CHANGELOG.md);
clients moving from 2.9 also read
[docs/upgrading-3.0.md](docs/upgrading-3.0.md).

Every release and what changed in it is in [CHANGELOG.md](CHANGELOG.md)
and on the [releases page](https://github.com/jrimmer/spoond/releases).

## Build

One binary, all services; exclude any module with build tags:

```bash
go build -o spoond ./cmd/spoond                                   # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Exclusion tags: `nobackend`, `nogateway`, `nomcp`, `norunner`,
`noctl`, `noimages`, `nocapabilities`, `nodoctor`, `nodrain`, `noinstall`, `nodash` (excludes `dash`
and `top` together), `nonotify`.

```bash
./spoond backend    # lease API, HTTP proxy, LLM gateway
./spoond gateway    # SSH gateway + ctl plane
./spoond mcp        # MCP endpoint
./spoond runner     # Forgejo Actions runner
./spoond ctl        # control-plane CLI
./spoond images     # build images into E2B templates; list the catalog
./spoond capabilities # what this spoond offers: images, sizes, policies, routes, surfaces
./spoond drain      # pause sandboxes before an orchestrator restart, resume after
./spoond install    # clean native install: check, host, runtime, spoond, images, doctor phases
./spoond doctor     # health checks (below)
./spoond dash       # read-only dashboard (browser)
./spoond top        # the dashboard grid in the terminal
./spoond notify test  # send a test message to every NOTIFY_WEBHOOKS receiver
```

`spoond doctor` checks the configuration, the orchestrator, the local
registry, the token seed, the SQLite database, the image catalog, the
pinned E2B artifacts (SHA-256, plus the Firecracker and kernel versions
builds still use), storage headroom, the backend, the SSH gateway port,
the LLM gateway, TLS, the drain unit, and the webhook receivers (their
reachability and any deliveries dropped in the last 24 h). It exits 1 if
any check fails.

## Configuration

spoond reads its settings from env files in `/etc/spoond`. The built-in
defaults are generic (`localhost`, `sandbox.example.com`), so a
deployment sets its own hosts and addresses. `spoond config show` lists
what a unit sets and anything that differs from the running backend;
`--all` lists every setting spoond knows. Any secret can come from a
file instead of a value (`<NAME>_FILE=/path`).

The settings you are most likely to change:

| Area | Settings (default) |
|---|---|
| Hosts and addresses | `SPOOND_GUEST_DNS_ADDR` (guest resolvers, comma-separated), `SPOOND_PROXY_HOST_SUFFIX`, `NODE_ID`, `HOST_GUEST_SERVICE_ADDR` (where guests reach host services), `HOST_API_PORT` (the lease API port `lan` and `internet` guests may reach) |
| Integrations | `METRICS_TOKEN`, `LLM_UPSTREAM_URL`, `NOTIFY_WEBHOOKS` |
| Memory | `MEMORY_OVERCOMMIT` (1.25 × the hugepage pool), `MEMORY_FLOOR_MIB` (2048), `GIVE_BACK` (on), `GIVE_BACK_MARGIN_MIB` (1024) |
| Disk and state | `SPOOND_DB_PATH`, `DISK_RESERVE_PCT` (5, the share of snapshot disk kept free) |
| Lease sizes | `SIZE_PRESETS` (`small=1:2048,medium=2:4096,large=4:8192,xlarge=8:16384`), `MAX_LEASE_VCPU` (the host's cores), `MAX_LEASE_MEMORY_MB` (32768) |
| Waiting and idling | `MAX_ADMIT_WAIT_SECS`, `IDLE_SUSPEND_DEFAULT_SECS`, `SATURATION_IO_FULL_PCT` (20), `SATURATION_CPU_SOME_PCT` (75) |
| Paused leases | `PAUSED_RELEASE_DAYS` (30), `PINNED_IDLE_NOTICE_DAYS` (7) |
| Snapshots | `MAX_KEPT_PER_LEASE` (4), `MAX_NAMED_SNAPSHOTS`, `SNAPSHOT_KEEP_VERSIONS`, `SNAPSHOT_WRITE_CONCURRENCY` |
| Jobs and exec | `MAX_RUNNING_JOBS_PER_LEASE`, `JOB_RETENTION_SECS` (7 days, the record and guest files an exited job keeps — a job's own `retention_secs` may shorten it), `JOB_OUTPUT_MAX_BYTES` (0 = unlimited; a job's stdout/stderr guest file growing past it has its front punched out between polls, keeping the tail), `JOB_MAX_RUNTIME`, `MAX_EXEC_BODY_BYTES`, `EXEC_CONCURRENCY_MAX` (8; per-owner concurrent exec/stream ops — an agent client holding one stream per working thread runs that many at once), `GUEST_DIAL_MAX` (16; per-owner concurrent open guest-port dials). Both are per-identity overridable: an identity's `exec_concurrency` / `guest_dial` (set by an admin via `POST /api/users/{id}/quota`, stored with the user) replaces the host default for that identity alone; unset keeps the host default |
| CI runner | `RUNNER_LABELS`, `IMAGE_MAP` and `DEFAULT_IMAGE` (no default image), `RUNNER_ADMIT_WAIT_SECS`, `RUNNER_JOB_TIMEOUT` (6h) |
| Images the clients pick | `GATEWAY_SSH_IMAGES`, `GATEWAY_DEFAULT_IMAGE`, `GATEWAY_IMAGE_ALIASES` (SSH gateway), `SPOOND_IMAGE` (`spoond mcp`); none has a default, since spoond ships no images |
| Drain and recovery | `DRAIN_SNAPSHOT_CONCURRENCY`, `UNDRAIN_*`, `RECOVERY_RETRY_*`, `ROOTFS_PROBE_SECS`, `GUEST_UNREACHABLE_SECS`, `SWEEP_TIMEOUT`, `E2B_CREATE_TIMEOUT` and the other per-call E2B limits |
| Housekeeping and testing | `ORPHAN_REAP` (`dryrun`), `CRASH_TEST` |

More detail:

- A deployment's own hosts and addresses: [PRODUCTION-ENV-2.7.md](deploy/PRODUCTION-ENV-2.7.md).
- The E2B deployment's fixed addresses, ports and paths: [01-architecture.md](docs/plans/2026-09-30-e2b-substrate/01-architecture.md).
- Pins and the paused-release clock: [operations.md](docs/operations.md#pins-and-the-paused-release-clock).

## Tests

```bash
go test ./...            # unit tests (fast, no infra)
```

The conformance suite (`conformance/`, build tag `conformance`) is the
executable definition of the lease-API contract. It runs on the host
against a live spoond, as root; see [conformance/README.md](conformance/README.md).

## License

BSD-3-Clause from 2.4.0 (2.3.3 and earlier: Apache-2.0); see
[`LICENSE`](LICENSE) and [`NOTICE`](NOTICE), which lists the
third-party files that keep their own licenses.
Contributions welcome: see [`CONTRIBUTING.md`](CONTRIBUTING.md).
