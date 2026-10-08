# Image inquiry

How the agent (and you) decide what image a repo needs — and how to keep
the image set from proliferating.

## The principle

**An image is a job capability, not a repo.** You don't give each project
its own toolbox. There's a small set of shared toolboxes, and a workflow
file says which one to grab.

Two axes, and every image is one cell:

| | Language bases (one per toolchain) | Function images (one per job type) |
|---|---|---|
| What's in it | Minimal toolchain: `go`, `rustc`, `elixir`, `python` | Cross-cutting tooling: LLM CLI, git, linters |
| Used by | Build/test/CI jobs | Jobs that do the same thing regardless of language |
| Examples | `go-base`, `rust-base`, `elixir-base`, `py-base` | `llm-review`, `deploy`, `lint` |

**The LLM review case is the key one.** A code-review job on a Go repo and
one on an Elixir repo both just need an LLM CLI + git. The commands are
effectively identical. So there's **one `llm-review` image**, never
`go-llm-review` / `elixir-llm-review`.

## The rules that prevent proliferation

1. **Name by capability, not by repo or project.** `go-base`, not
   `jason-go-project`. The name makes the shared-ness visible.
2. **One image per language, one per function.** Many Go repos → one
   `go-base`. Many review jobs → one `llm-review`.
3. **Don't version images** (`go-base:v1`). Re-build in place when the
   toolchain needs updating. Versioning is a proliferation trap.
4. **An image does one job type.** If a job must compile Go *and* run an
   LLM review, that's two jobs (build on `go-base`, review on
   `llm-review`) — not one fat image.

## The engagement flow

Most repos won't have a workflow yet (this is a new capability). So when
you point the agent at a repo, the flow is:

1. **Interrogate** — detect the repo's language/capability from its files
   (`go.mod`, `Cargo.toml`, `mix.exs`, `pyproject.toml`, etc.).
2. **Create the workflow** — write `.forgejo/workflows/*.yml` with the
   right `runs-on` labels for the repo's jobs.
3. **Image inquiry** — run the validation script to confirm a baked image
   covers the labels, or that a new one is needed.
4. **Assign or create** — if covered, use the existing image. If not,
   build a new one (named by capability) and update the manifest.

## The validation script

```bash
# local repo
python3 images/validate-image.py /path/to/repo

# remote repo (clones it)
python3 images/validate-image.py https://git.example.com/org/repo.git
```

Output:
- **COVERED** (exit 0) — a baked image exists; use it
- **NEEDS** (exit 1) — no baked image; create one (name suggested)
- **UNKNOWN** (exit 2) — couldn't detect; needs human input

## The manifest

`images/manifest.yaml` is the source of truth for baked images. It lists
every tag, its capability, its `runs-on` labels, and whether it's baked.
**Keep it in sync with the image catalog** — when you build a new image,
mark it `baked: true` here and add the tag to `IMAGE_MAP` on the runner.
Images are part of the SQLite catalog (`spoond images list`), so the
catalog is the runtime source of truth.

## Building a new image

Images are Dockerfiles under `images/`, described by the manifest, built
into E2B templates with `spoond images build` (see
[U07](../docs/plans/2026-09-30-e2b-substrate/U07-image-pipeline.md) and
[ci-jobs.md](../docs/ci-jobs.md)):

```bash
spoond images build <name> --manifest images/manifest.yaml --context images
```

Then:
1. Add `<label>=<name>` to `IMAGE_MAP` in `/etc/spoond-runner.env`
2. Mark `baked: true` in `images/manifest.yaml`

### Building Rust images

Rust images need special attention due to the size of the toolchain and
build artifacts. See the `rust-base` entry in `manifest.yaml` for
detailed notes. Key requirements:

- **Rootfs**: 8+ GiB (`disk_mb: 8192`). The Rust toolchain (~1.5 GiB)
  + cargo registry + build artifacts for `cargo test` exceed 4 GiB.
- **Memory**: 4+ GiB (`memory_mb: 4096`). 512 MiB OOM-kills `cargo
  check` during tokio compilation.
- **python3**: Required by the guest init hooks. Base the image on a
  Docker image that ships it, or `apt-get install` it in the Dockerfile.
- **rustup**: Docker `rust:*` images ship rustup without a default
  toolchain. Run `rustup default stable` inside the sandbox **before**
  the template snapshot so the toolchain is pre-installed and rustup
  doesn't try to download the latest stable at runtime. If the project
  has a `rust-toolchain.toml` with `channel = "stable"`, rustup will try
  to download the latest stable on first build — ensure it fits on the
  rootfs or pre-install it during the build.
- **PATH**: Ensure the manifest's `env.PATH` includes
  `/usr/local/cargo/bin` so exec commands reach the toolchain (the guest
  agent does not apply `/etc/environment`).

After building, run `rustup default stable` inside the sandbox before
the snapshot so the stable toolchain is pre-installed.

## Worker images (`<base>-worker`)

A **worker image** is a base image plus the agent loop: Pi, Agent Mail,
and `images/worker-start.sh`. `images/worker.dockerfile` builds one from
any catalog image, and `images/worker.manifest.yaml` declares the entries
(the `go-base-worker` image used for Go projects). The manifest entry
inherits the base's shape and env and can add a warm step, so the
project's dependencies are cached into the image before the snapshot.

When a worker takes a task it now:

1. **Never loses uncommitted work.** Before it rebases, `worker_rebase`
   commits any dirty file as a wip commit (`worker_commit_dirty`), so a
   rebase cannot fail on a dirty tree and a retried attempt's
   `reset --hard`/`clean` cannot wipe the tail of the implement round.
2. **Rebases onto the task's `Base:` before verifying** (default
   `origin/main`), fetching it first. A conflicted rebase is handed to
   the implementer as an extra implement round: it sees the conflict and
   the base's new commits, resolves them, finishes the rebase, and every
   gate is rerun. The [`worker-git.sh`](worker-git.sh) helper holds this
   logic (`worker_fetch`, `worker_rebase`, `worker_commit_dirty`,
   `worker_rebase_in_progress`) so it can be tested without a model.
3. **Guards migrations after the rebase.** `worker_migration_guard`
   fails the gate when two files under `store/migrations` share a
   version number, or when a migration the branch adds is not numbered
   above the base's highest. This is what a rebase before verify is for:
   a branch that started from an older base can no longer land a
   migration that collides with one already merged.
4. **Verifies with a scope and a bound.** The verifier reviews only
   `git diff <base>...HEAD` plus the task text — never the whole repo
   history. Each verify round has a wall-clock limit (`VERIFY_TIMEOUT`,
   default 20 minutes, or a task's `Verify-Timeout:` line in minutes),
   and the number of rounds is sized from the diff: under ~200 changed
   lines gets one verify round, larger diffs up to `SWARM_MAX_ROUNDS`.
   The cap counts verifies, not loop iterations: an implement round whose
   gates fail goes straight to another implement pass without spending a
   verify round. On timeout the worker reports `BLOCKED` at once with the
   partial notes instead of spending a second identical try. The
   verifier's prompt asks for findings first, gates second. A verdict of
   `PASS` whose findings still list a `BLOCKER:` is downgraded to `FAIL`,
   so it cannot leave the loop as a `DONE`.
5. **Waits out the model gateway without spending the verify clock.**
   The gateway serves 4 concurrent committed requests and bursts to 8
   from an as-available queue (slower). While the gateway answers
   `429`/`503` — queueing rather than serving — or Pi reports an
   automatic retry in flight (its `auto_retry_start` event), the pass's
   wall-clock limit does not run down (`run_pass` samples
   `SWARM_LLM_HEALTH_URL`, default the gateway's `/v1/models`), and the
   waited time is reported as its own `llm wait` bucket. `SWARM_LLM_MAX_WAIT` (seconds, default
   3600, 0 = unbounded) caps the total queue wait so a stuck gateway
   cannot hang a pass forever. Per-request model timeouts are generous
   (`SWARM_LLM_REQUEST_TIMEOUT`, default 600 s) and transient
   `429`/`503`/timeouts are retried with backoff inside Pi, so a slow
   response never fails the task. The first `amail register` at boot
   retries with backoff (five tries, `SWARM_REGISTER_BACKOFF` base) so a
   single DNS or network blip does not kill the worker.
6. **Reports the base, the timings and an honest empty result.** A
   `[DONE]` names the base commit it was verified on (`verified on base:
   <sha> (<base ref>)`); when the push-stage rebase moved the base after
   the verify it also says `re-gated on <new base>`. `[DONE]` and
   `[BLOCKED]` both carry `timings: implement …, rebase …, gates …,
   verify …, llm wait …` so the next optimisation is measured, and a
   `[BLOCKED]` that never reached a verifier says `no verify ran: gates
   failed` instead of claiming N verify rounds. When the rebase drops
   every branch commit (the change is already on the base) the worker
   reports an honest `[DONE] already on base <sha>; nothing to push` with
   the verifier's verdict, not a `verifier did not pass` block.

### Testing the worker loop

`images/worker-git_test.sh` covers the rebase and migration rules in
throwaway local repos (no model, no Agent Mail, no network):

```bash
bash images/worker-git_test.sh
```

`images/worker-start_test.sh` runs the whole worker for real inside a
mount namespace (`/work` and `/root` private tmpfs, `pi` and `amail`
stubs on `PATH`, a local bare origin) and asserts on what lands on origin
and in the mail: the PASS/BLOCKED/cancelled exit paths, the `-wip`
branches for rewritten history, `Base:`, rebase-before-verify with a
moving base, the one-round/two-round sizing, verify timeouts (including
one that writes PASS but runs out of clock), conflict resolution, the
push-stage fetch failure, the migration guard, an empty-after-rebase
`DONE`, a `PASS` downgraded by a listed `BLOCKER:`, the re-gated `DONE`
when the base moves during the verify, a queued gateway that does not
spend the verify clock, a dirty tail committed before the rebase, and
the boot-registration retry.

```bash
bash images/worker-start_test.sh
```

The scripts are expected to be `shellcheck`-clean, and the worker image
installs `shellcheck` so the gate can run there:

```bash
bash images/shellcheck.sh          # runs shellcheck when installed
```
