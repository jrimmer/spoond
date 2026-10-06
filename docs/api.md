# API Reference

Base URL: `https://<backend>:8890` (HTTPS when `TLS_CERT`/`TLS_KEY` are
set, plain HTTP otherwise). The resource is the **lease**; every route
lives under `/api/leases/…`. `/api/sandboxes/…` is a permanent alias for
the same routes — identical behavior, auth and responses — kept for
every client written before 2.0; new code should use `/api/leases/…`.
All endpoints except `/healthz`, `/readyz`, the
`/api/admin/*` routes (which carry their own `ADMIN_TOKEN`) and the
`/llm/` prefix (where the lease id in the path is the capability)
require a bearer token (`/metrics` also accepts the scrape-only
`METRICS_TOKEN`):

```
Authorization: Bearer <token>
```

Tokens are either `token=consumer` pairs from `CONSUMER_TOKENS` or
per-user tokens from the identity store. The authenticated consumer id
becomes the **owner** of every lease they create. A lease id acts as a
capability: only the owner (or anyone holding the token that owns it, or
a grantee via a share) can act on it.

Errors are JSON: `{"error":"human-readable message"}` with an appropriate
HTTP status. A lease runs on an E2B microVM (the substrate "sandbox");
what that implies for a given field is noted below, and the platform
itself is described in [substrate.md](substrate.md).

---

## Leases

### `POST /api/leases` — create a lease

Request:

| Field | Type | Default | Notes |
|---|---|---|---|
| `image` | string | *(required)* | image name; must have a current build in the catalog (`GET /api/images`) |
| `ttl` | int | `DEFAULT_TTL_SECS` | seconds; capped at `MAX_TTL_SECS` and at the user's `max_ttl` |
| `persistent` | bool | `false` | not TTL-swept; supports keepalive, suspend/resume, checkpoint |
| `memory_mib` | int | `0` | **must be `0` or exactly the image's `memory_mb`** — memory is fixed per image (a snapshot restores with its build's RAM). Any other value is `400`. |
| `network` | string | *(ignored)* | accepted for compatibility |
| `init_cmd` | string | *(ignored)* | accepted for compatibility |
| `network_policy` | string | `restricted` | `none` \| `lan` \| `internet` \| `restricted` |
| `egress_allowlist` | []string | *(empty)* | IPs/CIDRs/domains for `restricted`; also lease references (see below) |
| `expose_ports` | []int | *(none)* | guest TCP ports published for peer leases. Max 8; port 49983 (envd) is refused; duplicates and out-of-range ports are refused |
| `holder` | string | `""` | what holds the lease (a CI job, an orchestrator's flight, a person's scratch work). At most 128 printable characters. A non-empty holder makes the lease **held**: it is not released at its TTL and not idle-suspended by the plain sweep. A hold expires on its own (see `hold_ttl`) — the automatic held-lease limits in [operations.md](operations.md) act regardless |
| `holder_url` | string | `""` | link to the holder; empty or an absolute `http(s)` URL of at most 512 characters |
| `hold_ttl` | int | `0` | seconds the hold lasts from now instead of the default `HOLD_TTL_SECS`; capped at `HOLD_TTL_MAX_SECS`. Ignored when `holder` is empty |
| `checkpoint_interval` | int | host default | the lease's own periodic checkpoint interval in seconds: `0` = never checkpointed by the loop; `60`–`604800` = seconds between periodic checkpoints. Omitted = the host default (`CHECKPOINT_INTERVAL_MINS`, itself `0` = never — see [Checkpoints](#checkpoints)). Anything else is `400` |
| `idle_suspend` | int | host default | the lease's own idle reclamation threshold in seconds: `0` = never; `60`–`604800` = suspend the lease after that long without activity (exec, stream, proxy, keepalive, guest heartbeat, files, guest port dial), resuming it on the next call. Omitted = the host default (`IDLE_SUSPEND_DEFAULT_SECS`, itself `0` = never — see [Idle reclamation](#idle-reclamation)). Anything else is `400`, and a non-zero value needs a persistent lease (`400`) — a non-persistent lease has nothing to suspend into |
| `burst` | bool | `false` | force the **burst** admission class: the lease is scheduled preemptibly even while the owner's charge stays within their `guaranteed_mib` — see [Lease classes](#lease-classes) |
| `priority` | int | `0` | preemption order within the lease's class: a lower number is preempted first, between `-128` and `127` (anything else is `400`) — see [Preemption](#preemption) |
| `wait` | int | `0` | seconds a create refused for a **waitable** reason may wait for room instead of failing (see [Queued admission](#queued-admission)). `0` keeps the immediate refusal; above `0` it is capped at `MAX_ADMIT_WAIT_SECS` (`0` disables waiting, the field is accepted and ignored); negative is `400` |
| `secrets` | object | *(none)* | `{name: value}` delivered as files under `/run/secrets` in the guest — see [Secrets](#secrets). At most 32 secrets and 64 KiB of values per request; names match `[A-Za-z0-9_.-]{1,64}`. Values are never stored, logged or returned: they live in the backend's memory for the lease's life and are lost on a backend restart |

Response `201 Created`:

```json
{
  "id": "8f3a…32hex…",
  "owner": "u-…",
  "address": "10.11.0.7",
  "image": "dev-base",
  "ttl": 300,
  "persistent": false,
  "expires_at": "2026-10-01T03:00:00Z",
  "holder": "ci-job-42",
  "holder_url": "https://ci.example.com/jobs/42",
  "hold_expires_at": "2026-10-08T03:00:00Z",
  "generation": 1,
  "checkpoint_interval": 300,
  "idle_suspend": 0,
  "class": "guaranteed",
  "priority": 0,
  "exposed": {"9042": "10.11.0.7:9042"}
}
```

`hold_expires_at` is `""` when the lease was created without a holder
(the zero time renders as `""`).

When the request set `wait`, the response also carries `waited_ms` (how
long admission took, in milliseconds) — `0` or a few ms when it fit at
once. See [Queued admission](#queued-admission).

`generation` is the lease's continuity generation: `1` on create, bumped
whenever the guest's memory does not continue from where its processes
left it — see [Generations](#generations).

`checkpoint_interval` in the response is the **effective** interval in
seconds: the lease's own value, or the host default when the lease has
none. `0` means the periodic checkpoint loop never touches the lease.
Clone and fork copy the source's interval; a lease created without the
field keeps the host default until `PUT
/api/leases/{id}/checkpoint-policy` changes it — see
[Checkpoints](#checkpoints).

`idle_suspend` is the **effective** idle reclamation threshold in
seconds with the same shape (the lease's own value, or the host default;
`0` = never). Clone and fork copy the source's value; a lease created
without the field keeps the host default until `PUT
/api/leases/{id}/idle-policy` changes it — see
[Idle reclamation](#idle-reclamation).

`address` is the lease's host-side address (no port). `exposed` maps
each published port to `<address>:<port>` — reachable from peers whose
egress policy permits it (see [Network policy](#network-policy)), never
from the LAN. The same map appears in `GET /api/leases`.

Errors: `400` bad policy/ports/memory/holder/secret fields, `404` unknown image,
`429` quota — the user's concurrent-lease cap or their memory cap
(#128, see `POST /api/users/{id}/quota`), `503` capacity (not enough
free hugepage memory for the image, or the node is not healthy), and
`503` `no burst capacity` with `Retry-After: 30` when the lease is
burst (asked for, or above the owner's `guaranteed_mib`) and the node's
free hugepages would dip under `BURST_RESERVE_MIB` after it — see
[Lease classes](#lease-classes). A **guaranteed** lease that cannot get
its hugepages preempts burst leases instead (see
[Preemption](#preemption)); when the snapshot disk is too full to pause
one, the create answers `503`
`capacity: cannot preempt (snapshot disk low)` with `Retry-After: 30`.

### Lease classes

Every lease is admitted as `guaranteed` or `burst`, decided once at
admission (create, fork, clone and every path that resumes a suspended
lease: resume, warm and cold restart, restore, crash recovery,
undrain) and persisted with the lease. The class answers one question:
whose room does this lease take?

- **`guaranteed`** — the owner's running charge with this lease stays
  within their `guaranteed_mib`. A user with no `guaranteed_mib` keeps
  every lease guaranteed, which is today's behaviour.
- **`burst`** — the lease passes the guarantee (the first lease past it
  bursts), or the request forced it with `"burst": true`. A burst lease
  is preemptible even within another user's guarantee, and it is
  admitted only while the node's free hugepages stay above
  `BURST_RESERVE_MIB` (default 8192, `0` = the reserve is disabled;
  see [operations.md](operations.md)) after its own — guaranteed work
  and crash recovery always have room to land. A burst lease refused on
  the reserve answers `503` `no burst capacity` with `Retry-After: 30`
  and stays as it was (suspended on resume, uncreated on create).

The class can change at re-admission: a lease that burst because the
guarantee was full may come back `guaranteed` when the charge has room
again, while a lease the request forced burst stays burst (`burst` is
the request's flag, `class` the decided and stored outcome — the flag
itself is not persisted, so a backend restart re-classifies from the
owner's standing at the next resume; the stored class still reads burst
until then). `priority`
orders preemption within a class (lower is preempted first, `0` the
default) and is stored with the lease.

### Preemption

When a **guaranteed** admission (create, fork, clone, resume, warm or
cold restart, restore, crash recovery, undrain) cannot get its
hugepages — free hugepages less the burst reserve is smaller than the
lease's `memory_mb` — spoond reclaims them from burst leases. It
suspends them through the normal pause path (a snapshot build; the
memory continues on resume, so the generation does **not** change), in
this order: lowest `priority`, then newest, then the owner furthest
over its `guaranteed_mib`. It stops as soon as enough memory is free and
admits the guaranteed lease. Preemption is serialised: one preempting
admission at a time, so two guaranteed creates cannot each preempt for
themselves.

A preempted lease is marked `preempted: true` in `GET /api/leases` and
`GET /api/leases/{id}`, keeps its `resume_build_id`, and emits a
`preempted` event with detail `for a guaranteed lease of <owner>`. It
stays suspended until it fits again: the backend resumes preempted
leases every 15 s, oldest preemption first, through the normal resume
path (as a burst lease again if the owner is still above the
guarantee). On resume `preempted` is cleared and a `resumed` event is
emitted with detail `after preemption`. A client's explicit resume of a
preempted lease takes the same path; until it succeeds the lease stays
suspended.

Preemption has a disk floor: it pauses a burst lease only while the
snapshot disk stays above `PREEMPT_DISK_FLOOR_PCT` (default 15) after
the pause, estimated from the lease's `memory_mb`. When no candidate
clears that floor, the guaranteed admission answers `503`
`capacity: cannot preempt (snapshot disk low)` with `Retry-After: 30`.
Honey-like clients should treat a `preempted` lease as temporarily
unavailable and wait for its `resumed` event (or poll the lease) rather
than deleting and recreating it.

### Queued admission

A create refused only because the node is full can wait for room instead
of failing: send `"wait": N` (seconds) on `POST /api/leases` (and its
`/api/sandboxes` alias). `wait` defaults to `0`, which keeps today's
immediate refusal exactly; a negative value is `400`.

Only these refusals are **waitable** (the request is held open and
retried):

- `503 capacity: …` — hugepages full and preemption could not make room
  (or the node is not healthy).
- `503 no burst capacity` — the burst reserve would be crossed.
- `503 capacity: cannot preempt (snapshot disk low)` — the preemption
  disk floor blocked it.
- `429 … memory limit of N MiB exceeded` — the owner's `max_mib` cap
  refused it.
- `429 lease quota exceeded` — the owner is at its lease-count cap
  (`max_leases`); the create waits for one of the owner's own leases to
  be released (since 2.5.1). The `queued` event names it `lease cap`.

Everything else answers at once: bad requests, auth failures and
unknown images are **not** waitable.

The wait is capped at `MAX_ADMIT_WAIT_SECS` (default `900`; `0` disables
waiting and the field is accepted and ignored — see
[operations.md](operations.md)). The queue lives in the backend process
and is lost on restart (a waiting client sees its connection go and
retries). Waiting creates are served in **fair-share order**: the owner
furthest under its `guaranteed_mib` first (an owner with no
`guaranteed_mib`, or already at it, ranks after every owner with
headroom), then FIFO. Each wake-up admits every queued create that fits,
so a smaller create may pass a larger one that still does not fit. The
order is re-read before every attempt; it is best-effort only when room
frees in the middle of a pass (a create tried just before may miss room
that one tried just after gets).
Admissions from the queue go through the normal admission path, so
classes, the burst reserve, quotas and preemption apply unchanged. The
queue is retried whenever capacity may have freed (a release, suspend,
resume, preemption or quota change) and on a 5 s tick.

Outcomes:

- **Admitted** — the usual `201` body, plus `waited_ms` (the wait in
  milliseconds).
- **Timed out** — the refusal the create would have got had it not
  waited: the same status, body and `Retry-After`, plus `waited_ms`.
- **Client gone** — the ticket is dropped; nothing is written.
- **Drain** — a drain that starts answers every queued create `503
  draining` at once.

The `queued` event names the refusal being waited out and the create's
place in the fair-share order at that moment, e.g. `memory cap; position
2 of 3`; the lease id is allocated when the create is queued and the
created lease keeps it. On admission a `created` event follows as usual;
a wait that ends without a lease emits `timed_out` with detail `waited
Ns`, `client gone` or `draining`.

The create holds its HTTP request open for the whole wait. spoond sets
no server write timeout, but the client's own timeout must be longer
than its `wait` (`curl -m`, Go's `http.Client.Timeout`), or the client
gives up first and its ticket is dropped.

### `GET /api/leases/queue` — creates waiting for admission

Lists the caller's waiting creates (an admin sees every owner's) in
fair-share order: `{"queued": [{"id", "owner", "image", "position",
"waited_s", "reason"}]}`. `position` is the place in the whole queue
(1 is tried first), `reason` the refusal being waited out. Empty when
nothing waits. A client whose create is waiting can poll this to show
progress.

### Idle reclamation

A persistent lease may be suspended after a period without activity —
its own `idle_suspend` (or the host default `IDLE_SUSPEND_DEFAULT_SECS`,
both `0` = never). The plain `IDLE_TIMEOUT_SECS` sweep and the
held-lease idle rule (rule 1, and rule 4's pressure shortening) apply to
leases whose effective `idle_suspend` is `0`; a lease with a non-zero
value is reclaimed on that value alone. The idle sweep suspends it
through the normal pause path: memory and hugepages are freed into a
pause build, nothing is deleted, the generation does not change, and the
idle sweep shares preemption's snapshot-disk floor
(`PREEMPT_DISK_FLOOR_PCT`) — a pause that would take the disk under it
is skipped for that sweep and retried on the next one.

Activity is what the sweep counts: exec, stream, proxy, keepalive, guest
heartbeat, files API and guest port dial all move `LastActive`. An idle
suspension records `last_action` `idle_suspend/suspend_idle` with
`last_action_at` (persisted, like the held rules) and emits an
`idle_suspended` event whose detail is `idle for <duration>`. Because it
is a rule suspension, the stale-release (rule 2) and critical-disk
(rule 5) held-lease rules may later release the lease if it stays
idle-suspended and untouched; a preempted lease stays excluded.

**Resume on next use:** an exec, files call, guest port dial or stream
on a lease suspended by `idle_suspend` resumes it first through the
normal resume path (admission, class and quota apply) and then serves
the call; a refusal answers what resume would (`429` over quota, `503`
with `Retry-After` for capacity or the burst reserve) and the lease stays
suspended. A lease suspended any other way keeps the `409`
`lease is suspended; resume it first`. An explicit resume works as
always, and the SSH gateway already resumes on attach.

### `GET /api/leases` — list leases

Response `200 OK`: `{"sandboxes":[ {…lease…}, … ]}` where each row has
`id`, `owner`, `image`, `address`, `expires` (unix seconds),
`persistent`, `suspended`, `state`, `build_id`, `resume_build_id`,
`name`, `comment`, `holder`, `holder_url`, `net_policy`,
`egress_allowlist`, `exposed`, `generation`, `checkpoint_interval`
(effective seconds; `0` = never), `idle_suspend` (effective seconds;
`0` = never), `class`, `priority` and `preempted`
(see [Lease classes](#lease-classes) and
[Preemption](#preemption)).

A held lease's row adds `hold_expires_at` (RFC 3339, when the hold
expires and normal sweeping resumes) and — after the first automatic
held-lease action — `last_action` (`"rule/action"`, e.g.
`"idle/suspend_idle"`) with `last_action_at` (RFC 3339); see
[operations.md](operations.md) for the rules behind them.

### `GET /api/leases/{id}` — lease detail
The same object as a list row plus `state`, `recovered_from` (RFC 3339 or
`""`), `last_checkpoint_at` and `kept_builds` — the checkpoints the lease
pinned with `{"keep":true}` (#126), oldest keep first:

```json
"kept_builds": [
  {"build_id": "<uuid>", "size_bytes": 83928702976, "kept_at": "2026-10-01T12:00:00Z"}
]
```

`size_bytes` is the recorded disk size (allocated bytes, #125), so the
list shows what unpinning would free. Requires the owner or an `http`
share.

A lease with an identity-store owner also carries its owner's memory
quota (#128): `charged_mib` (the owner's current running-lease charge —
what this lease contributes to while it runs), `guaranteed_mib` and
`max_mib` (`0` = unset) — the same numbers as `GET /api/users/me`,
scoped to this lease's owner.

`state` is `running`, `suspended`, `recovered` or `lost`. `recovered`
behaves exactly like `running` — it marks a lease the crash reconcile
resumed from a checkpoint, and keeps showing `recovered` until the lease
is suspended or restarted. `lost` means the sandbox died with no
checkpoint; those leases answer `410` and should be deleted.

`build_id` is the E2B build the running sandbox was created from (`""`
while suspended, where `resume_build_id` is the one to resume from).

### Generations

Every lease carries a `generation` (in the create response and in every
list and detail row): the count of times the guest's memory did **not**
continue from where its processes left it. It starts at `1` on create
and is bumped — and persisted — by exactly four paths:

- **Crash recovery.** The crash reconcile resumed the lease from its
  checkpoint build; the processes in the guest find themselves in a
  memory snapshot taken earlier.
- **Restart of a non-persistent lease.** `POST /api/leases/{id}/restart`
  replaces its sandbox with a fresh one from the image, so the guest
  starts over. Restarting a persistent lease is a snapshot round-trip
  (pause, then resume from that pause build): the memory continues and
  the generation stays. Before 2.2.1 it bumped there too. The cold mode
  (`?mode=cold`) puts a persistent lease on the fresh-guest path too,
  and bumps its generation.
- **Restore to a kept checkpoint (2.3, #121).**
  `POST /api/leases/{id}/restore` replaces the sandbox with one from a
  checkpoint the lease pinned — the guest finds itself in that older
  snapshot, so work newer than it is gone.

A planned suspend/resume and the admin drain/undrain continue the
memory — the guest is resumed from the snapshot its own pause wrote —
and do **not** bump the generation. Neither does anything else: exec,
proxy, checkpoint, clone, fork and keepalive leave it alone.

After every bump the new value is written into the guest at
`/run/spoond/generation` (one line, `"2\n"`; the file is `0644`, its
parent `/run/spoond` is created `0755`). The file is also written at
create and on every resume, so a lease created before 2.2 gets it the
first time it resumes. The write is best
effort: a failure is logged and changes nothing else.

Processes in the guest read the file to notice that their memory did
not continue, and re-derive whatever they keep only in process (caches,
locks, half-finished work):

```bash
cat /run/spoond/generation   # e.g. 2
```

### `GET /api/names/{name}` — resolve by name

`{"id": "<lease-id>", "name": …, "image": …}` for a friendly name set
with `tag`. Owner-scoped. Used by the SSH gateway (`ssh <name>@…`) and by
scripts.

### `DELETE /api/leases/{id}` — delete

Releases the lease and its sandbox. `204 No Content`. Builds are left for
the GC (and stay listed by `GET /api/snapshots` until reclaimed).

The delete accepts an optional **reason** that its `released` event
carries, so a caller (the CI runner, an operator's script) can say why
it let the lease go: `?reason=<text>` on the query string, or a JSON
body `{"reason": "<text>"}`. The text is at most 120 printable
characters, sanitised like a lease comment (control and format
characters become spaces), and `400` otherwise. Without a reason the
event keeps its `deleted through the API` detail. The same reason works
through the `/api/sandboxes` alias.

### `POST /api/leases/{id}/exec` — run a command

| Field | Type | Default | Notes |
|---|---|---|---|
| `cmd` | string | *(required)* | shell command (run via `bash -c`) |
| `cwd` | string | *(none)* | working directory |
| `env` | object | *(none)* | extra environment variables |
| `timeout` | int | `30` | seconds; capped at `MAX_EXEC_TIMEOUT_SECS` (default 300) |
| `secrets` | object | *(none)* | `{name: value}` written to `/run/secrets` for this command only and removed afterwards — see [Secrets](#secrets). Same limits as on create |

Response `200 OK`:

```json
{"stdout": "…", "stderr": "…", "exit": 0}
```

`409` if the lease is suspended (resume it first); `410` if it is
`lost`, or if the sandbox no longer exists on the substrate; `429` when
the per-owner concurrent exec/stream cap is reached. (Exec does not wait for or take the lease's lifecycle lock; its concurrency
guard is the per-owner cap, which yields `429`. A busy lease shows up only
as the `409` below.)

A lease the idle sweep suspended (`idle_suspend`) is the exception: a
suspended lease whose `last_action` is `idle_suspend/suspend_idle` is
resumed first through the normal resume path and the exec then served;
a refused resume answers what resume would (`429` over quota, `503` with
`Retry-After` for capacity or the burst reserve). Any other suspension
keeps the plain `409` — see [Idle reclamation](#idle-reclamation).

While a lifecycle operation is in flight on the lease (the periodic
checkpoint, a suspend or a restart), the orchestrator briefly reports
its sandbox missing; for a large guest a checkpoint can take a couple of
minutes. Exec, stat, guest dial and the file routes then answer `409`
with `Retry-After: 5`, not `410`: retry. `410` means the sandbox is gone
with nothing in flight.

### `…/api/leases/{id}/files/{path…}` — lease files

Read and write files inside the lease's filesystem. `{path…}` is the
guest-absolute path (everything after `/files/`); it is cleaned before
use, so `…/files/a/../b` is `…/files/b`, and `..` can never escape the
guest root — a path that cleans to `/` names no file and answers `404`.
Access follows the strictest lease model: the **owner** (or an admin,
who may act on any lease); a grantee's `http` share does **not** carry
file content, and anyone else gets the usual `404`. Every call counts
as activity for the idle sweeper. A suspended lease answers `409` on
every file route (resume it first), except one suspended by
`idle_suspend`, which is resumed first and then served (see
[Idle reclamation](#idle-reclamation)); a `lost` lease answers `410`. File
counts and sizes are capped at **256 MiB**: a bigger upload is refused
with `413` before anything is written, and a bigger download with
`413` instead of the bytes.

**`GET`** downloads the file: `200` with
`Content-Type: application/octet-stream` and the raw bytes; `404` when
it does not exist. With `?stat=1` the route returns the metadata
instead:

```json
{"name":"motd","size":11,"mode":"640","mod_time":"2026-10-05T09:30:00Z","is_dir":false}
```

`mode` is the octal permission string, `mod_time` is RFC 3339, `is_dir`
distinguishes a directory (its `size` is substrate-defined, `0` on the
fake) from a file.

**`PUT`** replaces the file with the request body (parents are created;
an existing file is replaced, an existing directory in the way is a
`409`). The mode comes from `?mode=0644` — octal, default `0644` —
and is applied to the created file. Response `201` with the new file's
stat document (as above).

**`POST ?op=mkdir`** creates the directory and any missing parents.
`?mode=0755` (octal, default `0755`) applies to the leaf directory;
existing nodes are left untouched. Response `201` with the stat
document; anything other than `op=mkdir` is `400`.

**`DELETE`** removes the file or directory. `?recursive=1` removes a
non-empty directory with everything under it; without it, a non-empty
directory is refused with `409`. `204 No Content` on success, `404`
when the path does not exist.

Other errors: `400` for a malformed `mode` (not octal, or beyond
`0777`: setuid, setgid and sticky bits are not applied, so they are
refused) or a bad `op`; `429` when four file transfers (`GET` content
or `PUT`) are already in flight on the backend.

### `GET /api/leases/{id}/stat` — guest metrics

One-shot, stateless probe (loadavg/meminfo/netdev/df via exec, 5s
timeout). Response `200 OK`:

```json
{
  "cpu":  {"load1": 0.08},
  "mem":  {"used_mib": 320, "total_mib": 1024},
  "disk": {"used_mib": 512, "total_mib": 4096},
  "net":  {"rx_bytes": 12345, "tx_bytes": 6789}
}
```

### `GET /api/leases/{id}/endpoint` — resolve lease endpoint

Kept for compatibility. The gateway no longer uses it (SSH sessions are
relayed over `/stream`). The `forkd_id` key keeps its pre-2.0 name
(stored/protocol data; renaming it would break clients):

```json
{"id":"…","forkd_id":"<sandbox id>","image":"…","netns":"","guest_addr":"10.11.0.7"}
```

### `GET /api/leases/{id}/stream` — interactive process (WebSocket)

Upgrade to WebSocket; the first client message starts a process:

```json
{"args":["/bin/bash","-l"],"cwd":"/work","env":{…},"pty":true,
 "binary":true,"cols":120,"rows":40}
```

| Field | Default | Notes |
|---|---|---|
| `args` | *(required)* | argv |
| `cwd`, `env` | *(none)* | working directory and extra env |
| `pty` | `true` | allocate a PTY |
| `binary` | `false` | binary framing (below) |
| `cols`, `rows` | `80`×`24` | initial PTY size |

**Text mode (the default).** Server events are one text JSON frame each,
with no trailing newline:

- `{"stream":"started","pid":1234,"pty":true}`
- `{"out":"…"}` — stdout, stderr and PTY output all use this frame
- `{"exit_code":N}`, then the socket closes
- `{"error":"…"}`, then the socket closes

Client text frames are control JSON:

| Frame | Effect |
|---|---|
| `{"in":"…"}` | write to the process (PTY input, or stdin without a PTY) — **text mode only**; in binary mode send the bytes as binary frames |
| `{"resize":{"cols":C,"rows":R}}` | resize the PTY |
| `{"action":"stop"}` | SIGTERM |
| `{"action":"kill"}` | SIGKILL |
| `{"action":"eof"}` | close stdin (non-PTY only) |

**Binary mode** (`"binary":true`) is what the SSH gateway uses. Process
output arrives as WebSocket **binary** frames whose first byte selects
the channel — `1` stdout, `2` stderr, `3` PTY — with the payload after
it; client binary frames are raw input written to the process; text
frames carry the same control JSON as above. `started`, `exit_code` and
`error` stay text JSON frames in both modes.

Closing the WebSocket stops the relay but does **not** kill the process;
send `stop` or `kill` for that. A stream attach on a lease the idle
sweep suspended resumes the lease first and then starts the process (see
[Idle reclamation](#idle-reclamation)); any other suspension keeps the
`409`.

### `GET /api/leases/{id}/ports/{port}/dial` — raw TCP to a guest port (WebSocket)

Upgrade to a WebSocket that carries raw bytes both ways to TCP port
`{port}` (1–65535) inside the lease — a guest service the owner wants to
reach directly: a database shell, a REPL, a debug port. The connection is
made host-to-guest through the substrate (`DialGuest`); it is **not
guest egress**, so it works under every `network_policy` — `restricted`
and `none` included. Egress rules decide what may leave a sandbox; no
packet leaves the guest here.

Frames are **binary** in both directions: client binary frames are bytes
written to the guest port, and every byte the guest sends comes back as
a binary frame. Either side closing closes both (a guest close surfaces
as the WebSocket closing; a client close tears down the guest TCP
connection). A dial that carries no bytes in either direction for 10
minutes is closed; a stream that flows only one way stays open. Text
frames and empty frames from the client are ignored.

Owner or admin; anyone else gets the same `404` as the other lease
routes (no existence leak). Shares do not unlock dialing: it reaches
every guest port, a step past what an `http` share grants.

Errors: `400` port out of range or not a number, `403` port 49983 (envd,
the guest's management port), `404` unknown lease or not the owner's,
`409` suspended (resume it first — except a lease the idle sweep
suspended, which is resumed first and then dialed; see
[Idle reclamation](#idle-reclamation)), `410` lost, `429` when the owner's 16
concurrent dials are already open, `502` when the lease has no running
sandbox or the guest port refuses the connection.

### `POST /api/leases/{id}/keepalive` — extend a persistent lease

Request `{"ttl": <seconds>}` (0 = `MAX_TTL_SECS`; capped). Response:
`{"id":"…","persistent":true,"expires_at":"…"}`. `400` if the lease is
not persistent.

### `POST /api/leases/{id}/suspend` — snapshot + stop

Persistent leases only (`400` otherwise). The sandbox is paused into a
new build and stops; the lease stays, becomes `state: "suspended"`, and
records `resume_build_id`. Response
`{"id":"…","status":"suspended","message":"lease suspended; state snapshot kept (resume to restore)"}`.

### `POST /api/leases/{id}/resume` — start from the snapshot

Restores a suspended lease from `resume_build_id` **with the same sandbox
id**, so its address and identity are unchanged. Owner only (admins
too); the SSH gateway's service token may resume any lease before a
session starts. Response
`{"id":"…","status":"running","address":"…"}`. `400` if neither persistent nor held,
`409` if the lease is busy (another lifecycle operation is in flight).
A suspended lease holds no hugepages, so resuming one re-passes the
owner's memory quota (#128): `429` when the charge would pass
`max_mib` — the lease stays suspended. The resume re-decides the
lease's class too (#128 part 2): a burst lease coming back into a full
burst reserve answers `503` `no burst capacity` with `Retry-After: 30`
and stays suspended. Resuming a lease that a preemption suspended
(#128 part 3, `preempted: true`) takes the same path and answers the
same way; on success `preempted` is cleared and the `resumed` event's
detail is `after preemption`. Resuming a lease that is
already running does nothing and answers `200`
with the lease as it is: the guest keeps its memory. (Before 2.1.2 it
restored the pause build again, rolling the guest's memory back.)

### `POST /api/leases/{id}/restart` — pause and resume, or a fresh guest

Takes an optional `mode`: `warm` (the default, also when the parameter
or body field is absent) or `cold`. The mode comes from `?mode=` or the
JSON body `{"mode":"…"}`; the body wins when both are set. Anything else
is `400`.

**Warm** (the default) is the behaviour described first below. Persistent and
running: suspend then resume (same lease, same build chain, lossless
through the pause build). This is **not a reboot**: the guest comes back
with the same memory and processes, so a hung or out-of-memory process
is still there afterwards. Persistent and suspended: resume.
Non-persistent: delete the sandbox and create a fresh one from the
image's current build, keeping the lease id (its disk is lost — there is
no snapshot to restore). The non-persistent path bumps the lease's
generation and rewrites `/run/spoond/generation`; the persistent path
continues the guest's memory and keeps it (see
[Generations](#generations)).

**Cold** (`?mode=cold`) gives any lease — persistent or not, running or
suspended — the fresh-guest path: the sandbox is deleted and a new one
is created from the image's current build, keeping the lease id, owner,
holder, name, network policy and exposed ports. The generation bumps and
`/run/spoond/generation` is rewritten, and the create-time secrets are
re-written into the fresh sandbox. What cold loses: everything the guest
held in memory and on its ephemeral disk, any exec-time secrets (they
ride their request, as always), and — on a persistent lease — the pause
builds: `resume_build_id` is cleared, so the lease's next suspend writes
a fresh resume point and older checkpoints stay but are no longer on the
resume chain. A persistent lease restarted cold keeps being persistent,
and a suspended lease restarted cold comes back running.

Response `{"id":"…","status":"running","message":"lease restarted"}`.
`404` unknown, `400` for an unknown mode, `409` when busy; restarting a
suspended lease brings its guest (and its hugepages) back, so it
re-passes the owner's memory quota (#128): `429` when the charge would
pass `max_mib` — the lease stays suspended, untouched. The restart
re-decides the lease's class (#128 part 2), so a burst lease restarting
into a full burst reserve answers `503` `no burst capacity` with
`Retry-After: 30` — like resume, fork and clone — and stays suspended.
A substrate failure on the fresh-guest path (which does create a sandbox) surfaces
as `500`, not `503` — unlike create, fork and clone, restart does not
map capacity errors to `503`.

### `POST /api/leases/{id}/checkpoint` — snapshot a running lease

Owner only, live leases only (`409` otherwise, including while another
operation is in flight). Writes a checkpoint build the lease can be
recovered from, and the lease keeps running from it. This is also what
the background checkpoint loop does for a due lease. The optional body
`{"keep":true}` **keeps** the checkpoint: the build joins the GC's kept
set while the lease lives and becomes a restore point for
`POST /api/leases/{id}/restore` (see below) — otherwise it ages out of
the catalog like any unreferenced snapshot once the lease moves on.
Releasing the lease (any path) drops its kept builds, and
`DELETE /api/snapshots/{build_id}` also unpins a kept build. Response:

```json
{"id":"…","build_id":"<uuid>","at":"2026-10-01T12:00:00Z","kept":true}
```

`kept` mirrors the request (`false` when the body was empty; a
malformed body is the usual `400`, not a silent "keep nothing").

**Keep limits (#126).** Two caps govern `keep`:

- **Per lease** — `MAX_KEPT_PER_LEASE` (default `4`, `0` = no cap). A
  keep on a lease already holding that many kept builds answers `409`
  `{"error":"kept checkpoint limit reached (4 per lease); unpin one with
  DELETE /api/snapshots/{build_id}"}`. Nothing is evicted and **no
  checkpoint is taken**; unpin a build to free a slot.
- **Per owner** — an optional `max_kept_bytes` on the user record (set
  via `POST /api/users/{id}/quota`). The build's size is only known
  after it is written, so an over-budget keep **takes the checkpoint**
  and then refuses the pin: `409` naming the budget with the new build's
  `build_id` and `"kept":false`. The build stays an ordinary,
  GC-able checkpoint — retry without `keep`, or unpin builds to make
  room.

### `POST /api/leases/{id}/restore` — roll back to a kept checkpoint

Owner or admin (anyone else gets the usual `404`). Replaces the lease's
sandbox with one from one of the lease's **own** checkpoints or kept
builds — the guest rolls back to that snapshot in place:

```json
{"build_id": "<uuid>"}
```

The build must be this lease's newest checkpoint or a build it pinned
with `{"keep":true}`; another lease's checkpoint, another owner's build
and pause builds all answer `404`, like a lease the caller cannot see.
Works on a running or a suspended lease; a lease lost in a substrate
crash answers `410` like every other route (restore does not resurrect
it); `409` while another operation is in flight; a substrate capacity
failure maps to `503`. Restoring a suspended lease brings a running
sandbox (and its hugepages) back, so it re-passes the owner's memory
quota (#128): `429` when the charge would pass `max_mib` — the lease
stays suspended, untouched. The restore re-decides the lease's class
(#128 part 2), so a burst lease restored into a full burst reserve
answers `503` `no burst capacity` with `Retry-After: 30`, and the
lease stays as it was.

The lease keeps its id, owner, holder, name, network policy, exposed
ports and `checkpoint_interval`. Everything else about the guest starts
over from the checkpoint: files and processes newer than the snapshot
are gone, the generation bumps (see [Generations](#generations)),
`/run/spoond/generation` is rewritten, the create-time secrets are
re-written into the fresh sandbox, and the `restored` event carries the
build id. The lease comes back running (a suspended lease too, a
drained one included — the restored sandbox is running, so undrain no
longer owes it a resume), and its pause builds stop being its resume
point (`resume_build_id` is cleared; the next suspend sets it as
usual). Response `200`:

```json
{"id":"…","build_id":"<uuid>","generation":2,"status":"running"}
```

### `PUT /api/leases/{id}/checkpoint-policy` — set the checkpoint interval

Owner or admin (anyone else gets the usual `404`). Sets the lease's own
periodic checkpoint interval, overriding the host default:

```json
{"checkpoint_interval": 3600}
```

`checkpoint_interval` is required: `0` = the loop never checkpoints the
lease; `60`–`604800` = seconds between periodic checkpoints. Anything
else is `400` naming the field. Response `200 OK`:

```json
{"id":"…","checkpoint_interval":3600,"ok":true}
```

`checkpoint_interval` in the response is the effective value (the value
just set). The change emits a `checkpoint_policy` lease event naming the
new effective seconds — see [Lease events](#lease-events-server-sent-events) — and takes effect on
the loop's next pass (it ticks every minute). While a lease is busy
(409-checking lifecycle operations), the setting still applies; the loop
itself skips busy leases.

### `PUT /api/leases/{id}/idle-policy` — set the idle reclamation threshold

Owner or admin (anyone else gets the usual `404`). Sets the lease's own
idle reclamation threshold, overriding the host default:

```json
{"idle_suspend": 3600}
```

`idle_suspend` is required: `0` = never reclaimed by the idle sweep;
`60`–`604800` = seconds without activity before the sweep suspends the
lease. Anything else is `400` naming the field; a non-zero value on a
non-persistent lease is `400` (suspension needs a persistent lease).
Response `200 OK`:

```json
{"id":"…","idle_suspend":3600,"ok":true}
```

`idle_suspend` in the response is the effective value (the value just
set). The change emits an `idle_policy` lease event naming the new
effective seconds — see [Lease events](#lease-events-server-sent-events) — and takes effect on
the sweep's next pass.

### Checkpoints

A running lease can be snapshotted into a **checkpoint build**: the
guest's RAM is written out while the guest is paused briefly, and the
lease keeps running from the new build. Checkpoints happen three ways:

- on demand, `POST /api/leases/{id}/checkpoint`;
- periodically, for leases whose effective `checkpoint_interval` is
  `> 0` and that have been active since their last checkpoint — the
  host default comes from `CHECKPOINT_INTERVAL_MINS` (default `0` =
  never, see [operations.md](operations.md)), a lease's own
  `checkpoint_interval` overrides it;
- implicitly, as the first half of clone and fork (and the persistent
  restart round-trip).

The snapshot is what makes the lease survivable: an **orchestrator
(crash) recovery resumes a lease from its newest checkpoint**, so a
lease that has never been checkpointed **is lost** when the orchestrator
dies with the sandbox — its memory is gone with the VM. A planned
restart (or a suspend/resume, or the admin drain) is different: those
pause the guest into a fresh build first and resume from it, so nothing
is lost in a planned drain — drain suspends every lease into its own
pause build and undrain resumes exactly those. The loss window of a
crash is therefore bounded by the checkpoint interval: how long a lease
can run after its last checkpoint. `recovered_from` and
`last_checkpoint_at` on the lease detail show when that snapshot was
taken; the `spoond_checkpoint_pause_seconds` metric shows how long each
checkpoint pauses its guest.

### `POST /api/leases/{id}/clone` — branch to a new lease

Checkpoints the running sandbox and grants a fresh **persistent** lease
from the checkpoint build, copying the source's network policy and
exposed ports. The optional body `{"tag":"…"}` is accepted and ignored —
the checkpoint build id is the snapshot's identity (use `tag` for a
friendly *name*). Response `201 Created`:

```json
{"id":"…","image":"…","source":"<source-id>","branch_tag":"<build id>","persistent":true,"expires_at":"…"}
```

The clone costs the source image's `memory_mb` against the owner's
memory quota like any create (#128); `429` when it would pass
`max_mib`. The clone is classified and held to the burst reserve like
any create (#128 part 2); a refusal answers `503` `no burst capacity`.

### `POST /api/leases/{id}/fork` — N copies of a running lease

Owner only, as clone (shares are not honoured). Checkpoints the source
once and creates `count` leases from that build, each its own lease
owned by the caller, with the source's policy copied. Quota is reserved
for all of them up front (all or nothing); if any create fails, every
lease created in the call is deleted.

Request `{"count":1..20,"persistent":false,"ttl":300,"hold_ttl":600}`
(`ttl` 0 = the default TTL, capped at the maximum and the user's
`max_ttl`; `hold_ttl` bounds the forks' hold like on create). The
optional `holder`, `holder_url` and `hold_ttl` fields stamp every
created lease (same validation as create; the forks are held like the
holder wants its work kept).

Response `201 Created`:
`{"source":"<id>","build_id":"<uuid>","ids":["…","…"],"hold_expires_at":"…"}`.
Errors: `400` bad count or bad holder fields, `404` unknown, `409`
suspended or busy, `429` quota — including the memory cap (#128): each
fork costs its image's `memory_mb`, `count` times, reserved up front,
all or nothing — `503` capacity. Every child is classified at
admission (#128 part 2): with the whole batch's charge pending, all
children of a fork past the guarantee burst together, and a burst child
refused on the reserve fails the call with `503` `no burst capacity`.

### `POST /api/leases/{id}/network` — change egress policy live

Owner only. Updates the lease's policy and allowlist, re-applies the
egress config to the running sandbox, and refreshes every peer's
allowances — no restart, no new lease.

Request `{"network_policy":"none|lan|internet|restricted","egress_allowlist":[…]}`.
Response `200` `{"id","network_policy","egress_allowlist"}`. `400` on an
invalid policy, `404` unknown, `409` suspended.

### `POST /api/leases/{id}/tag` — friendly name

Request `{"name": "<unique-per-owner-name>"}`. Response
`{"id":"…","name":"…","ok":true}`. Names enable `ssh <name>@…` and
`GET /api/names/{name}`.

### `POST /api/leases/{id}/comment` — annotate

Request `{"comment": "…"}`. Response `{"id":"…","comment":"…","ok":true}`.

### `PUT /api/leases/{id}/holder` — set, renew or clear what holds the lease

Owner or admin; anyone else gets the same `404` as the other lease
routes. Sets the holder later on an existing lease — the same fields as
create plus `hold_ttl`:

```json
{"holder": "ci-job-42", "holder_url": "https://ci.example.com/jobs/42", "hold_ttl": 600}
```

Both fields empty clears the holder and restores normal sweeping. The
same validation applies as on create (`400` naming the offending
field). Response `200`
`{"id":"…","holder":"…","holder_url":"…","hold_expires_at":"…","ok":true}`.

Putting the **same** holder on a lease that is already held **renews**
the hold: it lasts another `HOLD_TTL_SECS` (or the given, capped
`hold_ttl`) from now. A **different** holder is refused with `409` —
take the lease over by clearing first.

**Held-lease semantics:** a lease with a non-empty `holder` is not
released by the TTL sweeper and is not idle-suspended by the plain
sweep — a CI job or an orchestrator can hold a plain (non-persistent)
lease past its TTL without keep-alive calls. Being held does not itself
put the lease on the periodic checkpoint pass: set the lease's
`checkpoint_interval` (on create or via `PUT
/api/leases/{id}/checkpoint-policy`) so its work survives a crash — see
[Checkpoints](#checkpoints). The hold ends
on its own (`HOLD_TTL_SECS` from when it was set or renewed, at most
`HOLD_TTL_MAX_SECS` for an explicit `hold_ttl`): a lapsed hold suspends
a running lease and never releases one; the lease keeps its holder,
`hold_state` becomes `lapsed`, and it is released only after staying
suspended and untouched for the stale limit, unless renewed. Even while held, the automatic limits in
[operations.md](operations.md) act on their own — idle suspend,
release of stale suspended leases, the pressure and critical-disk
rules — and every action is reported as `last_action`/`last_action_at`
on the lease. A held lease suspended by the idle rule resumes on next
use: the SSH gateway does this automatically on attach, and the owner
can call `POST /api/leases/{id}/resume`.

### `POST /api/leases/{id}/resume` — resume a held lease (gateway)

Owner-blind resume for **held** leases: used by the SSH gateway on
attach, where the capability is the lease id or name and no owner id is
known. Restores a rule-1-suspended held lease from `resume_build_id`
with the same sandbox id (see the held-lease semantics above). An
unheld or unknown lease answers `404`. Response `200`
`{"id":"…","status":"running","address":"…"}`.

### `POST /api/leases/{id}/prompt` — message the in-sandbox Shelley agent

Request `{"message":"…","model":"gpt-oss-20b-fireworks"}` (model
optional). Polls the Shelley conversation API inside the sandbox and
returns the agent's reply. Requires the agent to be running (see the
`shelly` ctl verb). Response `200 OK` with
`{"id":"…","message":"…","reply":"…"}` (the lease id, the message sent, and
the agent's reply).

---

## Lease events (Server-Sent Events)

Every lease lifecycle change emits one event on an in-process event
bus (#115). The bus keeps the last 10000 events; the two SSE routes
below stream them live, with resume.

### `GET /api/leases/events` — stream the caller's lease events

Server-Sent Events. Admins receive every lease's events; everyone else
receives only events for their own leases. `?lease_id=<id>` narrows the
stream to one lease (a lease you cannot see answers the same `404` as
the other lease routes). Requires a bearer token: a consumer token (or
an admin user), or the events-only `EVENTS_TOKEN` — which sees every
owner's events and works on this route only. Everywhere else — the
one-lease streams, the `/api/sandboxes` spellings included — refuses
it, and any method but GET gets `405` here.

### `GET /api/leases/{id}/events` — stream one lease's events

Same stream, pre-filtered to the lease in the path. The owner (or an
admin); anyone else gets `404`, like every other lease route — the
events-only `EVENTS_TOKEN` included: its one route is the all-events
stream above.

### Wire format

Each event carries an `id` of the form `<epoch>-<seq>`, an `event` type
and a JSON `data` payload:

```
id: 9f1c2a4b8d3e5f60-42
event: created
data: {"seq":42,"epoch":"9f1c2a4b8d3e5f60","at":"2026-10-04T12:00:00.123456789Z","lease_id":"8f3a…","owner":"u-…","type":"created","detail":"granted from image dev-base"}

```

- `seq` is monotonic per backend process (1, 2, 3, …), assigned in emit
  order. Events on a stream never go backwards and never repeat.
- `epoch` is a random id generated at backend start and stable for the
  process's lifetime. A different epoch in a later event id means the
  backend restarted (and the sequence may have restarted with it).
- `at` is the emit time (UTC, RFC3339 with nanoseconds).
- `detail` is a short human-readable note (empty is possible).

The stream opens with a `retry: 3000` hint and a `: keepalive` comment
every 15 s thereafter, so proxies do not close an idle stream.

### Event types

| `event` | emitted when | `detail` names |
|---|---|---|
| `created` | a lease is granted, forked or cloned | the source image and how long the grant took, e.g. `granted from image py-base in 61 ms` (forks: the source lease and build; clones: the source lease and checkpoint build) |
| `released` | the lease is deleted (TTL sweep, idle rules, `DELETE`, held-lease release) | why: the caller's `DELETE` reason when given (the runner sends e.g. `ci job 3609 ✓ 11m02s` or `ci job 3604 ✗ 4m10s`), else `deleted through the API`, `TTL expired`, `released by a held-lease rule` (or `lease released`) |
| `suspended` | the sandbox is paused into a build (suspend, drain, held idle-suspend, hold lapse) | the pause build id |
| `resumed` | the lease starts from a pause build (resume, undrain, gateway resume, preemption resume) | the resume build id; `after preemption` for a lease the resume queue brought back after preemption |
| `preempted` | a guaranteed admission suspended a burst lease to reclaim its hugepages (preemption, #128 part 3) | `for a guaranteed lease of <owner>` |
| `checkpointed` | a running lease is checkpointed | the duration and the checkpoint build id, e.g. `540 ms · build 9e1f2ab3…` |
| `recovered` | a lease is resumed from its checkpoint after a crash | the checkpoint build id |
| `lost` | the lease's sandbox died with nothing to recover from (crash reconcile, failed undrain resume) | the reason |
| `restarted` | `POST /api/leases/{id}/restart` completed | `restarted (snapshot round-trip)` for a warm persistent restart, `cold` for `mode=cold`, or `cold-restarted from image <image>` for a non-persistent lease |
| `restored` | `POST /api/leases/{id}/restore` completed (2.3, #121) | the restored-to checkpoint build id |
| `holder_set` | a hold is set or renewed on `PUT /api/leases/{id}/holder` | the holder and the new `hold_expires_at` |
| `holder_cleared` | the hold is cleared | the clear |
| `held_action` | an automatic held-lease rule acted (idle suspend, stale/pressure/critical release, lapse) | the rule, the action and the numbers that triggered it |
| `checkpoint_policy` | the lease's checkpoint interval changed on `PUT /api/leases/{id}/checkpoint-policy` | the new effective `checkpoint_interval` seconds |
| `idle_policy` | the lease's idle threshold changed on `PUT /api/leases/{id}/idle-policy` | the new effective `idle_suspend` seconds |
| `idle_suspended` | the idle sweep suspended the lease through the pause path | `idle for <duration>` |
| `gc` | a catalog GC pass deleted builds (spoond's own maintenance, not a lease's) | `N builds deleted · X GiB freed`, e.g. `1 build deleted · 512.0 MiB freed` |
| `gap` | a hole in *your* stream, not a lease change | what was missed and why |

A `gc` event is lease-less: its `lease_id` and `owner` are empty, it
reaches the all-leases stream (and the events-only `EVENTS_TOKEN`) but
never `GET /api/leases/{id}/events` or a per-lease in-process
subscription, and the dashboard shows its subject as `spoond`. A GC
pass that deletes nothing (the default dry run included) emits none.

### Resume and gaps

Send the last seen `id` as `Last-Event-ID` on reconnect:

- Same epoch and the event is still in the ring (last 10000): the
  stream resumes — every missed event is replayed, in order, and then
  live events flow. Nothing is lost or duplicated across the seam.
- Same epoch but the event has left the ring, or the id is past the
  newest sequence, or the id does not parse as `<epoch>-<seq>` at all:
  the stream sends one `gap` event naming the position it could not
  honour, then live events. The gap's id is the current position, so
  a reconnect from it resumes normally.
- Different epoch (the backend restarted): same — a `gap` event first
  ("epoch … is not the current epoch"), then live events. Treat the
  epoch change as a signal to re-list your leases.

Every event is checked again as it is written: a stream only ever
carries events stamped with the caller's owner id (admins: all, and
the events-only `EVENTS_TOKEN` on its one route). A `gap` marker is
always delivered — it reports the caller's own stream, not a lease
change.

A `gap` event is not part of the bus's sequence; it exists only in
streams (and for in-process subscribers that fell behind, see below).
A connect-time gap carries the current position as its `seq` and id;
a gap for events dropped mid-stream has `seq` `0` and no id line, so
the client keeps its last real id. The `suspended` and `resumed` pair
also brackets `restarted` when a running persistent lease is restarted
through a snapshot.

### In-process subscribers

The same bus is available inside the backend process:
`Service.Subscribe(filter)` returns a channel of events matching the
filter (by owner, by lease id, or everything). Delivery is
non-blocking: a subscriber that does not keep up has events dropped —
never blocking the lease lifecycle — and receives a `gap` event
detailing the loss once it catches up.

---

## Secrets

Lease create and exec both accept an optional `secrets` object
(`{name: value}`) for credentials a workload needs — API tokens, private
registry passwords. The delivery is a **file**, never an environment
variable and never part of the command line:

- Before anything runs, the backend makes sure a `tmpfs` is mounted at
  `/run/secrets` inside the guest (mode `0700`, owned by the user exec
  runs as) and writes every secret as `/run/secrets/<name>`, mode
  `0600`, through the substrate's file API. Nothing touches env or
  argv, so no secret can appear in `ps` output, shell history or error
  strings.
- **Create-time secrets** stay for the lease's life. They are re-written
  after a resume or restart (and after crash recovery), so a fresh
  sandbox gets them too.
- The tmpfs is guest memory: a suspend, checkpoint, fork or clone
  snapshot contains the files that were present when it was taken, and
  a fork or clone starts with them (the backend does not re-stage
  secrets into a fork or clone; send them again on its create or exec).
- **Exec-time secrets** are written before the command and removed when
  it finishes. A name that shadows a create-time secret is restored to
  the lease's value afterwards.
- Limits per request: at most **32** secrets and **64 KiB** of values in
  total; names match `[A-Za-z0-9_.-]{1,64}` (they become file names).
  Violations are `400`.

Values are held in the backend's **memory only**. They are never
persisted to SQLite, never written to logs or metrics, and never
returned by any endpoint — there is no read-back. A backend restart
loses every secret it holds; the caller must re-send create-time
secrets on its next exec (an exec request's secrets replace what the
backend would re-stage, and the same names are removed again when the
command finishes). See also the security notes in
[security.md](security.md).

```bash
curl -X POST https://backend/api/leases \
  -H "Authorization: Bearer $TOKEN" -d '{
    "image": "dev-base",
    "secrets": {"NPM_TOKEN": "…"}
  }'

curl -X POST https://backend/api/leases/$ID/exec \
  -H "Authorization: Bearer $TOKEN" -d '{
    "cmd": "npm publish",
    "secrets": {"NPM_TOKEN": "…"}
  }'
```

---

## Network policy

`network_policy` decides what may leave a lease; the substrate enforces
it, from the config carried on create and updated live by `/network`.
The default is **`restricted`** — a guest reaches only the host services
spoond grants it (the proxy/LLM gateway port and DNS) plus its
allowlist, and the peers that allowlist names through their published
ports. Under `lan` and `internet` there is no such gate: every other
live lease that publishes ports is reachable on those ports, whatever
the owner.

| Policy | Egress |
|---|---|
| `none` | nothing at all |
| `lan` | the LAN ranges (RFC 1918 minus the lease networks), host services, DNS, **every** exposing peer's published ports |
| `internet` | everything public, **plus** the LAN ranges, host services, DNS, **every** exposing peer's published ports |
| `restricted` *(default)* | host services, DNS, the allowlist, and the published ports of the peers the allowlist names |

`egress_allowlist` entries are IPs, CIDRs or domains. Entries that name
another lease — its id, its friendly name, or those prefixed `lease:` —
are peer references and permit that lease's published ports, not a
domain.

`lan` and `internet` guests may additionally reach the lease API itself
on the host service address (so a CI job can lease a database from inside
its sandbox); `restricted` and `none` may not.

Reserved guest ports: `49983` (envd, the guest agent) can never be
published or proxied.

---

## Images, health, snapshots

### `GET /api/images`

`{"images":["dev-base","go-base","py-base","elixir-base","elixir-release","llm-review","scylla"]}` —
the catalog entries with a current build. With `?detail=1`:

```json
{"images":[{"name":"py-base","build_id":"…","template_id":"…","digest":"…@sha256:…",
            "vcpu":2,"memory_mb":1024,"disk_mb":4096,"updated_at":"…"}]}
```

Images are built with `spoond images build` (see [ci-jobs.md](ci-jobs.md)).

### `GET /api/snapshots` — list your snapshot builds

The caller's builds (`kind` `pause` or `checkpoint`), excluding deleted
ones:

```json
{"snapshots":[{"build_id":"…","kind":"checkpoint","image":"dev-base",
               "parent_build_id":"…","size_bytes":123456,
               "created_at":"…","in_use":false}]}
```

`size_bytes` is measured when the build is written (checkpoint, pause,
clone, fork, drain; template builds get the same treatment from
`spoond images build`) and re-measured by the hourly disk accounting
pass, which overwrites every build's `size_bytes` with what the disk
then says — including any build whose write-time measurement failed and
stored 0, or whose files have changed since.

### `DELETE /api/snapshots/{build_id}` — delete a snapshot build

Checks in order: `404` unknown or already deleted; `403` for template
builds (they belong to the image catalog); `404` for another owner's;
`409 {"error":"snapshot in use"}` while the GC's kept set references it;
otherwise the build's files are removed and the row marked deleted →
`204`. A delete also **unpins** the build (2.3, #121): its kept-builds
rows go first, so a checkpoint pinned with `{"keep":true}` can be
removed ahead of the lease's own release (the delete still answers `409`
first while the lease itself runs from the build; the pin is gone after
that call, and the next one deletes).

### `GET /healthz`

No auth. `200 {"status":"ok","orchestrator":"<status>"}` when the
orchestrator answers, `503 {"status":"degraded","orchestrator":"unreachable"}`
when it does not — for Gatus/load balancers.

### `GET /readyz`

No auth. Readiness for an external uptime monitor: `200
{"status":"ok"}` only when every check passes — the orchestrator's
node info reports the node healthy, the catalog answers a trivial
query, and the snapshot disk and hugepage pool sit below the
dashboard's danger levels (90 % / 92 % used). Otherwise `503` with
`{"status":"fail","checks":[{"name","ok","detail"}…]}` naming each
failing check. Every check is bounded to 2 s and the answer is cached
for 5 s. See [operations.md](operations.md) for a Gatus example; the
dashboard listener serves a `/readyz` over its own sources too.

### `GET /metrics`

Prometheus output for the backend, plus the orchestrator's series when
`OTEL_PROM_URL` is set. Requires an admin user (identity-store mode) or
the scrape-only `METRICS_TOKEN`; `403` otherwise. The series are listed
in [operations.md](operations.md).

---

## Admin API

Routes under `/api/admin/*` are authenticated with
`Authorization: Bearer <ADMIN_TOKEN>` — a dedicated token, not a user or
consumer token. With no `ADMIN_TOKEN` configured they answer `404`; a
wrong or missing token answers `401`.

| Route | Effect |
|---|---|
| `POST /api/admin/drain` | Set the node draining and pause every live lease into a pause build (marking it drained), delete the warm pool, then wait up to 180 s until the node reports no running sandboxes and no outstanding work. Response `{"paused":N,"failed":[{"id","error"}],"pool_deleted":M,"quiesced":bool}`. `503 {"error":"orchestrator unreachable: …"}` (nothing changed) when the node cannot be reached. |
| `POST /api/admin/undrain` | Wait up to 120 s for the node, clear draining, resume exactly the drained leases (a lease that fails to resume becomes `lost`; one over its owner's memory cap stays drained for the next undrain). Response `{"resumed":N,"failed":[…]}`. |
| `POST /api/admin/reconcile` | Run the crash reconciliation now. Response `{"recovered":N,"lost":M}`. |

These are what `spoond drain --stop|--start` calls from the orchestrator
unit's `ExecStop`/`ExecStartPost`; see [operations.md](operations.md)
before using them by hand.

---

## LLM gateway (per-lease)

`POST /llm/{lease-id}/openai/chat/completions` — OpenAI-compatible chat
completion against the configured upstream. The lease id in the path is
the capability; leases hold no consumer token. The route only exists
when `LLM_UPSTREAM_URL` is set: with no upstream the gateway is never
built, so `/llm/…` answers `404` (`spoond doctor` reports the same as a
WARN). There is no in-sandbox fallback.

Per-user key auth: when the lease owner has an LLM key configured,
requests must present it as `Authorization: Bearer <user-key>`.
Missing/wrong/foreign keys → `401`. Owners without a key keep the open
behavior — **unless** `LLM_OPEN_LEGACY` is unset (the default): with an
identity store present, keyless identity users are then denied outright
(`401`) and only legacy consumer-owned leases stay open. Set
`LLM_OPEN_LEGACY=1` to restore the pre-2.0 open behavior. The user key is
replaced by the server-side upstream key before forwarding, so it never
reaches the provider.

## Guest-service endpoints (port `HOST_GUEST_SERVICE_PORT`)

Besides the per-lease LLM gateway above (`/llm/{lease-id}/…`), the
guest-service listener serves routes a sandbox itself calls. Every
network policy — `restricted` included — permits this port.

### `POST /lease/{lease-id}/active` — lease heartbeat

Records activity on a lease so the idle sweep
(`IDLE_TIMEOUT_SECS`) does not auto-suspend it while an agent is
working inside the sandbox. An agent never calls the lease API and holds
no owner token; this route is how it stays visible to the sweeper.

**Capability model:** the lease id in the path is the authorization —
the same model as `/llm/{lease-id}/` and the SSH gateway. No bearer
token; a sandbox that knows its own lease id may heartbeat it, and no
other lease is reachable or revealed through the response.

The heartbeat only sets `LastActive` (the state the idle sweep reads)
and persists it. It does **not** extend `ExpiresAt`, change
persistence, resume a suspended lease, or do anything else.

Responses: `204` on success (no body); `404` for an unknown or released
lease; `409` for a suspended lease; `405` for any other method. Writes
are limited to one per lease per 60 s — later calls inside that window
still return `204`, so a fast loop cannot hammer the store.

A guest uses the values its sandbox was created with
(`SPOOND_GATEWAY_URL`, `SPOOND_LEASE_ID`):

```
curl -fsS -X POST "$SPOOND_GATEWAY_URL/lease/$SPOOND_LEASE_ID/active"
```

Metric: `spoond_lease_heartbeats_total` counts accepted heartbeats
(every `204`, including rate-limited no-op calls).

## HTTP proxy

The proxy listener (`PROXY_ADDR`, e.g. `0.0.0.0:8891` behind a wildcard
TLS front) routes hostnames to guest ports:

- `<lease-id>.sandbox.example` → guest port 3000
- `<lease-id>-<port>.sandbox.example` → guest `<port>`
- `<label>.<user>.sandbox.example` → forward-auth mode only, scoped to
  that user

In the default capability model only the unguessable 32-hex lease id
hostname resolves (friendly names are `404`, since they are guessable);
with `PROXY_AUTH_MODE=forward-auth` every request must carry the
`X-Proxy-Auth` shared secret and a `Remote-User` identity, and lookups
are owner-scoped. Guest port 49983 (envd) is refused with `403`.

Guest apps receive `Host: 127.0.0.1:5007` (the orchestrator's sandbox
proxy), so the public hostname is preserved in `X-Forwarded-Host` and the
scheme in `X-Forwarded-Proto`. Frameworks that validate the Host header —
dev servers with a host allowlist, for instance — must read
`X-Forwarded-Host`.

## Users & identity

The user store (`USERS_FILE`) makes people and agents first-class
identities. User endpoints are admin-gated except `/me`, `/by-name`,
`/by-key` and `/identity-status`; **`GET /api/users` is admin-only** (the
full directory is not enumerable by any token holder).

### `POST /api/users` — create a user

Bootstrap: when the store is empty, the first create is open (or gated by
`X-Bootstrap-Token: <BOOTSTRAP_TOKEN>` when configured) and the first
user becomes admin. After that, admin only.

```json
{
  "name": "jason",
  "kind": "person",
  "fingerprints": ["SHA256:AbC…", "SHA256:DeF…"],
  "token": "optional-bearer-token"
}
```

- `kind`: `person` | `agent` — agents are non-interactive identities
  (the MCP endpoint authenticates as its agent user).
- `fingerprints`: SSH public-key fingerprints (use `ssh-keygen -lf
  pubkey.pub`) — the gateway's `PublicKeyCallback` resolves these.
- `token`: optional per-user bearer token (like a `CONSUMER_TOKENS`
  entry, but bound to the identity).

Response `201 Created`: `{"user": {id, name, kind, admin, fingerprints,
max_leases, max_ttl, guaranteed_mib, max_mib, used_mib, created_at}}` —
the token hash and LLM key hash are
never exposed. `used_mib` is `0` on a fresh user.

### `GET /api/users` — list users (admin only)

`403` for non-admin callers.

### `GET /api/users/me` — current user

Self-service: `{"user": {id, name, kind, admin, max_leases, max_ttl,
guaranteed_mib, max_mib, used_mib}}` (#128). `used_mib` is the
user's current memory charge — the sum of `memory_mb` over their
running leases (suspended ones hold no hugepages); `guaranteed_mib` and
`max_mib` are `0` when unset.

### `GET /api/users/by-name/{name}` — minimal lookup

`{"user": {id, name}}` — the shape for share-granting and gateway name
resolution.

### `GET /api/users/by-key?fingerprint=…` — resolve SSH key

`{"user": {id, name}}` — the gateway calls this in its
`PublicKeyCallback`; minimal shape.

### `DELETE /api/users/{id}` — remove a user (admin only)

Removes the identity and its keys. **This is what actually revokes SSH
access** — the gateway treats the identity store as authoritative when
present, so removing the user invalidates all their keys immediately.

### `POST /api/users/{id}/quota` — set lease quota (admin only)

Request `{"max_leases": N, "max_ttl": S, "max_kept_bytes": B,
"guaranteed_mib": G, "max_mib": M}` — concurrent-lease cap, per-user
TTL ceiling, the kept-checkpoint byte budget (#126), and the memory
quota (#128): the sum of `memory_mb` over the user's **running** leases
may not pass `M` (`0` = no cap; a suspended lease holds no hugepages
and is not charged). All five default to `0` =
unlimited/unset. `guaranteed_mib` is the user's guaranteed-memory
floor: a lease whose charge keeps the owner's running sum within it is
admitted `guaranteed`; once the sum passes it, the next lease is
admitted `burst` (see [Lease classes](#lease-classes)). It must be
`<= max_mib` when both are set (`400` otherwise). Over-cap
creates, forks and clones return `429`; so does any operation that
brings a suspended lease's guest back over the cap — resume, restart,
restore, crash recovery, undrain — leaving the lease suspended (or
drained, for undrain) instead. **Crash recovery of a suspended lease
re-passes the check against the image's current `memory_mb`, and a
successful recovery re-stamps `charged_mib` to that value** (a warm
restart or restore of a suspended lease does the same), so the detail
row's charge moves to what the recovered sandbox actually runs. An
over-budget keep answers `409` on the checkpoint route with the
unpinned build's id (see
[Keep limits](#post-apileasesidcheckpoint--snapshot-a-running-lease)).

There is no automatic conversion from `max_leases` to a memory limit: a
user with `max_leases > 0` and no `max_mib` keeps working unchanged. To
cap a user's memory, set `max_mib` explicitly:

```sh
curl -fsS -X POST "$SPOOND_URL/api/users/$UID/quota" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"max_leases":4,"max_mib":16384}'
```

Their in-flight usage is `used_mib` on `GET /api/users/me`.

### `POST /api/users/{id}/llm-key` — set/rotate/revoke a user's LLM gateway key

Admin only. Request: `{"llm_key": "<slk-…>"}`; an empty `llm_key`
revokes (the owner's leases revert to the open gateway). The key is
stored as a salted hash and never returned in responses. `404` unknown
user; `403` non-admin.

### `GET /api/identity-status`

`{"identity_store": true|false}` — tells the SSH gateway whether the
backend has an identity store, so the gateway knows whether key
resolution must be authoritative. Requires a bearer token (the gateway
uses its service token).

## Shares

A lease owner can grant another user access to a lease for a limited
time — sharing a workspace with a collaborator or an agent without
copying the lease id/capability.

### `POST /api/leases/{id}/share` — grant (owner only)

Request: `{"grantee": "<user-id>", "mode": "ssh"|"http", "ttl": 3600}`.
`grantee` must be an existing user id (the gateway's `share add` verb
also accepts a user name and resolves it; this route does not); `ttl` in
seconds (0 = no expiry); `mode` (default `http`) selects which
operations the grantee may perform: `ssh` → `/endpoint`, `/prompt` and
SSH attach through the gateway; `http` → `/exec`, `/stream`, `/stat`,
`GET /api/leases/{id}`. The HTTP proxy does not consult shares: in
the capability model the lease id is the only credential, and under
forward-auth lookups are owner-scoped. `/stream` is an `http`
operation — an `ssh`-share grantee who calls it directly gets `404`
(the gateway's service token is the exception: the SSH gateway relays
session channels through `/stream`, so requests carrying it may attach
over an `ssh` share too). Response `201
Created` `{"shared":true,"lease_id":…,"grantee":…,"mode":…}`.
`400` unknown grantee; a non-owner gets `404 "lease not found"` —
the lookup is owner-scoped, so a lease you do not own looks the same as
a lease that does not exist.

There is no per-lease share listing. `GET /api/shares` lists every share
granted on the caller's leases — `{"shares": [{lease_id, grantee, mode,
created_at, expires_at?}]}` — which is what `share ls` prints.

### `DELETE /api/leases/{id}/share/{grantee}` — revoke (owner only)

`204 No Content` — the grantee loses access immediately. `404` if not shared.

Grantee access is enforced owner-scoped (`lookupWithShare`): a shared
lease behaves like the grantee's own for the granted operations until
expiry or revocation.

---

## Auth & ownership model

- Every request is authenticated by consumer token → consumer id, or by
  per-user token → identity user.
- Leases belong to the consumer/user that created them; operations check
  `owner == lease.Owner` (shares are the explicit exception).
- The SSH gateway authenticates users by SSH key (identity store
  authoritative when present) and calls the backend as that user
  (trusted impersonation via `X-Spoond-User-Id`, gated on the gateway's
  service token).
- Persistent leases survive TTL sweeps; `keepalive` extends them;
  `IDLE_TIMEOUT_SECS` can auto-suspend idle persistent leases.
- All lease state, shares, the pool and the image catalog persist in
  SQLite: a backend restart loses none of it.
- The lease id doubles as a capability (e.g. `GET /api/…/endpoint` and
  the proxy hostname are how a caller addresses someone's lease).
