# Upgrading to 3.0 (for API clients)

A guide for API clients, Honey above all, moving from spoond 2.9 to 3.0.
The full contract is in [api.md](api.md); the history is in the
[changelog](../CHANGELOG.md).

## What changed

Every owner with a running or paused lease now gets an equal, floating
1/N slice of the box (memory and snapshot disk). There are no per-owner
caps any more. When a request does
not fit, spoond takes room back from the biggest borrower (the owner
furthest over its slice) by pausing that owner's least recently used
unpinned lease. A pin replaces a hold: a pinned lease is never paused or
deleted by spoond before its own expiry. One clock covers every paused
lease: it is released `PAUSED_RELEASE_DAYS` (default 30) days after its
pause date, with a `paused_expiring` event 24 h before. A taken-back lease
resumes by itself on the next work call.

## Do this

- [ ] **Pin what must not be paused.** Send `"pinned": true` on create, or
  `PUT /api/leases/{id}/pin` (`DELETE` unpins). Add `"persistent": true`
  if the lease must outlive the owner's `max_ttl`. `holder` and
  `holder_url` are plain labels now: they never pin and never renew
  anything, and `PUT /holder` with a new holder replaces the old one
  (no `409`). Honey must create its leases pinned before 3.0 goes live.
- [ ] **Handle `429 box_full`.** It means take-back can free nothing:
  every candidate is pinned, or the caller is itself the biggest
  borrower. Do not retry blindly. Release or unpin your own leases, then
  ask again. It applies to create, resume, every resume-on-use work call,
  restart, clone, fork and restore, and to checkpoints and named saves
  that do not fit the snapshot disk.
- [ ] **Treat `503 capacity_wait` as a wait on every path.** The body has
  `code: capacity_wait` and the response has `Retry-After: 30`. Nothing is
  lost; retry after the delay. A create can wait it out with `wait`.
- [ ] **Stop relying on `429 quota_exceeded`.** It and the plain
  over-quota `429` are gone from every path.
- [ ] **Expect take-back pauses.** A taken-back lease gets a `suspended`
  event with `reason: take_back`, plus `take_back_ratio` (the victim
  owner's usage/slice) and `take_back_for` (the requesting owner). It
  stays suspended until its next work call (exec, stream, files, proxy,
  jobs, LLM gateway) or an explicit `resume`. It is waiting, not gone:
  do not delete and recreate it. Resume of a non-persistent suspended
  lease is no longer a `400`.
- [ ] **Stop sending `burst`, `priority` and `hold_ttl`.** `burst` and
  `priority` are ignored. `hold_ttl` is accepted and ignored for one
  release, with a `Deprecation` header.
- [ ] **Stop reading removed lease fields.** `class` and `priority` are
  gone, as are `hold_expires_at` and `hold_state`. `preempted` stays and
  is always `false`. New fields: `pinned`, `paused_at`,
  `pinned_idle_since`.
- [ ] **Drop the four quota fields.** `max_leases`, `guaranteed_mib`,
  `max_mib` and `max_kept_bytes` are no longer returned by the user views
  or quota response. `POST /api/users/{id}/quota` sets `max_ttl` only; it
  still accepts the four, ignores them, and answers with
  `Deprecation: true` and a `Warning: 299` header when any is set.
- [ ] **Re-read `kept_budget`.** `409 kept_budget` now measures the
  owner's kept checkpoints plus named snapshots against the owner's fair
  disk slice. While the box capacity is unknown there is no budget.
- [ ] **Do not expect idle pauses for pinned leases.** A pinned lease is
  never idle-suspended; an explicit `POST /pause` still pauses it. A
  paused pinned lease is still on the 30 d clock: a pin protects only a
  running VM.

## Seeing your share

- `GET /api/usage` and `GET /api/users/me` return the caller's
  `slice_pct`, `memory {slice_mib, used_mib}`, `disk {slice_bytes,
  used_bytes, paused_bytes, kept_bytes, named_bytes}` and `ratio`.
  `capacity_known: false` means the slices are not computed yet (not zero).
- `GET /api/fair-shares` (admin) lists every owner by ratio, highest
  first (the share object carries an `active` flag: whether the owner
  counts toward N). `GET /api/shares` is unchanged: it still lists
  lease grants.
- A ratio above 1 means the owner borrows; those leases are taken first.

## Status codes per path

| Path | 2.9 | 3.0 |
|---|---|---|
| create | `429` over `max_leases`/`max_mib`; `503 capacity` | `429 box_full`, or `503 capacity_wait` + `Retry-After` |
| `resume` | `429 quota_exceeded`; `503 capacity_wait` | `429 box_full`, or `503 capacity_wait` + `Retry-After` |
| resume-on-use work calls (exec, stream, files, dial, proxy, jobs, LLM gateway, network, prompt) | `429 quota_exceeded`; `503 capacity_wait` | `429 box_full`, or `503 capacity_wait` + `Retry-After` |
| `restart` | `429` over `max_mib` | `429 box_full`, or `503 capacity_wait` (was `500` on a capacity failure) |
| `clone`, `fork` | `429` over caps; `503 capacity` | `429 box_full`, or `503 capacity_wait` (was a bare `503`) |
| `restore` | `429` over `max_mib`; `503 capacity` | `429 box_full`, or `503 capacity_wait` |
| `checkpoint` `{"keep":true}` | `409 kept_budget` over `max_kept_bytes` | `409 kept_budget` over the fair disk slice; `429 box_full` or `503 capacity_wait` if it does not fit the snapshot disk |
| named save (`POST .../snapshots`) | `409 kept_budget` | same as keep; no more `500 save failed` for disk |

Also: a disk-full failure from the orchestrator is a `503 capacity_wait`,
not a `500`.

## Events

- Removed: `preempted` and `promoted`.
- Take-back appears as `suspended` with `reason: take_back`,
  `take_back_ratio` and `take_back_for`, followed by a `take_back` event
  whose detail names the requesting owner. The reason `preempt` is now
  `take_back`. The `reason` values `idle`, `hold_lapsed` and `pressure`
  are gone; `idle_suspend` remains for a lease's own `idle_suspend`.
- Added: `box_full` (lease id `-`, all-leases stream only), `pinned`,
  `unpinned`, `paused_expiring`, `pinned_idle`, `admin_unpin` (lease id
  `-`). Key on `lease_id` and expect `-` for lease-less events.
- A `released` event may now say `paused_expired` (the one clock).
- The `queued` event's refusal is always `capacity`; queued creates are
  served lowest ratio first, then FIFO.

## Metrics

| Change | Name |
|---|---|
| removed | `spoond_preemptions_total`, `spoond_preempted_leases`, `spoond_quota_exceeded_total`, `spoond_held_actions_total` |
| added | `spoond_box_full_total` (replaces `spoond_held_actions_total`), `spoond_take_backs_total`, `spoond_snapshot_disk_reserved_bytes`, `spoond_snapshot_disk_usable_bytes` |
