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

Some errors carry a machine-readable `code` beside `error`, so a client
can branch on the code instead of matching the message text. A `409` in
particular means two different things depending on the code:

| Status | `code` | Meaning |
|---|---|---|
| `400` | `bad_request` | malformed or out-of-range request fields |
| `400` | `image_mismatch` | a create `image` does not match the `snapshot`'s image |
| `404` | `not_found` | unknown lease, name, snapshot or image |
| `409` | `lease_busy` | a suspend/resume/restart/checkpoint/save is already in flight, or another caller's resume is; retry |
| `409` | `lease_suspended` | a suspended lease on a path that cannot resume it (the stat probe, fork, crash test); `resume` it first. Every work call — exec, stream, files, proxy, jobs, the LLM gateway — resumes a suspended lease on use instead and never answers this. The body adds `"reason"` — `idle_suspend` or `take_back` when the suspension was automatic, `resume_failed` when an undrain could not bring the lease back and left it suspended with its snapshot intact for the next work call to retry (spoond-638d) |
| `409` | `lease_not_live` | a released lease where a live one is required |
| `409` | `cannot_start` | a snapshot build cannot run on this host; save it again |
| `409` | `save_in_progress` | a named-snapshot save with the same idempotency key is running |
| `409` | `secrets_in_use` | a background job with staged secrets blocks the save |
| `409` | `snapshot_in_use` | a live lease started from the named snapshot |
| `409` | `snapshot_limit` | the owner's `MAX_NAMED_SNAPSHOTS` cap is reached |
| `409` | `not_secret_free` | a named-snapshot version saved from a lease that had a secret, on a grant or a start through a grant; re-save it from a lease that never had one (see [Snapshot grants](#snapshot-grants)) |
| `409` | `kept_budget` | the owner's kept checkpoints and named snapshots would pass their fair disk slice (3.0; was `max_kept_bytes`) |
| `410` | `lease_lost` | the substrate lost the lease's sandbox; see [Lost leases](#lost-leases) |
| `429` | `box_full` | a request needed room and take-back can free none for it: every candidate is pinned (v3.0 FS5), or the requester is itself the biggest borrower — over its fair slice after the request with no other owner further over (FS2b); nothing was paused or released — unpin or release leases to make room. Answers a create, and (since the round-3 fix) any re-admission: resume, resume-on-use, restart, clone, fork, restore |
| `503` | `substrate_unavailable` | the substrate could not confirm the sandbox's state (the orchestrator was unreachable); retry; see [Substrate unavailable](#substrate-unavailable) |
| `500` | `scrub_failed` | a named-snapshot save could not scrub `/run/secrets` |
| `500` | `internal` | an internal failure |

A resume-on-use work call that finds no room on the host answers `503`
with `code: capacity_wait` and a `Retry-After` header, identically on
every path (exec, exec stream, files, proxy, jobs, the LLM gateway). Per
the owner contract (2026-10-08) a **host-structural** shortage — disk,
hugepages, the snapshot store, orchestrator capacity —
never refuses for good: spoond acts (reclaims or FIFO-cleanup) and the
caller waits. There are no per-owner caps since 3.0 (fair shares): the
only refusals for room are `429 box_full` (take-back can free nothing)
and `409 kept_budget` (the owner's disk slice).

### Refusals

Everything that refuses a request in 3.0:

| Status | `code` | When |
|---|---|---|
| `400` | `bad_request` (or a plain `error`) | validation: malformed JSON, out-of-range or unknown values, `image_mismatch` |
| `401` | | no or unknown token |
| `403` | | not allowed: an admin route, a share that does not grant it, a port the guest reserves, or `owner deleted` (the owner's identity was removed while the request was in flight) |
| `404` | `not_found` | unknown lease, name, snapshot, build, image or user — and any lease the caller cannot see |
| `409` | `lease_busy` | another lifecycle operation (pause, resume, restart, checkpoint, save) is in flight; retry |
| `409` | `kept_budget` | a keep or named save would push the owner's kept + named bytes past their fair disk slice |
| `409` | `lease_suspended` | a suspended lease on a path that cannot resume it (stat probe, fork, crash test, guest heartbeat) |
| `410` | `lease_lost` | the substrate lost the lease's sandbox |
| `429` | `box_full` | a request needed room and take-back can free none: every candidate is pinned, or the requester is itself the biggest borrower; nothing was paused or released |
| `503` | | `draining`: a planned restart is draining the node; `Retry-After` says when to come back |
| `503` | `capacity_wait` | no room yet (hugepages, snapshot disk, orchestrator capacity) and take-back cannot close the gap now; carries `Retry-After: 30`; a wait, never a loss. The same shape on every path (create, resume, restart, clone, fork, restore, checkpoint, named save, files); a create can wait it out with `wait` |
| `503` | `substrate_unavailable` | the orchestrator could not confirm the sandbox's state; carries `Retry-After: 5` |

Smaller limits keep their own `409`/`429` answers (`snapshot_limit`,
the per-lease kept cap, the per-owner exec, dial and file-transfer
concurrency caps, the auth throttle); see each route.

#### Changes from 2.9

The per-owner caps `max_leases`, `max_mib`, `guaranteed_mib` and
`max_kept_bytes` are gone, and with them every `429` they caused
(`quota_exceeded` and the plain over-quota `429`). Per path:

| Path | 2.9 | 3.0 |
|---|---|---|
| create (`POST /api/leases`) | `429` over `max_leases` or `max_mib` (waitable with `wait`); `503 capacity` when the node is full | no per-owner refusal; `429 box_full` when take-back can free nothing; otherwise `503 capacity_wait` with `Retry-After` (waitable with `wait`) |
| `POST /api/leases/{id}/resume` | `429 quota_exceeded` + `Retry-After` over `max_mib`; `503 capacity_wait` | `429 box_full`, or `503 capacity_wait` + `Retry-After` |
| resume-on-use work calls (exec, stream, files, dial, proxy, jobs, LLM gateway, network, prompt) | `429 quota_exceeded` + `Retry-After` over `max_mib`; `503 capacity_wait` | `429 box_full`, or `503 capacity_wait` + `Retry-After` |
| `POST /api/leases/{id}/restart` (suspended lease) | `429` over `max_mib` | `429 box_full`, or `503 capacity_wait` + `Retry-After` (was `500` on a capacity failure) |
| `POST /api/leases/{id}/clone` | `429` over `max_leases` or `max_mib`; `503 capacity` | `429 box_full`, or `503 capacity_wait` + `Retry-After` (was `503` without a code or `Retry-After`) |
| `POST /api/leases/{id}/fork` | `429` over `max_leases` or `max_mib` (all children reserved up front); `503 capacity` | `429 box_full`, or `503 capacity_wait` + `Retry-After` (was `503` without a code or `Retry-After`) |
| `POST /api/leases/{id}/restore` (suspended lease) | `429` over `max_mib`; `503 capacity` | `429 box_full`, or `503 capacity_wait` + `Retry-After` (was `503` without a code or `Retry-After`) |
| `POST /api/leases/{id}/checkpoint` `{"keep":true}` | `409 kept_budget` over `max_kept_bytes` (no budget when unset) | `409 kept_budget` over the owner's fair disk slice (no budget while the box capacity is unknown); `429 box_full` or `503 capacity_wait` when the checkpoint does not fit the snapshot disk (FS3) |
| `POST /api/leases/{id}/snapshots` (named save) | `409 kept_budget` over `max_kept_bytes` | `409 kept_budget` over the owner's fair disk slice; `429 box_full` or `503 capacity_wait` when the checkpoint does not fit the snapshot disk (FS3) |

`POST /api/users/{id}/quota` still accepts the removed fields (no `400`)
and ignores them; see
[`POST /api/users/{id}/quota`](#post-apiusersidquota--set-the-ttl-cap-admin-only).

---

## Leases

### `POST /api/leases` — create a lease

Request:

| Field | Type | Default | Notes |
|---|---|---|---|
| `image` | string | *(required unless `snapshot`)* | image name; must have a current build in the catalog (`GET /api/images`). With `snapshot` it may be omitted; when given it must equal the snapshot's image (`400 image_mismatch`) |
| `snapshot` | string | *(none)* | start the lease from a named snapshot version instead of the image's current build: `name` (latest) or `name@v`. The version must exist for the caller (`404 not_found`). The lease is never served from the warm pool, its memory is the snapshot's `memory_mb` for admission, and it is stamped with the version so retention never drops it — see [Start from a snapshot](#start-from-a-snapshot). A version that cannot run on this host answers `409 cannot_start` |
| `snapshot_owner` | string | *(the caller)* | with `snapshot`: start from another user's named snapshot (user name or id) through a grant that user gave the caller — see [Snapshot grants](#snapshot-grants). Without a grant it answers `404 not_found`, the same as an unknown name; a version that is not secret-free answers `409 not_secret_free`. Without `snapshot` it is `400` |
| `ttl` | int | `DEFAULT_TTL_SECS` | seconds; capped at `MAX_TTL_SECS` and at the user's `max_ttl` |
| `persistent` | bool | `false` | not TTL-swept; supports keepalive, suspend/resume, checkpoint |
| `size` | string | *(image default)* | a size preset by name (`GET /api/sizes`, e.g. `small`, `medium`, `large`, `xlarge`). Not with `vcpu`/`memory_mb` (`400 invalid_size`) |
| `vcpu` | int | image default | the VM's vCPU count: `1` or even, at most `MAX_LEASE_VCPU`. Never charged: fair shares count memory only |
| `memory_mb` | int | image default | the VM's memory in MiB: even, at least the image's `min_memory_mb` (`GET /api/images?detail=1`), at most `MAX_LEASE_MEMORY_MB` and the box's VM memory. It is the lease's charge for admission and fair shares. `memory_mib` is accepted as its old name; sending both with different values is `400 invalid_size` |
| `network` | string | *(ignored)* | accepted for compatibility |
| `init_cmd` | string | *(ignored)* | accepted for compatibility |
| `network_policy` | string | `restricted` | `none` \| `lan` \| `internet` \| `restricted` |
| `egress_allowlist` | []string | *(empty)* | IPs/CIDRs/domains for `restricted`; also lease references (see below) |
| `expose_ports` | []int | *(none)* | guest TCP ports published for peer leases. Max 8; port 49983 (envd) is refused; duplicates and out-of-range ports are refused |
| `holder` | string | `""` | a plain label naming what holds the lease (a CI job, an orchestrator's flight, a person's scratch work), at most 128 printable characters. **No lifecycle effect since v3.0:** it never pins, never keeps the lease past its TTL and never protects it from take-back. Use `pinned` for that |
| `holder_url` | string | `""` | link to the holder; empty or an absolute `http(s)` URL of at most 512 characters |
| `hold_ttl` | int | *(removed)* | **removed in v3.0** (pins replace holds): accepted and ignored for one release, with a `Deprecation` header |
| `pinned` | bool | `false` | pin the new lease: spoond never pauses or deletes it before its own expiry. The TTL still applies; a pinned persistent lease stays until the owner releases it. `holder`/`holder_url` never pin. See [Pins and the paused-release clock](#pins-and-the-paused-release-clock) |
| `checkpoint_interval` | int | host default | the lease's own periodic checkpoint interval in seconds: `0` = never checkpointed by the loop; `60`–`604800` = seconds between periodic checkpoints. Omitted = the host default (`CHECKPOINT_INTERVAL_MINS`, itself `0` = never — see [Checkpoints](#checkpoints)). Anything else is `400` |
| `idle_suspend` | int | host default | the lease's own idle reclamation threshold in seconds: `0` = never; `60`–`604800` = suspend the lease after that long without activity (exec, stream, proxy, keepalive, guest heartbeat, files, guest port dial), resuming it on the next call. Omitted = the host default (`IDLE_SUSPEND_DEFAULT_SECS`, itself `0` = never — see [Idle reclamation](#idle-reclamation)). Anything else is `400`, and a non-zero value needs a persistent lease (`400`) — a non-persistent lease has nothing to suspend into |
| `wait` | int | `0` | seconds a create refused for a **waitable** reason may wait for room instead of failing (see [Queued admission](#queued-admission)). `0` keeps the immediate refusal; above `0` it is capped at `MAX_ADMIT_WAIT_SECS` (`0` disables waiting, the field is accepted and ignored); negative is `400` |
| `secrets` | object | *(none)* | `{name: value}` delivered as files under `/run/secrets` in the guest — see [Secrets](#secrets). At most 32 secrets and 64 KiB of values per request; names match `[A-Za-z0-9_.-]{1,64}`. Values are never stored, logged or returned: they live in the backend's memory for the lease's life and are lost on a backend restart |

A lease created at a chosen size (`size`, `vcpu` or `memory_mb`) keeps
it for life: pause, resume, restart, clone, fork, restore and recovery
all run it at that size. A lease created at the default size keeps it
through pause, resume, clone, fork, restore and recovery, and takes the
image's current default when a restart boots a fresh guest. A size with a warm build (the image default, or a
preset listed in the image's `warm_sizes`) starts in milliseconds from
its memory snapshot (`"start": "warm"`). Any other size boots the
image's root filesystem at that size in about a second (`"start":
"cold"`). A bad size answers `400` with `code: invalid_size` before
anything starts. A create with `snapshot` keeps the snapshot's size:
`size` or `vcpu`, or a `memory_mb` other than the snapshot's, is `400
invalid_size`.

A create whose lease is not persistent answers with a `Warning:`
header naming its TTL when the TTL was not given, or a pin or holder is
set, since neither keeps a lease past its TTL.

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
  "pinned": false,
  "generation": 1,
  "checkpoint_interval": 300,
  "idle_suspend": 0,
  "exposed": {"9042": "10.11.0.7:9042"}
}
```

`pinned` mirrors the create request (or the later pin route).

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
[Idle reclamation](#idle-reclamation). A pinned lease always reports `0`:
spoond never idle-pauses a pinned lease (FS5, pins don't pause), and its
stored value comes back when it is unpinned.

`address` is the lease's host-side address (no port). `exposed` maps
each published port to `<address>:<port>` — reachable from peers whose
egress policy permits it (see [Network policy](#network-policy)), never
from the LAN. The same map appears in `GET /api/leases`.

A create with `snapshot` answers with a `snapshot` object carrying what
it used, and `GET /api/leases/{id}` shows the same object:

```json
"snapshot": {"name": "spoond/warm", "version": 3, "owner": "u-…", "build_id": "…"}
```

`owner` is the version's owner: the caller, or the user who granted it
(see [Snapshot grants](#snapshot-grants)).

Errors: `400` bad policy/ports/memory/holder/secret fields, `400
image_mismatch` when an explicit `image` differs from the snapshot's,
`404` unknown image or unknown snapshot (`not_found`), `409
cannot_start` when the snapshot's build cannot start on this host (the
body is `snapshot <name>@<v> cannot start on this host (<cause>); save
it again`, and there is no retry loop),
`429 box_full` when take-back can free nothing (see
[Memory take-back](#memory-take-back)), `503 capacity_wait` with
`Retry-After: 30` (not enough free hugepage memory for the image, or the
node is not healthy). When the lease's hugepages do not fit, take-back
runs first (see [Memory take-back](#memory-take-back)). The lease's root disk must fit in
usable snapshot disk too (FS3): `429` with `code: box_full` when
take-back had nothing to reclaim, `503` with `Retry-After: 30` and
`code: capacity_wait` when take-back ran and the disk is still short.

### Memory take-back

Every lease is admitted against one hugepage memory pool. When a request
(create, fork, clone, resume, restart, restore, undrain) does not fit,
spoond takes memory back by pausing leases through the normal pause path
(a snapshot build; the memory continues on resume, so the generation does
**not** change). It picks the owner furthest over their fair slice
(usage/slice ratio), and within that owner the least recently used lease.
Pinned leases are never taken. The requester takes only from owners whose
ratio stays above its own after the request. Take-back runs one request
at a time.

A taken-back lease is suspended with reason `take_back`. It stays
suspended until its holder next uses it (#145 D2): the next work call
(exec, exec stream, files, proxy, jobs, the LLM gateway) resumes it, and
an explicit resume takes the same path. If the node still has no room the
call answers `503` `capacity_wait` with `Retry-After: 30` and the lease
stays suspended; spoond never marks it `lost` for that. Clients should
treat a taken-back lease as waiting rather than gone: just use it when
needed instead of deleting and recreating it.

Two refusals tell the caller what is in the way:

- `429` `box_full` — only pinned leases stand in the way (nothing
  unpinned can be taken), or the requester is itself the biggest
  borrower.
- `503` `capacity_wait` with `Retry-After: 30` — a structural shortfall
  that take-back cannot close.

The create fields `burst` and `priority` and the lease field `class` no
longer exist (a create that sends them has them ignored). The lease
field `preempted` stays for old clients and is always `false`.

### Queued admission

A create refused only because the node is full can wait for room instead
of failing: send `"wait": N` (seconds) on `POST /api/leases` (and its
`/api/sandboxes` alias). `wait` defaults to `0`, which keeps today's
immediate refusal exactly; a negative value is `400`.

Only these refusals are **waitable** (the request is held open and
retried):

- `503 capacity_wait` with `Retry-After: 30`, the host saturated (3.1+,
  #136): a create, clone or fork waits while the host's I/O or CPU
  pressure is over its target (`SATURATION_IO_FULL_PCT`,
  `SATURATION_CPU_SOME_PCT`). The error and the `queued` event name the
  resource, e.g. `host saturated (io 30%)`. Running leases, resumes and
  restores are never held by it.
- `503 capacity_wait` with `Retry-After: 30` — hugepages full and
  take-back could not make room (or the node is not healthy). The
  `queued` event names it `capacity`.

There are no per-owner caps to wait on since 3.0; `429 box_full` is a
refusal and answers at once.

Everything else answers at once: bad requests, auth failures and
unknown images are **not** waitable.

The wait is capped at `MAX_ADMIT_WAIT_SECS` (default `900`; `0` disables
waiting and the field is accepted and ignored — see
[operations.md](operations.md)). The queue lives in the backend process
and is lost on restart (a waiting client sees its connection go and
retries). Waiting creates are served in **fair-share order**: the owner
with the lowest fair-share ratio (usage over their equal slice, see
[Fair shares](#fair-shares)) first, an owner the fair-share view does
not know after every known owner, then FIFO. Each wake-up admits every queued create that fits,
so a smaller create may pass a larger one that still does not fit. The
order is re-read before every attempt; it is best-effort only when room
frees in the middle of a pass (a create tried just before may miss room
that one tried just after gets).
Admissions from the queue go through the normal admission path, so
take-back applies unchanged. The
queue is retried whenever capacity may have freed (a release, suspend,
resume or take-back) and on a 5 s tick.

Outcomes:

- **Admitted** — the usual `201` body, plus `waited_ms` (the wait in
  milliseconds).
- **Timed out** — the refusal the create would have got had it not
  waited: the same status, body and `Retry-After` (a structural capacity
  refusal times out as `503 capacity_wait` with `Retry-After: 30`), plus
  `waited_ms`.
- **Client gone** — the ticket is dropped; nothing is written.
- **Drain** — a drain that starts answers every queued create `503
  draining` at once.

The `queued` event names the refusal being waited out and the create's
place in the fair-share order at that moment, e.g. `capacity; position
2 of 3`; the lease id is allocated when the create is queued and the
created lease keeps it. On admission a `created` event follows as usual;
a wait that ends without a lease emits `timed_out` with detail `waited
Ns`, `client gone`, `draining` or `owner deleted` (the owner's identity
was removed while the create waited; the create is refused `403 owner
deleted`).

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
both `0` = never). Since v3.0 this is the **only** idle threshold: the
plain `IDLE_TIMEOUT_SECS` sweep and every held-lease rule are removed
(see [Pins and the paused-release clock](operations.md#pins-and-the-paused-release-clock)).
A **pinned lease is never idle-suspended** — its effective threshold
reads `0` whatever it and the host default say (the pin wins; the
stored value returns on unpin). For everyone else
the idle sweep suspends it through the normal pause path: memory and
hugepages are freed into a pause build, nothing is deleted, the
generation does not change.

Activity is what the sweep counts: exec, stream, proxy, keepalive, guest
heartbeat, files API and guest port dial all move `LastActive`. An idle
suspension records `last_action` `idle_suspend/suspend_idle` with
`last_action_at` (persisted) and emits an
`idle_suspended` event whose detail is `idle for <duration>`; the
`suspended` event and the lease carry `suspend_reason` `idle_suspend`
beside the pause build and time (#145 D6). A paused lease is released by
the **one clock** — `PAUSED_RELEASE_DAYS` (default 30) after its pause
date — unless a resume clears the date first.

**Resume on next use (#145 D2):** every work call resumes a suspended
lease first through the normal resume path (admission and take-back
apply) and then serves the call — whatever suspended it: the lease's own
`idle_suspend`, take-back, or the owner's hand suspend. This
is one rule for every kind of suspend, not just
`idle_suspend`. GET, status, events and SSE never resume, and neither
does a path with nothing to resume. `POST /resume` is the explicit
resume: it runs the same resume path as a work call and shares its
refusal shapes below, during a drain too (see
[`POST /api/leases/{id}/resume`](#post-apileasesidresume--start-from-the-snapshot));
the SSH gateway's resume on attach takes the same path.

While its pause or another caller's resume is in flight, a work call
answers `409` with `code: lease_busy` (retryable). A resume whose
re-admission take-back cannot serve answers `429` with `code: box_full`;
a resume that finds no room
on the host — including one during a planned-restart drain, which must
not resurrect a `Drained` lease the drain's quiesce wait depends on —
answers the one shared no-room shape, `503` with `Retry-After: 30` and
`code: capacity_wait`; a resume refused because the substrate is
unavailable answers the retryable `503 substrate_unavailable` with
`Retry-After: 5` (spoond-638d round 2); and a resume of a lost sandbox
answers `410 lease_lost`. The lease stays suspended
in every refusal. Resuming does not renew a hold: there is no hold any
more, only labels.

**Per-path status (resume-on-use).** Every path is listed; the
`capacity_wait` and `box_full` columns are the same on all of them:

| Path | Resumes on use | No room | Take-back frees nothing | In-flight pause/resume | Lost sandbox |
|---|---|---|---|---|---|
| `POST /api/leases/{id}/exec` | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| `GET /api/leases/{id}/stream` (exec stream) | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| `GET`/`PUT`/`POST`/`DELETE /api/leases/{id}/files/...` (read, write, stat, list, mkdir, remove) | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| `GET /api/leases/{id}/ports/{port}/dial` | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| the HTTP proxy (`<lease>-<port>.<suffix>`) | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| background jobs (`POST .../exec` `background:true`, `POST .../jobs/{job}/signal`) | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| the LLM gateway (`/llm/{lease-id}/...`) | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| `POST /api/leases/{id}/network` | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| `POST /api/leases/{id}/prompt` | yes | `503 capacity_wait` + `Retry-After` | `429 box_full` | `409 lease_busy` | `410 lease_lost` |
| `GET /api/leases/{id}` (lease detail) | **no** | `200` (suspended details returned) | `200` | `200` | `200` (carries `lost_at`/`lost_reason`) |
| `GET /api/leases/{id}/stat` (the stat probe runs an exec) | **no** | `409 lease_suspended` | `409 lease_suspended` | `409 lease_busy` | `410 lease_lost` |
| `POST /api/leases/{id}/fork` (needs a running source) | **no** | `409 lease_suspended` | `409 lease_suspended` | `409 lease_busy` | `410 lease_lost` |
| `POST /api/leases/{id}/crash-test` (nothing to crash) | **no** | `409` (suspended; plain message, no code) | `409` (suspended) | `409 lease_busy` | `410 lease_lost` |
| the guest heartbeat (`POST /lease/{id}/active`) | **no** (not a work call) | `409 lease_suspended` | `409 lease_suspended` | `204` (busy is not suspended) | `410`/`409` |

A path in the last four rows does not resume. The stat probe, fork,
the crash test and the guest heartbeat need a running guest and cannot
start one, so they answer `409 lease_suspended` with the structured
`"reason"` when the suspension was automatic; the lease detail is a
read and answers `200` for a suspended lease. No other path ever answers
`409 lease_suspended` for a suspended lease.

### `GET /api/leases` — list leases

Response `200 OK`: `{"sandboxes":[ {…lease…}, … ]}` where each row has
`id`, `owner`, `image`, `address`, `expires` (unix seconds),
`persistent`, `suspended`, `state`, `build_id`, `resume_build_id`,
`name`, `comment`, `holder`, `holder_url`, `net_policy`,
`egress_allowlist`, `exposed`, `generation`, `checkpoint_interval`
(effective seconds; `0` = never), `idle_suspend` (effective seconds;
`0` = never; **a pinned lease always reports `0`** — the pin wins over
the idle sweep, and the lease's stored value comes back on unpin),
`preempted` (always `false`) and `pinned`
(see [Memory take-back](#memory-take-back) and [Pins and the paused-release clock](operations.md#pins-and-the-paused-release-clock)).

Every row also carries the lease's own size (3.1, #151): `vcpu`,
`memory_mb` (what its VM runs with and what it is charged) and `disk_mb`
(its disk allowance), plus `start` (`"warm"` or `"cold"`, how its
current VM started; omitted after a backend restart until the next
start). Pause, resume, clone, fork, restore and recovery run a lease at
its own size even after its image is rebuilt at another size; a restart
keeps it too for a lease created at a chosen size, and otherwise takes
the image's current default (see [create](#post-apileases--create-a-lease)).
The size fields are omitted for an old lease whose image row is gone.

A pinned lease's row adds `pinned_idle_since` (RFC 3339) once its last
API activity passes `PINNED_IDLE_NOTICE_DAYS` (visibility only; cleared
when activity returns). A lease that was ever automatically executed
carries `last_action` (`"rule/action"`, e.g. `"idle_suspend/suspend_idle"`)
with `last_action_at` (RFC 3339). A paused lease carries `paused_at`
(RFC 3339, the one clock's start).

A suspended lease whose suspension was automatic also carries its
structured suspension facts (#145 D6): `suspend_reason` (`idle_suspend`
= the lease's own threshold, `take_back`), `suspend_policy_step` (the
pressure order's step name, omitted until that order names steps),
`suspend_build_id` (the pause build) and `suspended_at` (RFC 3339). A
hand or drain suspend has no automatic reason and carries none of the
four; the pause build is still `resume_build_id` and
`last_action`/`last_action_at` name the suspension. All four fields are
additive and omitted while unset, and are cleared on resume, restore,
cold restart and crash recovery.
`lease.held_action` is unchanged.

### `GET /api/leases/{id}` — lease detail
The same object as a list row plus `state`, `recovered_from` (RFC 3339 or
`""`), `last_checkpoint_at` and `kept_builds` — the checkpoints the lease
pinned with `{"keep":true}` (#126), oldest keep first. A lease in the
`lost` state also carries `lost_at` (RFC 3339) and `lost_reason` (the
cause the `lost` event reported; see [Lost leases](#lost-leases)):

```json
"kept_builds": [
  {"build_id": "<uuid>", "size_bytes": 83928702976, "kept_at": "2026-10-01T12:00:00Z"}
]
```

`size_bytes` is the recorded disk size (allocated bytes, #125), so the
list shows what unpinning would free. Requires the owner or an `http`
share.

The detail also carries the lease's **pause-chain size** (spoond-p9j):
`chain_depth` is how many builds its chain holds (the pause build it
would resume from, or the build it runs from, plus its ancestors up to
the template root) and `chain_bytes` is their summed recorded
`size_bytes`. A persistent lease that suspends repeatedly keeps one
memory snapshot per pause until a cold restart breaks the chain; these
fields show what that chain costs. A lease with no build yet omits both
fields.

A lease with an identity-store owner also carries its owner's memory
charge (#128): `charged_mib` (the owner's current running-lease charge —
what this lease contributes to while it runs), the same number as
`used_mib` on `GET /api/users/me`. `guaranteed_mib` and `max_mib` are
gone since 3.0.

`state` is `running`, `suspended`, `recovered` or `lost`. `recovered`
behaves exactly like `running` — it marks a lease the crash reconcile
resumed from a checkpoint, and keeps showing `recovered` until the lease
is suspended or restarted. `lost` means the sandbox died with no
checkpoint (or its crash-recovery retries ran out); a failed
resume-on-use never loses a lease, its error goes to the caller (#145
D2). The detail carries `lost_reason`,
the cause the `lost` event reported, and every call on the lease answers
`410` with `code: lease_lost` (see [Lost leases](#lost-leases)); the
lease should be deleted to free its quota. While a crash recovery is
retrying after a transient failure, the detail also carries
`recovery: {"attempt": N, "of": K, "since": "<RFC 3339>"}` — the
counted attempts so far, the limit and when the first failure happened.

A lost lease keeps its resume and checkpoint snapshots for a grace
period after the loss — 7 days for a persistent lease, 1 day otherwise
(`GC_LOST_GRACE_PERSISTENT` / `GC_LOST_GRACE`) — so the owner can still
reclaim them. Once that grace period lapses the GC releases the lease
itself through the normal release path: the concurrent-lease slot it
held comes back, its snapshots become ordinary
GC candidates, its running jobs are settled and a `released` event with
the reason `lost_expired` reaches the owner's event stream. The release
is automatic and idempotent, so a `lost` lease the owner has moved on
from does not hold their quota forever. Deleting a lost lease with
`DELETE /api/leases/{id}` does the same thing immediately and frees the
quota sooner.

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

Alongside it, every path that (re)creates a guest — create, resume,
restart, cold restart, restore, crash recovery, fork and clone — writes
the current lease id to `/run/spoond/lease-id` (`0644`, one line,
`"<uuid>\n"`). A process captured in a memory snapshot keeps the source
lease's `SPOOND_LEASE_ID`; a long-running process should re-read this
file to notice that it is now in a different lease. Both files are
written atomically (a temp file, then a rename), so an inotify watcher
on `/run/spoond` sees each whole write. See
[Identity in a restored guest](#identity-in-a-restored-guest).

Processes in the guest read the file to notice that their memory did
not continue, and re-derive whatever they keep only in process (caches,
locks, half-finished work):

```bash
cat /run/spoond/generation   # e.g. 2
cat /run/spoond/lease-id     # e.g. 8f3a1c…
```

### Lost leases

A lease becomes `lost` when the crash reconcile finds its sandbox gone
and cannot recover it: it had no checkpoint, or its recovery from one
failed. The reconcile emits a `lost` event whose `detail` is the reason
(e.g. `no checkpoint to recover from; the running state is gone`,
`recovery from checkpoint <build> failed: <err>`, or `root disk
unreadable (I/O errors)` when the rootfs probe found the disk dead) and
stamps the same text as `lost_reason` (persisted, migration 0019).

A lost lease's guest is stopped; `DELETE` frees the quota. Every path
that marks a lease `lost` (crash recovery, undrain, the rootfs probe)
deletes the lease's sandbox through the substrate, retrying a few times
so a create or resume that failed after its VM started cannot leave a
guest running. The sandbox is gone for good and the lease's quota is
still charged. A delete that still fails is left to the periodic orphan
sweep, which treats a sandbox whose lease is `lost` or released as an
orphan, and also deletes any substrate sandbox no lease and no pool
entry claims once it has been seen unclaimed on two consecutive passes;
a creation's sandbox is never swept while the creation is in flight.
A failed **resume-on-use** never loses a lease (#145 D2):
its error goes to the caller (the shared `429 box_full`/`503
capacity_wait`/`409 lease_busy` shapes) and the lease stays suspended with its snapshot
intact. Undrain's bounded resume no longer marks a lease lost for a
retryable or indeterminate failure either (spoond-638d): a resume that
exhausts its retries is left suspended with reason `resume_failed` and
its `drained` flag, so resume-on-use and the self-heal loop retry it.
Only a permanent error (the build the resume needs is gone) loses the
lease there, and only a lease still suspended and not busy is lost: a
resume an owner has in flight saves its guest. A
create that finishes after its lease was released stops the fresh guest
and saves nothing, so a release is never undone by a late recovery,
resume, restart or restore.
Every call that acts on it — exec, background exec, files, guest dial,
stream, proxy, stat, resume, restart, suspend, keepalive, checkpoint,
snapshot save, restore, crash-test, clone, fork, tag, comment, holder,
checkpoint-policy, idle-policy, network, share — answers `410 Gone`
with `code: lease_lost`, the reason and the way out (`409` keeps meaning
"busy, retry"):

```json
{
  "error": "the substrate lost this lease's sandbox: no checkpoint to recover from; the running state is gone; DELETE the lease to free its quota",
  "code": "lease_lost"
}
```

`GET /api/leases/{id}` returns `state: "lost"` and the `lost_reason`
field, so a client can show the cause. `DELETE /api/leases/{id}` frees
the lease's quota (the snapshot builds a kept build pinned follow the
GC's normal grace period). A lost lease's guest is stopped when it
becomes lost, so `DELETE` only has to release the lease's quota; it
does not have to stop a running sandbox.

### Substrate unavailable

Before a lease is called `lost`, spoond confirms the sandbox really is
gone by listing the orchestrator's sandboxes. When an operation fails
(an envd stream drop, an exec error, a file-path failure) and the
orchestrator `List` *also* fails — an orchestrator stall or restart —
the state is **unknown**, not absent. The e2b client retries `List` a
bounded number of times with a short backoff and then answers
`503 Service Unavailable` with `Retry-After: 5` and code
`substrate_unavailable`:

```json
{
  "error": "the substrate could not confirm the sandbox state (orchestrator unreachable); retry shortly",
  "code": "substrate_unavailable"
}
```

This is deliberately not `410 lease_lost`: `410` is final for clients
(Honey treats it as gone with no confirming GET, and the runner fails
the job permanently), and an orchestrator stall must never become one.
The lease is not marked lost on this answer. A `List` that succeeds and
does not name the sandbox is still a confirmed absence and answers
`410` (the body is `{"error":"lease no longer exists"}` with **no**
`code`; only a lease that is already marked `lost` carries
`code: lease_lost`), or `404` on files.

On the e2b backend only `Start`, `Exec` and `WriteFile` call the
sandbox-list check, so the paths that can actually answer
`substrate_unavailable` are exactly the ones in the table below. The
other routes (`dial`, job read/output, the file `GET`/`stat`/`mkdir`
branches) keep a defensive `ErrUnavailable` branch, but on e2b their
calls do not fail into it today, so it is not documented as a live
status change.

#### Status changes for the substrate-unknown path

"Unknown" means the operation failed *and* the confirming `List`
failed; the confirmed-absence answer is unchanged in every row.

| Path | Confirmed absent | Unknown (orchestrator `List` failed) |
|---|---|---|
| `POST …/exec` (`api/server.go`, `handleExec`) | `410 lease no longer exists` (no code) | `503 substrate_unavailable` |
| `GET …/stat` (`api/server.go`, `handleStat`) | `410 lease no longer exists` (no code) | `503 substrate_unavailable` |
| `POST …/exec` `background:true` (`api/jobs_http.go`) | `410 lease no longer exists` (no code) | `503 substrate_unavailable` |
| `PUT …/files/{path}` and directory remove (`api/files.go`) | `404 file not found` | `503 substrate_unavailable` |
| `POST …/jobs/{job}/signal` (`api/jobs_http.go`) | `409 job is not running` | `503 substrate_unavailable` |
| `GET …/stream` (`api/server.go`, `handleStream`) | `error` frame | `error` frame naming `substrate: unavailable` |

The exec-stream path has already committed a protocol (a WebSocket
upgrade) by the time the substrate answers, so it cannot return a
status; it surfaces the new error text instead. Recovery and the
lost-lease guard are unaffected: a lease is never marked lost on an
unknown, and a recovery that fails with `substrate.ErrUnavailable` is a
transient failure and gets the existing bounded retry (`api/retry.go`).

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
through the `/api/sandboxes` alias. Every create, release, lost and
suspend also writes one durable journal line (see
[operations.md](operations.md#the-lease-journal)) that outlives the
event ring and the lease row.

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

`409` if the lease is suspended and the path cannot resume it. Every
work path — and this one is work — resumes a suspended lease on use
(#145 D2) first, then serves the exec: whatever suspended it (the lease's
own `idle_suspend`, take-back, or a hand suspend). A refused
resume answers the shared shapes: `429`
`box_full` when take-back can free nothing, `503 capacity_wait` with `Retry-After` when the host has
no room, `409 lease_busy` while its pause or another resume is in
flight. `410` if it is
`lost` (`code: lease_lost`, see [Lost leases](#lost-leases)) or the
sandbox no longer exists on the substrate; `413` when the JSON body is
larger than `MAX_EXEC_BODY_BYTES` (default 8 MiB, leaving room for a
large `cmd` and `env` next to the 64 KiB secrets cap); `429` when
the per-owner concurrent exec/stream cap is reached. (Exec does not wait for or take the lease's lifecycle lock; its concurrency
guard is the per-owner cap, which yields `429`. A busy lease shows up only
as the `409` below.)

The body cap bounds what the host buffers, not what the guest can run:
the guest's own argv limit is about 128 KiB per string, so a `cmd`
larger than that fails with `Argument list too long` from the guest
shell (exit non-zero) rather than being refused here.

While a lifecycle operation is in flight on the lease (the periodic
checkpoint, a suspend or a restart), the orchestrator briefly reports
its sandbox missing; for a large guest a checkpoint can take a couple of
minutes. Exec, stat, guest dial and the file routes then answer `409`
with `Retry-After: 5`, not `410`: retry. `410` means the sandbox is gone
with nothing in flight (a lease marked `lost` answers `410` with
`code: lease_lost` and its reason instead). When spoond cannot reach the
orchestrator to confirm the sandbox is gone, the answer is `503` with
`code: substrate_unavailable` and `Retry-After` — see
[Substrate unavailable](#substrate-unavailable); the lease is kept.

#### Background exec (2.6, #135)

Add `"background": true` to the exec body and the command runs as a
tracked job instead of holding the request open. The other fields keep
their meaning (`cmd`, `cwd`, `env`, `secrets`); `timeout` is ignored —
a background job runs until it exits, is signalled, or the lease does.
The optional `max_runtime_secs` shortens the host's `JOB_MAX_RUNTIME`
(default 24 h) for this job only; it can never make a job run longer
than the host cap and a negative value is `400`. It applies to a
background job; a synchronous exec ignores a non-negative value, but a
negative value is still `400` so a client cannot think it changed the
exec's timeout.

Response `202 Accepted` as soon as the process has started:

```json
{"job_id": "<hex>", "started_at": "2026-10-05T12:00:00.123456789Z"}
```

Without `background` nothing changes. A suspended lease resumes on use
first (the background exec is a work call, #145 D2), then the job starts;
a `lost` lease answers `410` with `code: lease_lost` (see [Lost
leases](#lost-leases)). At most `MAX_RUNNING_JOBS_PER_LEASE`
(default 16) jobs may run at once per lease; past it the request answers
`429`.

The command runs in the caller's lease and is detached from the envd
stream, so a backend restart does not kill it. The guest records the
outcome itself under `/var/lib/spoond/jobs/<job_id>/`: `stdout`,
`stderr`, `pid`, and `rc` (written atomically when the command ends).
Those files, not the stream, are the source of truth, and they are kept
as long as the record — they are removed when the exited record is
pruned (`JOB_RETENTION_SECS`). The `reconcile` pass also enforces the
max runtime: a job that has run for its effective cap is killed first
(the job's process group, like `POST .../signal`) and only then marked
exited with reason `timed_out` and exit code `124`, and a `job_exited`
event names the cap, so a `sleep infinity` cannot pin the lease's memory
and hugepages forever. If the kill fails (a transient substrate error)
the record stays running and the next reconcile retries, so a job that
cannot be signalled is still tracked and counted. If the kill succeeds
but the store write that marks it timed out fails, the in-memory intent
is kept and the next reconcile records the outcome without signalling a
gone process; a job whose `pid` file never appears is retried on a
backoff rather than spending the pid wait on every pass. The cap is wall-clock
from the job's start; while the lease is suspended — or busy with an
in-flight pause, resume, restart or restore — reconcile leaves the job
running (the guest cannot be signalled), and the first reconcile after a
resume kills a job whose cap was spent in the meantime. A record written
before the cap existed (`max_runtime_secs` 0) is still capped by the
current host value once the backend is upgraded. `JOB_MAX_RUNTIME=0`
(unset) is the 24 h default and a negative value disables the cap; the
cap is held as whole seconds, so a positive fractional duration rounds
up to the next whole second (`500ms` is 1 s, not a 0 that reads as the
default, and `1500ms` is 2 s, not a truncated 1 s) and a negative value
under one second is rejected at startup because it would round to 0 and
mean the default rather than "off". A bare integer too large to
represent as a duration is clamped at startup rather than wrapping.
Per-exec `secrets` stay staged under
`/run/secrets` for the job's life and are removed when it exits (the
guest wrapper removes them; the backend also removes them on
reconcile). Neither `env` nor secret values are ever stored in the job
record, logged, sent in an event or written to the guest's job
directory — `env` rides the substrate's start request — and `cmd` is
stored as given (put credentials in `env` or `secrets`, not argv).

See [`GET /api/leases/{id}/jobs`](#get-apileasesidjobs--list-background-jobs)
for reading and controlling jobs.

### `GET /api/leases/{id}/jobs` — list background jobs

Lists the lease's background jobs, newest first:

```json
{"jobs": [
  {"job_id": "…", "lease_id": "…", "owner": "…", "cmd": "…", "cwd": "",
   "state": "exited", "exit_code": 7, "started_at": "…",
   "ended_at": "…", "stderr_tail": "…"}
]}
```

`state` is `running`, `exited` or `lost` (the guest's memory did not
continue — a cold restart, restore, crash recovery or generation bump).
`exit_code` is `null` while running. `stderr_tail` is the last 4 KiB of
stderr. `reason` is `timed_out` when the max runtime, not the command,
ended the job, and omits otherwise. `max_runtime_secs` is the effective
cap the job runs under, in whole seconds, and is omitted when 0 (the
host cap is disabled). Owner, admins and `http` shares as exec has them.

### `GET /api/leases/{id}/jobs/{job}` — read one job

Returns the record plus the last 64 KiB of `stdout` and `stderr`:

```json
{"job": {…}, "stdout": "…", "stderr": "…"}
```

The `stdout` and `stderr` keys are always present (empty when the guest
has no output or the sandbox is not reachable, e.g. a suspended lease);
on a non-fatal read failure an extra `stdout_error` / `stderr_error`
key marks it rather than dropping the field.

With `?wait=<seconds>` (at most 900) the request long-polls until the
job is no longer running or the wait ends, then answers the current
record. This is the easy way to follow a job to completion.

### `GET /api/leases/{id}/jobs/{job}/output` — stream job output

Returns raw bytes from one stream so a client can follow output:

| Query | Default | Notes |
|---|---|---|
| `stream` | `stdout` | `stdout` or `stderr` |
| `offset` | `0` | byte offset |
| `limit` | `1048576` (1 MiB) | capped at 16 MiB |

### `POST /api/leases/{id}/jobs/{job}/signal` — signal a job

```json
{"signal": "TERM"}
```

`TERM` or `KILL`; signals the job's process group. `200` on success,
`409` when the job is not running.

Background jobs emit `job_started` (detail: the command, cut to 120
chars), `job_exited` (detail: `exit <code>` and the last 10 stderr
lines, at most 1 KiB; for a job killed by the max runtime the detail
starts `timed out: exit 124`), and `job_lost` on the lease's event
stream. The
lease object (`GET /api/leases/{id}` and every list row) carries a
`jobs` field:

```json
"jobs": {"running": 1, "last_exit": {"job_id": "…", "exit_code": 7, "ended_at": "…"}}
```

`last_exit` is `null` when the lease has no exited job. A lease with a
running job counts as active for the idle_suspend threshold, so nothing suspends it
mid-job.

### `…/api/leases/{id}/files/{path…}` — lease files

Read and write files inside the lease's filesystem. `{path…}` is the
guest-absolute path (everything after `/files/`); it is cleaned before
use, so `…/files/a/../b` is `…/files/b`, and `..` can never escape the
guest root — a path that cleans to `/` names no file and answers `404`.
Access follows the strictest lease model: the **owner** (or an admin,
who may act on any lease); a grantee's `http` share does **not** carry
file content, and anyone else gets the usual `404`. Every call counts
as activity for the idle sweeper. A suspended lease — whatever suspended
it — is resumed first on every file route and then served (#145 D2); a
refused resume answers the shared shapes (`429 box_full`, `503
capacity_wait` with `Retry-After`, `409 lease_busy`). A `lost` lease
answers `410`
with `code: lease_lost` (see [Lost leases](#lost-leases)). File
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

The first message is bounded to `MAX_EXEC_BODY_BYTES` (the exec body
cap; default 8 MiB), as is every later message, so a client cannot make
the server buffer an unbounded argv/env/secrets frame or stdin chunk.
An oversize frame closes the WebSocket with close code `1009` (message
too big) instead of being buffered whole.

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
send `stop` or `kill` for that. A stream attach — a work call — resumes a
suspended lease first, whatever suspended it (#145 D2), and then starts
the process; a refused resume answers the shared shapes (`429
box_full`, `503 capacity_wait` with `Retry-After`, `409
lease_busy`).

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
`429 box_full` and `503 capacity_wait` with `Retry-After` if a suspended
lease cannot resume on use (a dial is work, #145 D2), `409 lease_busy`
while its pause or another resume is in flight; `410` with
`code: lease_lost`
for a lost
lease (see [Lost leases](#lost-leases)), `429` when the owner's 16
concurrent dials are already open, `502` when the lease has no running
sandbox or the guest port refuses the connection.

### `POST /api/leases/{id}/keepalive` — extend a persistent lease

Request `{"ttl": <seconds>}` (0 = `MAX_TTL_SECS`; capped). Response:
`{"id":"…","persistent":true,"expires_at":"…"}`. `400` if the lease is
not persistent.

### `POST /api/leases/{id}/suspend` — snapshot + stop

Persistent leases only (`400` otherwise). The sandbox is paused into a
new build and stops; the lease stays, becomes `state: "suspended"`, and
records `resume_build_id`. The snapshot must fit in usable snapshot
disk (FS3): `429` with `code: box_full` when take-back had nothing to
reclaim, `503` with `Retry-After: 30` and `code: capacity_wait` when
take-back ran and the disk is still short — in both the lease stays
running as it was. A pause is charged only **beyond its own `disk_mb`**:
the lease's reservation is released by the pause, so a lease whose own
allowance covers its snapshot pauses even when usable disk is negative
and nothing can be taken back. An **admin drain is never refused for
disk**: a `POST /api/admin/drain` pauses leases through the normal path
without a disk admission, regardless of how full the snapshot is. When
the pause does take back a lease, the released bytes are charged to the
**victim's owner** (the owner further over their slice), not to the
pausing caller. Response
`{"id":"…","status":"suspended","message":"lease suspended; state snapshot kept (resume to restore)"}`.

### `POST /api/leases/{id}/resume` — start from the snapshot

Restores a suspended lease from `resume_build_id` **with the same sandbox
id**, so its address and identity are unchanged. Owner only (admins
too); the SSH gateway's service token may resume any lease before a
session starts. Response
`{"id":"…","status":"running","address":"…"}`. `409` with
`code: lease_busy` if the lease is busy (another lifecycle
operation is in flight; this is how a pause or another caller's resume
answers, and the already-suspended body no longer uses
`lease_suspended` anywhere a resume is possible).
While spoond is draining for a planned restart (or still owes a drain
clear), `POST /resume` answers `503` with `Retry-After: 30` and `code:
capacity_wait` and the lease stays suspended; the SSH gateway's resume
on attach takes the same path, so neither can race a resume into a
node that is stopping.
A suspended lease holds no hugepages, so resuming one re-admits its
memory like a create. A resume that finds no room on
the host answers `503` with `Retry-After: 30` and `code: capacity_wait`
(one shape for every no-room refusal, whether the
snapshot disk, or the orchestrator's
capacity) — the lease stays
suspended. A resume whose re-admission cannot get room because
every take-back candidate is pinned, or because the caller is itself
the biggest borrower, answers `429` with `code:
box_full` (the create body's refusal, not a 500) — the lease stays
suspended, nothing was paused or released. The same `429 box_full`
answers a resume whose root disk does not fit in usable snapshot disk
with nothing left to reclaim, and the `503 capacity_wait` one whose
take-back ran and the snapshot disk is still short (FS3). The resume
Resuming a lease that is
already running does nothing and answers `200`
with the lease as it is: the guest keeps its memory. (Before 2.1.2 it
restored the pause build again, rolling the guest's memory back.) An
`owner deleted` (`403`) refuses the resume when the owner's identity was
removed while the resume was in flight (spoond-q4j). A suspended lease
the caller owns resumes whatever suspended it; being persistent or
pinned is not required (v3.0).

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
re-admits its memory like a resume: `429 box_full` when take-back can
free nothing — the lease stays suspended, untouched. A substrate failure on the fresh-guest path (which does create a sandbox) surfaces
as `500`, not `503` — unlike create, fork and clone, restart does not
map capacity errors to `503`.

An `owner deleted` (`403`) refuses the restart when the owner's identity
was removed while the restart was in flight (spoond-q4j).

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

The checkpoint must fit in usable snapshot disk (FS3): the write waits
until take-back has made room, and a disk that stays short answers
`429` with `code: box_full` (nothing reclaimable) or `503` with
`Retry-After: 30` and `code: capacity_wait` — in both nothing was
written and the lease keeps running as it was.

**Keep limits (#126).** Two caps govern `keep`:

- **Per lease** — `MAX_KEPT_PER_LEASE` (default `4`, `0` = no cap). A
  keep on a lease already holding that many kept builds answers `409`
  `{"error":"kept checkpoint limit reached (4 per lease); unpin one with
  DELETE /api/snapshots/{build_id}"}`. Nothing is evicted and **no
  checkpoint is taken**; unpin a build to free a slot.
- **Per owner** — the owner's fair **disk slice** (3.0; it replaced
  `max_kept_bytes`): the owner's kept builds may not pass it. The
  build's size is only known after it is written, so an over-budget
  keep **takes the checkpoint** and then refuses the pin: `409` naming
  the budget with the new build's `build_id` and `"kept":false`. While
  the box capacity is unknown (`capacity_known: false`) there is no
  budget. The build stays an ordinary,
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
crash answers `410` with `code: lease_lost` like every other route
(restore does not resurrect it; see [Lost leases](#lost-leases)); `409`
while another operation is in flight; a substrate capacity
failure maps to `503 capacity_wait` with `Retry-After: 30`. Restoring a suspended lease brings a running
sandbox (and its hugepages) back, so it re-admits its memory like a
resume: `429 box_full` when take-back can free nothing — the lease
stays suspended, untouched. An `owner deleted` (`403`) refuses the restore
when the owner's identity was removed while it was in flight
(spoond-q4j): the user is gone, so the restore is not run ownerless.

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
{"id":"…","build_id":"<uuid>","generation":2,"status":"running","build_created_at":"2026-10-06T09:12:30Z"}
```

**Restore to the start snapshot (#141).** A lease started from a named
snapshot may also restore to that version's build — its
`snapshot.build_id` — whoever owns the version, so a lease started
through a [grant](#snapshot-grants) can go back to the shared warm-up
without a new lease. No other lease may use that build: not a clone or
fork of the started lease (they do not inherit the start), and not any
lease of the owner that did not start from it. A revoked grant does
not stop it; the lease already runs from the version. On top of the
usual restore, the fresh guest is prepared as a snapshot start prepares
a copy: `/run/spoond/lease-id` and `/run/spoond/started-from` name this
lease and the version, and everything under `/run/secrets` is removed
before the lease's own create-time secrets are re-written. If either
marker cannot be written or the scrub fails, the restore answers `500`
and the lease stays on its old sandbox.

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
set; a pinned lease reads `0` — the pin wins, and the stored value
comes back on unpin). The change emits an `idle_policy` lease event naming the new
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

A checkpoint, pause, resume or restore that is still running when its
lease is released does not resurrect the lease: the released lease's row
is not written back, the sandbox the operation created is stopped, and
the build it wrote (a checkpoint or pause build) is left unreferenced for
the snapshot GC to reclaim (spoond-775). A backend start reloads every
stored lease, so a row an older binary left from such a race is loaded
as an ordinary live lease and goes through the lost/grace path rather
than being dropped.

### `POST /api/leases/{id}/crash-test` — crash a lease and recover it

**For testing crash recovery.** The route exists only when the host
sets `CRASH_TEST=1` (see [operations.md](operations.md#crash-test));
otherwise it answers `404 {"error":"not found"}` like an unknown route,
whoever calls it. Owner or admin: the owner may crash their own lease,
an admin any lease; anyone else gets the usual `404` `lease not found`.
No body.

It deletes the lease's sandbox through the substrate directly — as a
crash would, without releasing the lease or emitting `released` —
drops the sandbox row, then runs the same per-lease recovery the
backend runs after a real crash (see [operations.md](operations.md#crash-recovery)):
with a checkpoint the lease comes back from its newest one (generation
+1, state `recovered`, event `recovered`; files newer than the
checkpoint are gone); without one it is marked `lost` (event `lost`)
and answers `410` with `code: lease_lost` from then on. A `crash_test`
event comes first, with
the detail `crashed by its owner` or `crashed by an admin`, so a reader
of the event stream can tell a test from a real crash.

It touches only that one lease: no other lease, no warm-pool sweep, no
peer refresh and no release. The recovery runs to the end even if the
client hangs up. `409` while another operation is in flight or while
the lease is suspended (nothing is running to crash; this is one of the
few paths that cannot resume, so it answers `409 lease_suspended` with
the reason), `409` with `code:
lease_lost` for a lease
already lost, `404` for an unknown or released lease. Response `200`:

```json
{"id":"…","result":"recovered","generation":2,"state":"recovered"}
```

`result` and `state` are `lost` (and `generation` unchanged) when there
was no checkpoint. A recovery that fails transiently answers
`result: "recovering"` with the lease's current state: the crash test is
single-shot, while the background reconcile keeps retrying under its
bounded budget (spoond-dxq).

### `POST /api/leases/{id}/clone` — branch to a new lease

Checkpoints the running sandbox and grants a fresh **persistent** lease
from the checkpoint build, copying the source's network policy and
exposed ports. The optional body `{"tag":"…"}` is accepted and ignored —
the checkpoint build id is the snapshot's identity (use `tag` for a
friendly *name*). Response `201 Created`:

```json
{"id":"…","image":"…","source":"<source-id>","branch_tag":"<build id>","persistent":true,"expires_at":"…"}
```

The clone admits the source image's `memory_mb` like any create:
`429 box_full` when take-back can free nothing, `503 capacity_wait` with
`Retry-After: 30` when the node has no room. An `owner deleted` (`403`) refuses the clone when the owner's identity
was removed while the clone's checkpoint was in flight (spoond-q4j): the
fresh sandbox is stopped and no lease is committed, so a deleted owner
cannot acquire an ownerless copy.

### `POST /api/leases/{id}/fork` — N copies of a running lease

Owner only, as clone (shares are not honoured). Checkpoints the source
once and creates `count` leases from that build, each its own lease
owned by the caller, with the source's policy copied. Their memory is
reserved for all of them up front (all or nothing); the batch's memory
is admitted at once, before any child is created, so a refusal leaves
no earlier child and no other owner's take-back victim behind; if any
create fails, every lease created in the call is deleted.

Request `{"count":1..20,"persistent":false,"ttl":300,"holder":"ci-job"}`
(`ttl` 0 = the default TTL, capped at the maximum and the user's
`max_ttl`). The optional `holder` and `holder_url` fields stamp every
created lease as plain labels (same validation as create); they never
pin. `hold_ttl` is removed (accepted and ignored for one release, with
a `Deprecation` header).

Response `201 Created`:
`{"source":"<id>","build_id":"<uuid>","ids":["…","…"]}`.
Errors: `400` bad count or bad holder fields, `404` unknown, `409`
suspended or busy, `429 box_full` when take-back can free nothing for
the batch (each fork costs its image's `memory_mb`), `503 capacity_wait`
with `Retry-After: 30`. An `owner deleted` (`403`) refuses a fork whose owner's identity was
removed while the fork's checkpoint was in flight (spoond-q4j): every
child created so far is rolled back (its sandbox stopped once) and the
rest are refused, so a deleted owner cannot acquire ownerless copies.

### `POST /api/leases/{id}/network` — change egress policy live

Owner only. Updates the lease's policy and allowlist, re-applies the
egress config to the running sandbox, and refreshes every peer's
allowances — no restart, no new lease.

Request `{"network_policy":"none|lan|internet|restricted","egress_allowlist":[…]}`.
Response `200` `{"id","network_policy","egress_allowlist"}`. `400` on an
invalid policy, `404` unknown. A suspended lease resumes on this work
call (#145 D2) before the policy is applied; a refused resume answers
the shared shapes (`429 box_full`, `503 capacity_wait` with
`Retry-After`, `409 lease_busy`).

### `POST /api/leases/{id}/tag` — friendly name

Request `{"name": "<unique-per-owner-name>"}`. Response
`{"id":"…","name":"…","ok":true}`. Names enable `ssh <name>@…` and
`GET /api/names/{name}`.

### `POST /api/leases/{id}/comment` — annotate

Request `{"comment": "…"}`. Response `{"id":"…","comment":"…","ok":true}`.

### `PUT /api/leases/{id}/holder` — set or clear the holder label

Owner or admin; anyone else gets the same `404` as the other lease
routes. Sets the `holder` and `holder_url` labels on an existing lease:

```json
{"holder": "ci-job-42", "holder_url": "https://ci.example.com/jobs/42"}
```

Both fields empty clears the label. The same validation applies as on
create (`400` naming the offending field). Response `200`
`{"id":"…","holder":"…","holder_url":"…","ok":true}`. Setting a
different holder replaces the old one; there is no `409` (v3.0: a
holder is a plain label, so there is nothing to conflict with).

**Since v3.0 the labels have no lifecycle effect:** setting a holder
never pins, never keeps the lease past its TTL and never protects it
from take-back — and it is not activity for the idle sweep either: only
work-call and keepalive activity moves `LastActive`. `hold_ttl` is
removed (accepted and ignored for one
release, with a `Deprecation` header). To keep a lease, use the pin
routes below. The label is shown on the dashboard (a pinned lease's
holder is marked ◆), and a lease with a holder that has its own
`checkpoint_interval` follows it.

### `PUT /api/leases/{id}/pin` and `DELETE /api/leases/{id}/pin` — pin or unpin

Owner or admin; anyone else gets `404`. A **pinned** lease is never
paused or deleted by spoond before its own expiry: take-back (memory or
disk) skips it, and only its own TTL or the owner's `DELETE` releases
it. A pinned **persistent** lease (no TTL) stays until the owner
releases it. There is no limit on how many leases an owner pins.
Response `200` `{"id":"…","pinned":true|false,"ok":true}`.

`holder` and `holder_url` never pin; only this route (or `"pinned":
true` on create) does. If a request needs room and every take-back
candidate is pinned, the create answers `429` with code `box_full` (and
an alert); nothing is paused or released.

### `POST /api/admin/unpin-by-holder` — unpin by holder prefix (admin)

Admin token only. Query `?holder_prefix=<prefix>` clears the pinned flag
of every lease whose `holder` starts with the prefix, returning
`{"unpinned": N}`. It exists for the 2.9→3.0 migration window: store
migration 0022 turns every live hold into a pin, and `pool-spawn` held
its workers by holder label (`pool:`), so an operator can unpin exactly
those. An empty prefix is refused (`400`).

### `POST /api/leases/{id}/resume` — resume a suspended lease (gateway)

Owner-blind resume for any suspended lease: used by the SSH gateway on
attach, where the capability is the lease id or name and no owner id is
known. Restores a suspended lease from `resume_build_id`
with the same sandbox id (see [Pins and the paused-release clock](operations.md#pins-and-the-paused-release-clock)). An
unknown lease answers `404`. Response `200`
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
- `reason`, `policy_step` and `build_id` are the structured fields of a
  `suspended` event (see below); they are omitted on every other event
  type and on a hand or drain suspend, which has no automatic reason.

A `suspended` event carries the structured suspension facts (#145 D6)
beside its human detail:

```
event: suspended
data: {"seq":43,…,"type":"suspended","detail":"paused into build 9e1f2ab3…","reason":"idle_suspend","build_id":"9e1f2ab3…"}

```

`reason` is `idle_suspend` (the lease's own threshold) or `take_back`
(a take-back suspension). The removed `idle`
(plain `IDLE_TIMEOUT_SECS` sweep), `hold_lapsed` and `pressure` reasons
are gone with the held rules (FS5). `policy_step` is the pressure
order's step name,
empty until that order names steps. `build_id` is the pause build the
suspend wrote. A `take_back` suspension also carries
`take_back_ratio` (the paused lease's owner's usage/slice ratio when it
was taken) and `take_back_for` (the owner whose request it made room
for). A suspend taken by hand or by the drain carries no
`reason`, `policy_step` or `build_id` — it has no automatic reason, so
the human `detail` still names the pause build.

The stream opens with a `retry: 3000` hint and a `: keepalive` comment
every 15 s thereafter, so proxies do not close an idle stream.

### Event types

| `event` | emitted when | `detail` names |
|---|---|---|
| `created` | a lease is granted, forked or cloned | the source image and how long the grant took, e.g. `granted from image py-base in 61 ms` (forks: the source lease and build; clones: the source lease and checkpoint build; a create from a named snapshot: `started from snapshot spoond/warm@3 in 410 ms`) |
| `released` | the lease is deleted (TTL sweep, `DELETE`, the one paused-release clock, a lost lease's grace period lapse) | why: the caller's `DELETE` reason when given (the runner sends e.g. `ci job 3609 ✓ 11m02s` or `ci job 3604 ✗ 4m10s`), else `deleted through the API`, `TTL expired`, `paused_expired` (the one clock released a paused lease 30 d after its pause date), `lost_expired` (the GC released a lost lease whose grace period lapsed), or `lease released` |
| `suspended` | the sandbox is paused into a build (suspend, drain, the lease's own idle_suspend, take-back) | the pause build id; the structured `reason`, `policy_step` and `build_id` fields name why (see [Wire format](#wire-format)) |
| `take_back` | the lease was paused by memory take-back so another owner's request could fit; it always follows the lease's `suspended` event with reason `take_back`, and the lease resumes on its holder's next work call | `for <owner>: owner <ratio> over their slice; it resumes on the holder's next work call` |
| `resumed` | the lease starts from a pause build (resume, undrain, gateway resume, resume on use) | the resume build id; a taken-back lease its holder's work call brought back |
| `checkpointed` | a running lease is checkpointed | the duration and the checkpoint build id, e.g. `540 ms · build 9e1f2ab3…` |
| `snapshot_saved` | `POST /api/leases/{id}/snapshots` saved the lease as a named snapshot (2.7, #83) | `saved as <name>@<version> · <size> · <duration>`, e.g. `saved as spoond/warm@4 · 2.1 GiB · 820 ms` |
| `recovered` | a lease is resumed from its checkpoint after a crash | the checkpoint build id |
| `recovery_retry` | a crash recovery failed transiently and the lease will be retried (spoond-dxq) | `recovering from checkpoint <build>: attempt N/K failed: <err>`, or `recovering from checkpoint <build>: waiting for capacity: <err>` (a capacity wait is not an attempt) |
| `rootfs_dead` | the rootfs liveness probe found the lease's root disk unreadable (I/O errors) and started the shared recovery | `root disk unreadable (I/O errors)` (before the `recovered`/`lost`/`recovery_retry` event that follows) |
| `lost` | the lease's sandbox died with nothing to recover from (crash reconcile, a permanently failed undrain resume such as a missing build, or a crash-recovery retry budget spent), or its root disk answered I/O errors (rootfs liveness probe) | the reason, e.g. `no checkpoint to recover from; the running state is gone`, `recovery from checkpoint <build> failed after N attempt(s) within <window>: <err>` or `root disk unreadable (I/O errors)`; the same text is stored as `lost_reason` and returned by `GET` and every `410 lease_lost` (see [Lost leases](#lost-leases)). A resume failure that is not permanent leaves the lease `suspended` with `suspend_reason: resume_failed` instead (spoond-638d) |
| `restarted` | `POST /api/leases/{id}/restart` completed | `restarted (snapshot round-trip)` for a warm persistent restart, `cold` for `mode=cold`, or `cold-restarted from image <image>` for a non-persistent lease |
| `restored` | `POST /api/leases/{id}/restore` completed (2.3, #121) | the restored-to checkpoint build id |
| `crash_test` | `POST /api/leases/{id}/crash-test` crashed the lease (only on hosts with `CRASH_TEST=1`) | `crashed by its owner` or `crashed by an admin` (before the `recovered`/`lost` event that follows) |
| `holder_set` | the holder label is set on `PUT /api/leases/{id}/holder` | the label |
| `holder_cleared` | the holder label is cleared | the clear |
| `pinned` | a lease is pinned (`PUT /api/leases/{id}/pin`, or `"pinned": true` on create) | the pin |
| `unpinned` | a lease is unpinned (`DELETE /api/leases/{id}/pin`) | the unpin |
| `paused_expiring` | a paused lease is 24 h from release by the one clock (FS5) | the pause date and the release time |
| `pinned_idle` | a pinned lease's last API activity crossed `PINNED_IDLE_NOTICE_DAYS` (visibility only) | the idle-since time, or `activity; pinned-idle notice cleared` |
| `box_full` | a request needed room and take-back could free none for it (FS5, FS2b); carries lease id `-` and the refused requester as `owner`, so it reaches the requester's own stream | the request's memory and owner |
| `admin_unpin` | `POST /api/admin/unpin-by-holder` unpinned leases by holder prefix; carries lease id `-` | the count and prefix |
| `job_started` | a background exec job started (2.6, #135) | the command, cut to 120 chars |
| `job_exited` | a background exec job ended (2.6, #135) | `exit <code>` and the last 10 stderr lines (at most 1 KiB); for a job killed by the max runtime, `timed out: exit 124` and the stderr excerpt (spoond-wb5) |
| `job_lost` | a running background job did not survive a generation bump (cold restart, restore, crash recovery) | the reason |
| `checkpoint_policy` | the lease's checkpoint interval changed on `PUT /api/leases/{id}/checkpoint-policy` | the new effective `checkpoint_interval` seconds |
| `idle_policy` | the lease's idle threshold changed on `PUT /api/leases/{id}/idle-policy` | the new effective `idle_suspend` seconds |
| `idle_suspended` | the idle sweep suspended the lease through the pause path | `idle for <duration>` |
| `gc` | a catalog GC pass deleted builds or failed a stale `building` row (spoond's own maintenance, not a lease's) | `N builds deleted · X GiB freed`, e.g. `1 build deleted · 512.0 MiB freed`; or `stale build <id> failed · build timed out` |
| `drain_failed` | the admin drain could not pause the lease: it ran on into the orchestrator stop (spoond-52c) | the pause error |
| `drain_deferred` | an undrain (or the drain self-heal loop) could not resume the drained lease yet: an admission refusal, a capacity answer, an indeterminate orchestrator error, or a bounded context (spoond-52c, spoond-638d) | `after N attempt(s): <error>`; the lease stays `drained` for a retry |
| `drain_healed` | the drain self-heal loop lifted a drain that outlived `DRAIN_MAX_SECS` on a healthy node, or cleared a node drain a failed undrain left set (spoond-52c) | `drain lasted <duration>` |
| `drain_gave_up` | the drain self-heal loop stopped retrying the lease's resume after `DRAIN_RESUME_MAX_AGE`; the lease stays suspended with its snapshot intact, for the owner or the one paused-release clock to exit (spoond-52c) | `resume deferred for over <duration>; leaving the lease suspended for the owner` |
| `user_deleted` | `DELETE /api/users/{id}` removed a user and cleaned up their state (spoond-q4j); every one of the user's leases emitted its own `released` event with reason `user_deleted` | `removed user <id>: N lease(s), N job(s), N snapshot(s), N kept build(s)` |
| `gap` | a hole in *your* stream, not a lease change | what was missed and why |

A `gc` event is lease-less: its `lease_id` and `owner` are empty, it
reaches the all-leases stream (and the events-only `EVENTS_TOKEN`) but
never `GET /api/leases/{id}/events` or a per-lease in-process
subscription, and the dashboard shows its subject as `spoond`. The
`box_full` and `admin_unpin` events are not about one lease either: they
carry lease id `-` (a placeholder, not a real id) so a consumer that
keys on `lease_id` sees a well-formed value and a per-lease stream
(`?lease_id=<id>`, or an in-process `EventFilter{LeaseID: …}`) never
matches them, since a real id is never `-`; they reach the all-leases
stream only (the SSE JSON carries `"lease_id":"-"`, as the dashboard's
event panel shows in its id column). The notifier keys them by
`box.full` and the prefix count, not the lease id. A GC
pass that deletes nothing (the default dry run included) emits none, but
the stale-building sweep does emit one per row it fails even in dry-run
mode, because a template build has no owner and never appears in
`/api/snapshots`, so the event is where its failure is visible.
`drain_healed` is lease-less the same way, and so is `user_deleted`
(it carries the removed owner); `drain_failed` and `drain_deferred`
name their lease (and owner), so they reach the per-lease stream too.

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

**Had secrets (#141).** A lease that ever held a secret is marked for
good: the first create-time `secrets`, exec-time `secrets` or
background job `secrets` sets the mark (it is stored with the lease,
before the files are written, and never cleared). A clone or fork
carries its source's mark, and a lease started from a named-snapshot
version that is not secret-free starts marked. A named snapshot saved
from a marked lease is not secret-free (`secret_free: false`): the save
scrubs `/run/secrets`, but a secret may live on elsewhere in the
guest's memory, so such a version can never be granted to another
user. Exec `env` values are **not** secrets and never set the mark, so
keep credentials out of `env` on a lease you mean to share.

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
            "vcpu":2,"memory_mb":1024,"disk_mb":4096,"min_memory_mb":512,
            "warm_sizes":["large","medium"],"updated_at":"…"}]}
```

`vcpu` and `memory_mb` are the image's default size. `min_memory_mb` is
the smallest memory a lease of the image may ask for: the manifest's
`min_memory_mb`, or when unset 512, or the image's own `memory_mb` when
that is smaller. `warm_sizes` names the size presets (see
[`GET /api/sizes`](#get-apisizes--size-presets-and-limits)) the image has
a memory snapshot for, sorted; `[]` when only the default size is warm
(#151).

Images are built with `spoond images build` (see [ci-jobs.md](ci-jobs.md)).

### `GET /api/sizes` — size presets and limits

Any authenticated caller. The named VM sizes a lease may ask for, in
`SIZE_PRESETS` order, and the most vCPU and memory one lease may be
given (`MAX_LEASE_VCPU`, `MAX_LEASE_MEMORY_MB`) (#151):

```json
{"presets":[{"name":"small","vcpu":1,"memory_mb":2048},
            {"name":"medium","vcpu":2,"memory_mb":4096},
            {"name":"large","vcpu":4,"memory_mb":8192},
            {"name":"xlarge","vcpu":8,"memory_mb":16384}],
 "max_vcpu":16,"max_memory_mb":32768}
```

A preset is a global name for a shape; an image starts warm at a preset
only when its `warm_sizes` (in `GET /api/images?detail=1`) lists it.

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

## Named snapshots (2.7, #83)

A **named snapshot** is a checkpoint build that has a name and a
version. It is owned by an identity and outlives the lease it was saved
from. Save one with `POST /api/leases/{id}/snapshots`, then start new
leases from it (see [Start from a snapshot](#start-from-a-snapshot)) and
manage it under `/api/named-snapshots`.

- **Name:** `<project>/<name>` or a bare `<name>`; each part matches
  `[a-z0-9][a-z0-9._-]{0,62}`. Names are unique per owner (the prefix is
  a convention, not a project concept).
- **Version:** an integer per (owner, name), starting at `1`. A version
  number is never reused: deleting a version and saving again gives the
  next number, not the deleted one (the name's high-water mark). `name`
  means the latest version; `name@3` pins one.
- A version row is inserted only after its checkpoint build is `ready`,
  in one transaction that computes `max(version)+1`, so a failed or
  in-flight save is never visible and `latest` moves atomically.

Every route is owner-scoped (a consumer token, or an admin): another
owner's name is invisible and answers `404 not_found`. Error bodies
carry a machine-readable `code` beside `error`.

### `POST /api/leases/{id}/snapshots` — save a lease as a named snapshot

Owner only; live leases only. An unknown lease answers `404 not_found`
with `"lease not found"`; a released lease answers `409
lease_not_live` and a lost one `410 lease_lost` (a lease busy with another operation answers `409
lease_busy`). A replay whose key already committed answers `200` before
the live check, so it works even after the source lease is gone.

```json
{"name": "spoond/warm", "idempotency_key": "fl-81c2/steps/warm", "keep": 3}
```

Saving checkpoints the lease (the guest pauses briefly and carries on)
and inserts the version. Response `201`:

```json
{"name":"spoond/warm","version":1,"build_id":"<uuid>","image":"py-base",
 "memory_mb":2048,"size_bytes":2254857830,"created_at":"2026-10-06T09:12:30Z",
 "secret_free":true}
```

`secret_free` is `true` when the source lease never had a secret (see
[Secrets](#secrets)); only a secret-free version can be granted.
Versions saved before grants existed read `false` until saved again.

- **Idempotent replay.** A save whose `(name, idempotency_key)` already
  exists answers `200` with that version and takes **no** checkpoint.
  The key is scoped by `(owner, name)`, never by lease: a replay from a
  different lease answers the same version (A1). A concurrent save with
  the same key while the first runs answers `409 save_in_progress` with
  `Retry-After: 5`. Without a key, every save makes a new version.
- **Secrets are not captured.** Before the checkpoint spoond removes
  **everything** under `/run/secrets` through a guest exec and verifies
  the directory is empty (so a secret staged before a backend restart is
  removed even though the process no longer remembers it; a directory
  that is not empty aborts the save with `500 scrub_failed` and no
  checkpoint), then re-stages the create-time secrets on the source
  after. A running background job that staged exec-time secrets makes
  the save `409 secrets_in_use`. A save and an exec/job that stages
  secrets are serialised by a per-lease gate: while a save runs an exec
  or job with secrets answers `409 lease_busy` with `Retry-After: 5`
  (it did not start, so retrying is safe). Anything else in guest memory
  or on disk is the caller's to scrub. Because the scrub touches the
  guest rather than this process's memory, **a save after a backend
  restart drops the source lease's create-time secrets**: the backend no
  longer knows their values, so it cannot re-stage them. It logs the
  lease id and how many removed files it no longer knew about (R4);
  re-send the secrets on the next exec.
- **Limits.** Named snapshot bytes count toward the owner's kept
  budget — their fair disk slice since 3.0 — alongside kept checkpoints
  (`409 kept_budget`), and a
  save that would add a name past `MAX_NAMED_SNAPSHOTS` (`0` = no cap)
  answers `409 snapshot_limit`. An existing name is always savable.
- **Snapshot disk.** The save's checkpoint must fit in usable snapshot
  disk (FS3): `429 box_full` when take-back has nothing to reclaim,
  `503 capacity_wait` with `Retry-After: 30` while the disk is still
  short.
- **Retention.** A name keeps its last `SNAPSHOT_KEEP_VERSIONS` versions
  (default `3`; the first save's optional `keep`, `1`–`20`, overrides it
  for the name — A6). Older versions lose their row after a save unless
  a live lease started from them (`leases.snapshot_build_id`); those go
  once no such lease remains. The builds are freed by the GC as usual.
- **Event.** A `snapshot_saved` event on the source lease, e.g.
  `saved as spoond/warm@4 · 2.1 GiB · 820 ms`.

A failed save stores nothing, so a replay with the same key runs a fresh
save (the key is not poisoned). Look the key up without starting a
checkpoint:

```
GET /api/named-snapshots/spoond/warm?idempotency_key=fl-81c2/steps/warm
```

answers `200` with `{"state":"in_progress"}`,
`{"state":"ready","version":4,"build_id":"…"}`,
`{"state":"failed","error":"…","code":"…"}` or
`{"state":"absent"}`. In-flight/failed state lives in memory for up to
24 h; after a backend restart a key reads `absent` (A2). A backend that
stops mid-save writes no version: at startup a `checkpoint` build still
`building` with no named row is marked `failed`, so nothing is stranded
(A7).

### `GET /api/named-snapshots` — list your snapshots

`?prefix=spoond/` narrows the list. `200`:

```json
{"snapshots":[{"name":"spoond/warm","latest":4,"versions":[
  {"version":4,"build_id":"…","image":"go-base-worker","memory_mb":4096,
   "size_bytes":2254857830,"created_at":"…","in_use":1,"stale":false,
   "secret_free":true}]}]}
```

`in_use` counts the live leases started from that version (`0` while
nothing uses it). `stale` is `true` when the image's current build is no
longer the version's `image_build_id` — the image was rebuilt since the
save — and is informational only.

### `GET /api/named-snapshots/{name}[@v]` — show one

`{name}` selects the latest version, `{name}@{v}` one version. A slash
in the name is allowed in the path. Response `200` is one version object
as in the list, with `in_use` and `stale`. `404 not_found` for an
unknown name or version.

### Start from a snapshot

A lease create accepts `"snapshot": "spoond/warm"` (the latest version)
or `"snapshot": "spoond/warm@3"` (a pinned one) in place of a fresh
lease from the image's current build. `image` may be omitted; when given
it must equal the snapshot's image (`400 image_mismatch`). Everything
else is a normal create — TTL, persistence, network policy and
allowlist, holder, create-time `secrets` and `wait` all come from the
request, not the source lease.

- **Memory** is the snapshot's `memory_mb`, for admission and the
  owner's fair-share usage alike.
- The lease gets a **new lease id and generation `1`**. Its create
  response and `GET /api/leases/{id}` carry
  `"snapshot":{"name":"spoond/warm","version":3,"build_id":"…"}` —
  the resolved version.
- Its `created` event says
  `started from snapshot spoond/warm@3 in 410 ms`.
- The lease is **never served from the warm pool**, and it is stamped
  with the version's build (`leases.snapshot_build_id`), so retention
  never drops a version a live lease runs from.
- **Errors:** an unknown name or version is `404` `{"code":"not_found"}`;
  a snapshot that cannot start on this host — for example after an
  orchestrator or envd upgrade — is `409`
  `{"error":"snapshot spoond/warm@3 cannot start on this host (…); save
  it again","code":"cannot_start"}` with no retry loop. Only failures
  that mean the build cannot run (missing build files, an incompatible
  envd/firecracker/orchestrator) become `cannot_start`; capacity and
  transient failures keep their usual handling.

A version spared by retention only because a lease ran from it is
dropped once that lease is released (delete, TTL, lost cleanup):
releasing the lease re-runs the name's retention — the owner's, also
when the lease was another user's started through a grant.

**From another user's snapshot.** With `"snapshot_owner": "<user name
or id>"` the create starts from that user's name through a grant (see
[Snapshot grants](#snapshot-grants)). Without a grant — or for an
unknown user or name — it answers `404 not_found`, the same body as an
unknown name, so a caller learns nothing about names it was not
granted. A granted version that is not secret-free answers `409
not_secret_free`. Everything else is a normal snapshot start: the
caller is charged the version's memory, and the snapshot's disk stays
the owner's.

### `DELETE /api/named-snapshots/{name}[@v]` — delete

`{name}` deletes every version, `{name}@{v}` one version. `409
snapshot_in_use` when a live lease started from it; `?force=1` drops the
row anyway (the build stays until the lease no longer needs it).
Responds `204`; a second delete answers `404 not_found`. The name's
retention setting (and its version high-water mark) is kept, so a later
save gets the next number, not a deleted one.

### `PUT /api/named-snapshots/{name}` — set retention

```json
{"keep": 3}
```

`keep` is required and `1`–`20`. Changes the name's retention and
applies it at once (the live-lease rule still holds). Response `200
{"name":"…","keep":3,"ok":true}`; `404 not_found` for an unknown
name. `keep` is set per name, so a `@version` in the path is `400
bad_request` (`"keep is set per name; drop the @version"`).

### Snapshot grants

An owner can let another user start leases from one of its names
(#141), so one warm-up — a checkout plus build caches — serves both.
A grant covers the name, not a version. Only **secret-free** versions
(saved from a lease that never had a secret, see [Secrets](#secrets))
can be granted or started through a grant. The grantee is charged the
lease's memory; the snapshot's disk stays in the owner's budget, and
retention, `in_use` and delete-in-use count the grantee's leases like
the owner's. A grantee's lease shows the owner in its `snapshot` object
and may [restore to the start snapshot](#post-apileasesidrestore--roll-back-to-a-kept-checkpoint).
Deleting a user removes the grants it gave and received.

#### `POST /api/named-snapshot-grants` — grant

```json
{"name": "honey/warm", "grantee": "honey"}
```

`grantee` is a user name or id; it must exist and differ from the
caller (`400 bad_request`). Response `201`, also on a repeat (the first
grant is kept):

```json
{"owner":"u-…","name":"honey/warm","grantee":"u-…","created_at":"…"}
```

`404 not_found` when the caller has no such name; `409 not_secret_free`
when the name's latest version is not secret-free — re-save it from a
lease that never had a secret, then grant.

#### `GET /api/named-snapshot-grants` — list

```json
{"given":[{"owner":"u-pool","name":"honey/warm","grantee":"u-honey","created_at":"…"}],
 "received":[]}
```

#### `DELETE /api/named-snapshot-grants?name=<name>&grantee=<user>` — revoke

Owner only. `204`; `404 not_found` when there is no such grant.
Revoking stops new starts at once (they answer `404`); it never touches
a lease already running from the snapshot.

### Identity in a restored guest

A memory snapshot resumes every captured process as soon as the VM runs,
before spoond can write anything, so spoond cannot promise that files
exist before captured processes run. Instead it guarantees markers so a
process that knows a save is coming can tell which side it is on (A4).

- **Source side.** On the source lease, right after the checkpoint
  completes and before the save answers, spoond writes
  `/run/spoond/last-save` (`0644`, JSON
  `{"name","version","build_id","idempotency_key"}`). The copy's
  memory was captured before this write, so a copy never sees this
  save's marker.
- **Copy side.** On a lease started from a snapshot, spoond writes
  `/run/spoond/lease-id`, `/run/spoond/generation` and
  `/run/spoond/started-from` (JSON `{"name","version","build_id"}`)
  before the create answers and before any exec or job the API runs in
  it.

All of these are written atomically (a temp file, then a rename), so
inotify on `/run/spoond` sees whole writes. A process such as Honey's
stepd records `save pending (key K)` before it asks for the save; after
that it acts on no captured handle until either `last-save` shows key
`K` (it is the source and carries on) or `lease-id` differs from the id
it recorded (it is a copy and runs its restore path).

**Known limit (A5).** No spoond credential is bound to the generation: a
lease id is a capability (the heartbeat and the LLM gateway key on it),
and processes captured in a snapshot keep the source's
`SPOOND_LEASE_ID`. While the source lease lives, a copy that still uses
that id acts as the source (fork and clone already behave this way). A
new lease gets a new id and generation `1`, but the generation revokes
nothing. Meanwhile: save **before** long-running agent processes start,
and release the source after saving. A guest capability bound to
`(lease id, generation, guest identity)` is a separate issue, not part
of 2.7.

### `GET /healthz`

No auth. `200 {"status":"ok","orchestrator":"<status>","draining":bool}`
when the orchestrator answers, `503 {"status":"degraded","orchestrator":"unreachable"}`
when it does not — for Gatus/load balancers. `"draining":true` while
an admin drain is in effect, so a monitor can tell a draining node from
a healthy one.

### `GET /readyz`

No auth. Readiness for an external uptime monitor: `200
{"status":"ok"}` only when every check passes — the orchestrator's
node info reports the node healthy, the catalog answers a trivial
query, and the snapshot disk and hugepage pool sit below the
dashboard's danger levels (90 % / 92 % used). Otherwise `503` with
`{"status":"fail","checks":[{"name","ok","detail"}…]}` naming each
failing check. A `draining` check names the admin drain state; it never
fails readiness (the create route's 503 draining is the refusal).
Every check is bounded to 2 s and the answer is cached for 5 s. See
[operations.md](operations.md) for a Gatus example; the dashboard
listener serves a `/readyz` over its own sources too.

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
| `POST /api/admin/drain` | Set the node draining and pause every live lease into a pause build (marking it drained), delete the warm pool, then wait up to 180 s until the node reports no running sandboxes and no outstanding work. Runs on a context detached from the request (a client disconnect does not cancel the pauses) bounded by 6 min. Response `{"paused":N,"failed":[{"id","error"}],"pool_deleted":M,"quiesced":bool}`; each failure is logged and emits a `drain_failed` event. `503 {"error":"orchestrator unreachable: …"}` (nothing changed) when the node cannot be reached. A manual drain held longer than `DRAIN_MAX_SECS` (default 900) on a healthy node is lifted automatically (see `POST /api/admin/undrain`); a backend that starts while the node reports `draining` adopts that drain. |
| `POST /api/admin/undrain` | Wait for the orchestrator to answer `NodeInfo` and a `List` (`UNDRAIN_READY_TIMEOUT`, default 180 s), clear draining, then resume exactly the drained leases `UNDRAIN_CONCURRENCY` (default 2) at a time. A resume that fails with a retryable envd/start error is retried `UNDRAIN_RESUME_RETRIES` (default 2) times; an indeterminate orchestrator error (`Unavailable`, a connection reset) is retried with backoff for `UNDRAIN_RESUME_WINDOW` (default 5 min) and never loses the lease. A resume that exhausts its retries is left suspended and `drained` with reason `resume_failed` so resume-on-use and the self-heal loop can retry it; only a permanent error (the build is gone) marks it `lost`. An admission refusal, capacity answer or bounded call stays drained for the next undrain or the self-heal loop. A failed clear keeps the node draining and the leases drained so the self-heal loop retries the clear. Runs on a context detached from the request bounded by 20 min. `failed` entries carry `attempts`. Response `{"resumed":N,"failed":[{"id","error","attempts"}]}`. |
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

Records activity on a lease so the idle_suspend sweep does not
auto-suspend it while an agent is
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
lease; `409` for a suspended lease (`code: lease_suspended`); `405` for
any other method. A lease merely busy with a lifecycle operation is not
suspended, so its heartbeat still answers `204`. Writes
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
max_ttl, used_mib, created_at}}` —
the token hash and LLM key hash are
never exposed. `used_mib` is `0` on a fresh user.

### `GET /api/users` — list users (admin only)

`403` for non-admin callers.

### `GET /api/users/me` — current user

Self-service: `{"user": {id, name, kind, admin, fingerprints,
created_at, max_ttl, used_mib}, "share": {owner, name, slice_pct,
capacity_known, memory {slice_mib, used_mib}, disk {slice_bytes,
used_bytes, paused_bytes, kept_bytes, named_bytes}, ratio}, "leases":
{running, paused, pinned, lost, running_jobs}}` (#128, #145 FS1, #140
U1).
`used_mib` is the user's current memory charge — the sum of `memory_mb`
over their running leases (suspended ones hold no hugepages).
`max_leases`, `guaranteed_mib`, `max_mib` and `max_kept_bytes` are no
longer returned (3.0). `share` is the
caller's fair-share slice and usage; see
[Fair shares](#fair-shares) below. `capacity_known` is `false` when the
box totals were not readable, so the slices are not computed yet.
`leases` counts the caller's leases from the in-memory map: `running`
(including recovered), `paused`, `pinned`, `lost`, and `running_jobs`
the background jobs those leases run (#140 U1). A pinned running lease
counts in both `running` and `pinned`.

### `GET /api/users/{id}` — one owner's usage (admin only)

Admin-only. `{"user": {…as `GET /api/users`…}, "share": {…as
`GET /api/users/me`…}}` — the full user record plus that owner's
fair-share slice and usage. `404` for an unknown id; `403` for a
non-admin caller.

### `GET /api/usage` — caller's own fair-share usage

Any authenticated caller. `{"share": {owner, name, slice_pct,
capacity_known, memory {slice_mib, used_mib}, disk {slice_bytes,
used_bytes, paused_bytes, kept_bytes, named_bytes}, ratio}, "leases":
{running, paused, pinned, lost, running_jobs}, "waiting": [{id, owner,
image, position, waited_s, reason}], "recent": [event]}`. `waiting` lists the caller's
creates queued for admission, the same entries as
[`GET /api/leases/queue`](#get-apileasesqueue--creates-waiting-for-admission):
`position` is the place in the whole queue (1 is tried first), so it
counts other owners' waiting creates too. Empty when nothing waits
(#140 U2).
`recent` lists the caller's newest refusal and reclaim events, newest
first, at most 20: `box_full` (the caller's refused requests),
`take_back`, `suspended`, `critical_release` and `paused_expiring` (on
the caller's own leases) and `queued`, each in the lease event shape
(`seq`, `at`, `lease_id`, `type`, `detail`, `reason`, …). A take-back
appears for the owner whose lease was taken, not for the owner it made
room for. It also lists the caller's leases spoond released on its own
clock (`released` for a TTL expiry, the paused-release clock or a lost
lease's grace; not a release the owner asked for). It reads the
backend's in-memory event ring, so it starts
empty after a backend restart (#140 U3).
Self-scoped: a legacy consumer
token also has an owner, so its usage is available; a caller whose
owner somehow does not exist answers `404`.

### `GET /api/usage/history?hours=N` — the caller's 7-day usage history

Any authenticated caller. Returns the caller's own recorded samples,
oldest first, from the backend's sampler: one row per owner every 15
minutes (memory used in MiB, disk used in bytes, and the running and
paused lease counts). `hours` selects the window — default `24`, minimum
`1`, and values above `168` (7 days, the retention) are clamped rather
than refused. A non-integer or a value below `1` is a `400`. An owner
with no samples reads an empty list.

```json
{"hours": 24, "samples": [
  {"at": "2026-06-01T12:00:00Z", "memory_used_mib": 2048,
   "disk_used_bytes": 31457280, "running": 1, "paused": 2}
]}
```

Storage is `usage_samples` (migration 0024): `owner`, `at`,
`memory_used_mib`, `disk_used_bytes`, `running`, `paused`, indexed by
`(owner, at)`; rows older than 7 days are pruned by the sampler.

### `GET /api/users/by-name/{name}` — minimal lookup

`{"user": {id, name}}` — the shape for share-granting and gateway name
resolution.

### `GET /api/users/by-key?fingerprint=…` — resolve SSH key

`{"user": {id, name}}` — the gateway calls this in its
`PublicKeyCallback`; minimal shape.

### `DELETE /api/users/{id}` — remove a user (admin only)

Removes the identity and its keys, then cleans up the state that would
otherwise outlive it: every lease of the removed user is released (each
`released` event carries reason `user_deleted`), every running job is
signalled and settled, every named snapshot version and its settings
row are dropped, every kept build is unpinned and every share held on
the user's leases goes with the releases. The cleanup runs on a context
detached from the request (a client disconnect cannot leave it half
done) bounded by 5 min.

The answer is `200` with what was removed:

```json
{"removed": {"user": "u-…", "leases": ["…"], "jobs": ["…"],
            "snapshots": ["warm@1"], "kept_builds": ["…"]}}
```

The lists are always present (empty when there was nothing to remove).
A create of the deleted user that is still waiting in the admission
queue (or that raced the delete) is refused with `403 owner deleted`
rather than granted: granting it would recreate exactly the ownerless
state the delete removes. Removing the identity is what actually revokes SSH access —
the gateway treats the identity store as authoritative when present, so
removing the user invalidates all their keys immediately. Before
spoond-q4j the delete answered `204` and left the leases, snapshots,
kept builds and jobs behind, ownerless.

An id that is neither a known identity nor has any remaining state
answers `404 user not found`; deleting the same real user twice is
idempotent and answers `200` the second time, **until the backend
restarts**: the marker that makes a repeat delete answer `200` rather
than `404` lives in memory only, so after a restart a repeat delete of a
fully cleaned user answers `404` (it was already gone before the mark
was lost). An id that is the owner of
a legacy consumer token (a single-user deployment's token map) answers
`409` and is left untouched: it has no identity row but still
authenticates, so marking it deleted would permanently refuse its
creates.

A cleanup that fails part-way answers `500` instead of `200`, with the
partial `removed` body, `"incomplete": true` and the store step that
failed:

```json
{"removed": {"user": "u-…", "leases": ["…"], "jobs": [],
            "snapshots": [], "kept_builds": ["…"]},
 "incomplete": true, "step": "drop_named_snapshots",
 "error": "drop_named_snapshots: database is locked"}
```

The `step` names the failed call: `list_jobs`, `list_kept_builds`,
`unpin_kept_builds` or `drop_named_snapshots`. The other steps still
ran, so `removed` lists what the attempt did remove. Retrying the delete
completes the cleanup: the identity is already gone and every step is
idempotent, so the second call clears whatever rows the failed step left
and answers `200` when nothing is left.

A retry after a backend restart is the same, with one extra check: the
in-memory deleted-owner mark is gone, so the delete first reads the
store to tell an id that never existed (`404`) from a half-cleaned one
the retry must finish. When that read itself fails — a busy SQLite —
the delete answers `500` with `read state: …` rather than a `404` that
would hide the leftover rows; retry once the store is readable
(spoond-y0jj T2).

### `POST /api/users/{id}/quota` — set the TTL cap (admin only)

Request `{"max_ttl": S}` — the user's lease TTL ceiling in seconds
(`0` = the global maximum applies; negative is `400`). Response
`{"user": {…}}` as `GET /api/users`.

**Deprecated fields.** `max_leases`, `guaranteed_mib`, `max_mib` and
`max_kept_bytes` were removed in 3.0 (fair shares): every owner gets an
equal slice instead, and the kept budget is the owner's disk slice. A
request may still send them — any value is accepted (no `400`, and the
old `guaranteed_mib <= max_mib` rule is gone) and ignored. When any of
them is set to a non-zero value, the `200` carries:

```
Deprecation: true
Warning: 299 - "max_leases, guaranteed_mib, max_mib and max_kept_bytes are ignored since 3.0 (fair shares)"
```

```sh
curl -fsS -X POST "$SPOOND_URL/api/users/$UID/quota" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"max_ttl":3600}'
```

The user's memory charge is `used_mib` on `GET /api/users/me`.

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

## Fair shares

Every owner gets an **equal floating slice** of the box: `1/N` of the
hugepage memory pool and of the snapshot volume's **usable** bytes,
where `N` is the number of owners that exist — every identity user,
plus the legacy consumer token as one owner. There are no per-owner
weights or settings; the slice is recomputed when an owner is added or
deleted (`POST /api/users`, `DELETE /api/users/{id}`).

The same fraction applies to both resources. Usage is measured from
recorded state, never a filesystem walk per request:

- **memory** — the memory of the owner's *running* leases (a suspended
  lease holds no hugepages).
- **disk** — the owner's pause snapshots + kept checkpoints + named
  snapshots, from each build's recorded `size_bytes`. A build that is
  both a kept checkpoint and a named snapshot counts once. For a named
  snapshot the build's `size_bytes` is preferred and the named row's
  own `size_bytes` (the size at save time) is used only when the build
  row is gone — the build is re-measured by the hourly disk accounting
  pass and is authoritative. `named_bytes` can therefore differ from the
  `size_bytes` the [named-snapshot listing](#get-apinamed-snapshots--list-your-snapshots)
  reports for the same snapshot; the fair-share number is the disk's.

The **disk slice basis** is the snapshot volume's usable bytes: the
`statfs` bytes available to an unprivileged writer (`Bavail`, not the
raw filesystem total, which includes the root-only reserve) plus the
snapshot bytes spoond itself already accounts for (kept + named +
paused). The accounted bytes are summed over **distinct builds**, so a
build shared by two owners (one pins another's build, or a named
snapshot is owned apart from its build's owner) counts once for the box
rather than once per owner. That is the space spoond can hand out to
owners: the free space plus what it currently holds and can reclaim.
Memory uses the hugepage pool spoond admits against.

The share object is:

```json
{
  "owner": "u-…",
  "name": "jason",
  "slice_pct": 33.33,
  "capacity_known": true,
  "memory": {"slice_mib": 341, "used_mib": 0},
  "disk": {"slice_bytes": 357913941, "used_bytes": 0,
           "paused_bytes": 0, "kept_bytes": 0, "named_bytes": 0},
  "ratio": 0.0
}
```

`ratio` is `usage/slice`, the larger of the memory and disk ratios; an
owner inside their slice is below `1`. When the box has no capacity (or
no owners exist) every slice and ratio is `0` — no division by zero.

`capacity_known` reports whether the box totals behind the slice were
readable. `false` means the slices are **not computed yet** — the
node-info cache that holds the hugepage pool was cold (about the first
15 s after a restart) or a store read failed — so the endpoint does not
serve a wrong zero for a real slice. The response is still `200` and
carries the owner list with the usage buckets that were readable; the
result is not cached, so the next request recomputes. The same flag is
on every owner object in `GET /api/fair-shares` and on the self-scoped
`share` payloads.

Capacity is read from the node-info cache the admission path keeps warm
(`updateNodeMetrics`); the fair-share routes never make a substrate RPC.
When that cache is cold, or on a failed store read, the slices are `0`,
`capacity_known` is `false` and the view is not cached, so the next
request retries.

### `GET /api/fair-shares` — every owner's slice and usage (admin only)

`403` for non-admin callers. Read-only:
`{"capacity_known": true|false, "owners": [{owner, name?,
capacity_known, slice_pct, memory {slice_mib, used_mib}, disk
{slice_bytes, used_bytes, paused_bytes, kept_bytes, named_bytes},
ratio}], "disk": {…}}`, sorted by `ratio` descending — the owner
furthest over their slice first. Ties sort by owner id. This is the
view the take-back policy reads; this version only reports it.

When the snapshot disk can be read, the response also carries a `disk`
object — the room the take-back policy decides with, every size in
bytes:

```json
"disk": {"total_bytes": 0, "free_bytes": 0, "reserved_bytes": 0,
         "floor_bytes": 0, "pending_bytes": 0, "freeing_bytes": 0,
         "usable_bytes": 0}
```

`total_bytes` and `free_bytes` are the statfs totals, `reserved_bytes`
the running leases' whole `disk_mb` allowance, `floor_bytes` the
reserve kept free (`DISK_RESERVE_PCT`, default 5), `pending_bytes` the
admitted requests that have not written yet, `freeing_bytes` the
deletions issued but not yet visible in statfs, and `usable_bytes` the
room a new request may actually use
(`free − reserved − floor − pending + freeing`, negative when short).
`disk` is **omitted** when the statfs read fails or the total is zero —
an unknown disk is not a zero disk. See
[Fair shares](#fair-shares) for the take-back rule that reads these
numbers.

`GET /api/shares` is unchanged: it still lists the caller's lease
grants (see [Shares](#shares)).

### Self-scoped usage

An owner reads their own slice and usage from `GET /api/users/me`
(identity users) or `GET /api/usage` (any authenticated caller,
including a legacy consumer token). `GET /api/users/{id}` is the
admin-only per-owner view.

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

There is no per-lease share listing. `GET /api/shares` lists
every share granted on the caller's leases — `{"shares": [{lease_id,
grantee, mode, created_at, expires_at?}]}` — which is what `share ls`
prints. This route is unchanged in 3.0; the admin fair-share view is
`GET /api/fair-shares` (see [Fair shares](#fair-shares)).

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
  `idle_suspend` can auto-suspend idle persistent leases, and a paused
  lease is released 30 days after its pause date by the one clock.
- All lease state, shares, the pool and the image catalog persist in
  SQLite: a backend restart loses none of it.
- The lease id doubles as a capability (e.g. `GET /api/…/endpoint` and
  the proxy hostname are how a caller addresses someone's lease).
