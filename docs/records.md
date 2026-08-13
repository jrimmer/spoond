# Sandbox record / replay

Record a sandbox run as a pair of checkpoints and re-attach to the
after-state later — for debugging and audit (issue #55).

## Model

A **record** is a before/after checkpoint pair of one sandbox run:

| Field | Meaning |
|---|---|
| `id` | record id |
| `owner` | owning user |
| `label` | optional free-form annotation |
| `sandbox_id` | the lease id the run happened in |
| `before_tag` | forkd snapshot tag captured at `record start` |
| `after_tag` | forkd snapshot tag captured at `record stop` |

Each checkpoint is a forkd **branch** snapshot of the running sandbox
(memory + filesystem). Replay spawns a fresh sandbox from the checkpoint
tag via the same snapshot grant path as `clone`.

> **Cost note:** checkpoints today are full branch snapshots. Diff / live
> (UFFD) snapshot modes are a forkd-level follow-up, so a record costs a
> full snapshot per checkpoint. Snapshot retention is managed by forkd —
> deleting a record removes spoond's bookkeeping but not the underlying
> forkd snapshot.

## Limits and lifecycle

- **Per-owner cap:** at most 50 live records per owner; `record start`
  returns 429 beyond that. Delete old records to make room.
- **One open record per sandbox:** `record start` on a sandbox that
  already has an open (un-stopped) record returns 409; stop it first.
- **Label length:** labels are capped at 128 characters; longer labels
  are rejected with 400.
- **Records are volatile bookkeeping:** the record store is in-memory and
  lost on a backend restart, while the forkd snapshots they reference
  survive. Reaping orphaned `rec-*` snapshots is a forkd-level follow-up
  (there is no snapshot-delete endpoint yet).
- **Open records linger if the sandbox is deleted:** stopping a record
  whose sandbox was `rm`'d/expired returns 404; the record can still be
  replayed from its `before` checkpoint or deleted.

## API

| Endpoint | Purpose |
|---|---|
| `POST /api/sandboxes/{id}/record/start` `{label?}` | checkpoint "before", open a record |
| `POST /api/records/{id}/stop` | checkpoint "after", close the record |
| `GET /api/records` | list your records |
| `GET /api/records/{id}` | get one record |
| `POST /api/records/{id}/replay` | spawn a sandbox from the after (or before) checkpoint |
| `DELETE /api/records/{id}` | delete a record |

All routes are bearer-token authenticated and owner-scoped.

## ctl / spoondctl

```
ssh ctl@<host> record start <lease-id> [label]
ssh ctl@<host> record stop <record-id>
ssh ctl@<host> record ls
ssh ctl@<host> record replay <record-id>
ssh ctl@<host> record rm <record-id>
```

## Metrics

- `spoond_record_created_total` — records started
- `spoond_record_replay_total` — replays granted
- `spoond_records_active` — open (started, not stopped) records