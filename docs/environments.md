# Per-PR ephemeral environments

A per-PR *environment* is a persistent, workspace-backed sandbox bound to
a repository PR (or branch). It is created automatically on PR open and
kept warm across pushes, then torn down on PR close. Think of it as
"spin up this PR's dev environment, run a command in it, tear it down" —
sandbox as the unit of CI, but reusable across the PR's lifetime instead
of thrown away per job.

An environment is just a normal sandbox under the hood (a persistent
lease with a workspace snapshot), with three extras: a stable key, a
stable SSH-addressable name, and a Forgejo-webhook-driven lifecycle.

## Model

| Field | Meaning |
|---|---|
| `key` | stable id `owner/repo#<pr>` (or `owner/repo#<branch>` if you create one by hand) |
| `repo` | Forgejo `owner/repo` full name |
| `ref` | PR number (or branch name) |
| `sandbox_id` | the backing lease id — use it with `exec`/`ssh`/`rm`/… like any other sandbox |
| `image` | the snapshot the environment is provisioned from |
| `owner` | the identity that owns the environment (webhook-created envs use `ENV_OWNER`) |

Environments are workspace-backed persistent leases, so they
suspend/resume across pushes and can idle-suspend without losing state.
`ensure` is idempotent: repeated events for the same `repo#ref` return the
same environment (a provisioning sentinel prevents concurrent double
creation).

## Webhook lifecycle

Configure a Forgejo webhook (`pull_request` events) to
`POST https://<backend>/hooks/forgejo` with a shared secret. The receiver
verifies the `X-Forgejo-Signature` (HMAC-SHA256 of the raw body) and:

| PR event `action` | Effect |
|---|---|
| `opened`, `reopened`, `synchronize`, `edited`, `ready_for_review` | ensure the environment exists (create-or-reuse) |
| `closed` (merged or not) | tear the environment down |
| anything else (`ping`, `push`, …) | acknowledged, ignored |

The receiver is **auth-exempt** (Forgejo presents no consumer token) —
the HMAC secret is the only gate, so set a strong `WEBHOOK_SECRET`.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `WEBHOOK_SECRET` | *(empty = receiver disabled)* | HMAC-SHA256 shared secret for `/hooks/forgejo` |
| `ENV_OWNER` | first consumer token | identity that owns webhook-created environments |
| `ENV_IMAGE` | `dev-base` | snapshot used for webhook-created environments |

The backend already runs TLS on `:8890`; the webhook should target the
TLS listener (or sit behind Caddy) so the secret is not sent in clear
text.

## API

All environment endpoints are owner-scoped (bearer-token auth, like the
rest of the lease API).

```bash
# Create/ensure an environment (201 created, 200 existing)
curl -s -X POST https://127.0.0.1:8890/api/environments \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"repo":"lacy/repo","ref":"123","image":"dev-base"}'

# List (optionally filtered by repo/ref)
curl -s "https://127.0.0.1:8890/api/environments?repo=lacy/repo" \
  -H "Authorization: Bearer $TOKEN"

# Tear down
curl -s -X DELETE "https://127.0.0.1:8890/api/environments?repo=lacy/repo&ref=123" \
  -H "Authorization: Bearer $TOKEN"
```

## ctl / spoondctl

```bash
ssh ctl@<host> -p 2222 "env ls"
ssh ctl@<host> -p 2222 "env new lacy/repo 123 dev-base"
ssh ctl@<host> -p 2222 "env id lacy/repo 123"     # → {"sandbox_id":"..."}
ssh ctl@<host> -p 2222 "env rm lacy/repo 123"

spoondctl env new lacy/repo 123
spoondctl env id lacy/repo 123
spoondctl env rm lacy/repo 123
```

The returned `sandbox_id` works with every existing verb
(`exec`, `ssh`, `stat`, `rm`, `cp`, …).

## Metrics

| Metric | Meaning |
|---|---|
| `spoond_environments_active` | live environments |
| `spoond_environments_created_total` | cumulative creates |
| `spoond_environments_teardown_total` | cumulative teardowns |

## Scope & follow-ups

The environment is a *lifecycle + addressability* layer: it provisions a
sandbox and keeps it alive across a PR's lifetime. It does **not** yet
auto-checkout the PR's working tree into the guest — CI images already
have the repo checked out by the runner, and dev images can run an
`init_cmd`/`exec` to clone. Wiring a configurable post-create init command
into `ensureEnv` is a natural follow-up (see the issue tracker).

Tear-down on PR close relies on the webhook being delivered; a periodic
sweep of stale environments (PR closed but webhook missed) is a hardening
follow-up.