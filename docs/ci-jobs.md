# Writing CI jobs for the spoond runner

How a Forgejo workflow selects its sandbox, and what a job script may and may
not rely on inside one. This is the contract between a repo's workflow and the
sandboxes the runner hands it.

## Selecting an image: `runs-on`

The runner maps a job's `runs-on` label to an image tag via `IMAGE_MAP`
(label=tag, comma-separated). First matching label wins; a label that is not
in the map falls through to `DEFAULT_IMAGE`.

Deployed on `sandbox`:

```
RUNNER_LABELS=forkd,ubuntu-latest,go,golang,elixir,elixir-base,llm-review,elixir-release,release
IMAGE_MAP=ubuntu-latest=py-base,go=go-base,golang=go-base,elixir=elixir-base,\
elixir-base=elixir-base,llm-review=llm-review,dev=dev-base,\
elixir-release=elixir-release,release=elixir-release
DEFAULT_IMAGE=py-base
```

So `runs-on: elixir-release` gets the `elixir-release` tag. This is the only
job-side knob: no workflow needs to know about forkd, snapshots, or leases.

**Footgun:** an unmapped or mistyped label silently gets `py-base`, and the
job then fails somewhere unrelated with a missing toolchain. If a job needs a
capability image, use a label from `IMAGE_MAP` and add the mapping on the host
rather than relying on the default.

## Writing inside the sandbox

A job gets an ordinary writable root filesystem. Check out and build wherever
you like: `/`, `$HOME` and `/tmp` all behave the way they do on a normal
machine, and nothing about a job's write paths needs to know how the sandbox
was created.

What is worth knowing is *where those writes go*. A snapshot's rootfs is shared
by every sandbox restored from that tag, so each sandbox is given its own copy
before it runs, and a job's writes land in that copy rather than in the shared
image. Three consequences:

- **Writes are private to the sandbox and last as long as it does.** They
  survive a BRANCH (they are part of `memory.bin` and of the sandbox's own
  rootfs) but they never enter the tag's image, so a job cannot seed the next
  job by writing to disk. Warm caches persist across jobs the way they always
  have: the pool reuses sandboxes, and a reused sandbox keeps its own copy.
- **Disk, not RAM, bounds a job's writes.** A build that fills the disk of the
  sandbox's rootfs fails that build; it can no longer fill the host's disk for
  everyone, which was the failure mode that used to corrupt a whole pool.
- **The sandbox's copy is reclaimed when the sandbox is.** Draining the pool
  therefore releases a pool's accumulated writes, which is what makes the disk
  guard's drain effective.

Already handled by the runner, so jobs need not:

- `CI=true` is set in the step environment (interactive prompts otherwise hang
  on `/dev/console`, which never EOFs).
- Step exec timeouts: `EXEC_TIMEOUT_SECS` / `MAX_EXEC_TIMEOUT_SECS` (both 5400
  on `sandbox`), with `FORKD_HTTP_TIMEOUT_SECS` on the backend raised to match.
  These were four stacked 10-minute ceilings; do not reintroduce one by
  hardcoding a shorter timeout in a step.

## Host prerequisites

- **Firecracker ≥ 1.15.** Restores use `PATCH /drives` on a restored VM to give
  each sandbox its own rootfs, and older builds accept the call without moving
  the device's storage — so the minimum is enforced rather than advisory. On an
  older build a restore fails and the sandbox is not started, rather than
  running against a shared rootfs.
- **A reflink-capable filesystem for the rootfs cache** (XFS/btrfs, or ZFS 2.2+
  block cloning) if per-sandbox copies are to be free. Elsewhere each spawn
  costs a full copy of the rootfs, which makes a large `POOL_SIZE` expensive.
- **Provisioned netns** for multi-child restores: `scripts/netns-setup.sh N`.
  `POOL_SIZE × len(KNOWN_IMAGES)` is the pool's desired size and must fit inside
  the namespaces available, or spawning outside the pool fails with
  `netns pool exhausted`.

## Using the concurrency the sandbox layer allows

Concurrency is bounded by host configuration, not by workflow content:

- `RUNNER_MAX` is the number of **registered runners** (not sandboxes).
  `sandbox` runs `RUNNER_MAX=1`, so jobs are serialized today. `RUNNER_FLOOR`
  above `RUNNER_MAX` is contradictory and is clamped up to the floor now
  (`runner/pool.go`); it used to register only one runner and silently cap
  every job behind it.
- Sandboxes restored from *different* tags were always safe to run
  concurrently — different images, different files. Same-tag concurrency is
  safe now too, since each sandbox has its own rootfs copy; raising
  `RUNNER_MAX` and provisioning netns accordingly is what unlocks it.

## When a build fails, look for the job record

A failed job writes `/var/lib/spoond/jobs/job-<id>.json` naming the step that
died, its exit code, and the tail of its output. Forgejo exposes no readable
log API, so without this a red run reaches the consumer as a bare `failure` —
no step name, no exit code, nothing to act on. The runner's own journal line
carries the same fields:

```
executor: job 1729 final result=1 steps=5 failed_step=4(build) exit=1
```

Records are written on failure only and capped at 500 files. `JOB_RECORD_DIR`
moves them (`/var/lib/spoond/jobs` by default); empty disables writing.

Workflow authors should not need to add their own failure logging for this:
the runner already has the step name, exit code and output at the point it
fails. If a red build still leaves you guessing, that is a gap in the runner,
not something to route around in the workflow.

## The sandbox integrity probe

Every sandbox is checked from the inside before it is pooled or leased
(`SANDBOX_PROBE`, on by default; `SANDBOX_PROBE=0` disables). This exists
because a sandbox can carry a corrupted toolchain that is invisible from
outside: it answers a ping, `uname --version` exits 0, and the job then fails
48 seconds into a dependency build with an error that names neither the
sandbox nor the corruption.

The probe checks **behaviour, not exit status**, because a swapped binary is a
valid ELF of plausible size that exits 0. Observed carriers, and what the
probe does with each:

| sandbox state | `uname --version` | probe |
|---|---|---|
| healthy | `uname (GNU coreutils) 9.1` | `PROBE_OK` |
| `uniq` swapped into `/usr/bin/uname` | `uniq (GNU coreutils) 9.1`, exit 0 | `PROBE_FAIL uname -s -> option requires an argument -- 's'` |
| garbage bytes | exec format error | `PROBE_FAIL` |

A failing sandbox is killed on the spot and the grant either retries from the
pool or fails with a message naming the probe — a bad sandbox is never handed
to a job. It is not a fix for whatever corrupts an image, only a way to stop
one from silently costing a debugging cycle; `docs/operations.md` covers the
image side.
