# Writing CI jobs for the spoond runner

How a Forgejo workflow selects its sandbox, and what a job script may and
may not rely on inside one. This is the contract between a repo's workflow
and the sandboxes the runner hands it. What a sandbox *is* — a Firecracker
microVM restored from a memory snapshot of an E2B template — is
[substrate.md](substrate.md).

## Selecting an image: `runs-on`

The runner maps a job's `runs-on` label to an image name via `IMAGE_MAP`
(label=name, comma-separated). First matching label wins; a label that is
not in the map falls through to `DEFAULT_IMAGE`. Neither has a default:
a job whose labels map through neither fails before a sandbox is
created, with an error naming `IMAGE_MAP` and `DEFAULT_IMAGE`, and a
runner with both unset warns at startup.

For example (the image names stand for images your backend serves):

```
RUNNER_LABELS=ubuntu-latest,go,release
IMAGE_MAP=ubuntu-latest=ci-image,go=go-ci-image,release=release-image
DEFAULT_IMAGE=ci-image
```

So `runs-on: release` gets the `release-image` image. This is the
only job-side knob: no workflow needs to know about the orchestrator,
templates, builds or leases.

**Footgun:** with `DEFAULT_IMAGE` set, an unmapped or mistyped label
silently gets it, and the job then fails somewhere unrelated with a
missing toolchain. If a job needs a capability image, use a label from
`IMAGE_MAP` and add the mapping on the host rather than relying on the
default; leave `DEFAULT_IMAGE` unset to make an unmapped label fail at
once instead.

The image must have a current build in the catalog; otherwise the grant
fails with `unknown image tag` rather than falling back.

A value can also name a **named snapshot** instead of an image, so a job
starts from a VM someone prepared and saved (a toolchain and warm
dependency caches already in place):

```
IMAGE_MAP=web=snapshot:scout/node24,yaml=snapshot:yaml@3,shared=snapshot:alice:ci/base
```

- `snapshot:NAME` starts from the newest version of a snapshot the
  runner's own identity (`LEASE_TOKEN`) saved; `snapshot:NAME@V` from
  version V.
- `snapshot:OWNER:NAME` starts from another user's snapshot, through a
  grant that user gave the runner's identity. Without one the create
  answers 404, like an unknown name.
- `DEFAULT_IMAGE` takes the same forms.

The snapshot is the job's environment from then on: whoever saves it owns
what CI runs on. Replacing or deleting it changes or breaks those jobs at
once, and spoond does not warn.

## Images come from Dockerfiles

There are no bake scripts in the pipeline, no rootfs assembly and no
snapshot tags to re-bake: images are built from Dockerfiles by
`spoond images build`. An image is:

1. a Dockerfile in `images/` (`images/<name>.dockerfile`);
2. an entry in `images/manifest.yaml` with `baked: true`, the sizing
   (`vcpu`, `memory_mb`, `disk_mb`) and the env every sandbox gets;
3. a build: `spoond images build <name>` (or `--all`), which builds it
   with docker, pushes it to the local registry (`IMAGE_REGISTRY`,
   default `localhost:5000`) and has the orchestrator's template manager
   produce an E2B **template build** — a booted, snapshotted microVM. The
   build and its digest are recorded in the catalog
   (`spoond images list`, or `GET /api/images?detail=1`), together with
   the build's disk size, measured when the build is written and
   refreshed by the hourly disk accounting pass.

The entry's `capability` and `description` are recorded in the catalog
row too, and `GET /api/capabilities` serves them from there (an entry
built `from` another image does not inherit them). To set them on images
that are already built, without rebuilding, run
`spoond images describe <name>` (or `--all`, every manifest entry) with
the same `--manifest`: it writes the two fields into the existing rows
and skips, with a note, any entry that has no row yet.

Builds run on the host as root, take minutes each, and are idempotent
per name: the template id is stable for the life of the image name, and
each build records a new `build_id` plus the Firecracker/kernel/envd
versions it was made with. A failed build leaves the previous current
build in place, so grants keep working — fix the Dockerfile and rebuild.

Two constraints inherited from the substrate:

- **Base images must be Debian/Ubuntu, Fedora-family, Arch, Alpine or
  NixOS.** E2B's provisioning rejects RHEL-family images (that is why
  `scylla` is built from Debian 12 with ScyllaDB's apt repository rather
  than the upstream RHEL UBI image).
- **`memory_mb` is the image's default size.** A job always runs at it
  (jobs take no size); a lease may ask for another size on create
  (3.1, #151), which cold-boots unless the image has a warm build at
  that size. Size the manifest entry for jobs.

The common Dockerfile tail (`images/guest/spoond-guest-init`) is what
runs at template-build time and ends up in every sandbox's restored
memory and disk: it writes `/etc/resolv.conf` with the build-time
`SPOOND_GUEST_DNS_ADDR` (comma-separated, one `nameserver` line per
address plus short timeouts and retries; it leaves the image's own
resolver alone when that is empty), empties dpkg's statoverride (so
tools that unpack a base image over the live root, such as kaniko, do
not abort apt), drops
`/.dockerenv` so container-aware tools behave, runs the
`/etc/spoond/init.d/*` hooks (the scylla service, for instance) and then
waits on a ready file. The snapshot is taken after the ready command
succeeds.

## The rust-gpui image

`rust-gpui-base` (`runs-on: rust-gpui`) is `rust-qt-base` with what an
app on gpui 0.2.2 (Zed's GUI crate) needs to build and test on Linux:
libxcb, libxkbcommon and libxkbcommon-x11 (linked), wayland and
wayland-protocols, the Vulkan loader and headers, and Mesa's lavapipe
(`vulkan-swrast`) as a software Vulkan driver. gpui renders only through
Vulkan, so a test that opens a real window runs under `xvfb-run` on
lavapipe; tests on gpui's `TestPlatform` need no display. It is a
`from: rust-qt-base` layer, so it keeps that image's Arch snapshot,
Rust 1.99.0 and env. It is sized at 4 vCPU, 8 GiB and 24 GiB of disk,
because a cold gpui release build needs about 5 GB of target dir.

Deploying it on the host:

1. Check that `rust-qt-base` has a current build (`spoond images list`),
   then build the layer from a checkout that has it:

   ```bash
   spoond images build rust-gpui-base --manifest images/manifest.yaml --context images
   ```

2. Add the label and the mapping in `/etc/spoond/runner.env` (append to
   the existing lists; labels take no spaces):

   ```
   RUNNER_LABELS=<existing labels>,rust-gpui
   IMAGE_MAP=<existing pairs>,rust-gpui=rust-gpui-base
   ```

3. Make the runners register again, as in [Adding a label](#adding-a-label).

### Adding a label

A runner sends its labels to Forgejo only when it registers. Each worker
saves its Forgejo credentials in `RUNNER_STATE_FILE` (default
`/var/lib/spoond/runner-state.json`), and on restart a worker with saved
credentials restores them and goes straight to fetching jobs
(`runner/pool.go`, `worker.run`); nothing re-declares labels. So a plain
restart after changing `RUNNER_LABELS` leaves Forgejo with the old labels,
and jobs for the new label sit waiting. Workers spawned fresh by scale-up
do register with the new labels, which makes this intermittent.

To re-register every worker, while the runner is idle:

```bash
systemctl stop spoond-runner
mv /var/lib/spoond/runner-state.json /var/lib/spoond/runner-state.json.bak
systemctl start spoond-runner
```

The old runner entries stay in Forgejo, offline. With
`FORGEJO_ADMIN_TOKEN` set, a later start deletes offline runners named
with the `RUNNER_NAME` prefix; without it, delete them under Site
Administration > Actions > Runners. That page also shows each runner's
labels, which is where to check that the new one arrived. Delete the
`.bak` file once the runners show the new label.

## Writing inside the sandbox

A job gets an ordinary writable root filesystem, its own copy-on-write
rootfs over the image, and fixed memory. Check out and build wherever
you like: `/`, `$HOME` and `/tmp` all behave the way they do on a normal
machine, and nothing about a job's write paths needs to know how the
sandbox was created.

What is worth knowing:

- **Writes are private to the sandbox and last as long as it does.** A
  job cannot seed the next job by writing to disk — the image is never
  modified. Warm caches inside one sandbox last only as long as that
  sandbox.
- **Memory is the image's `memory_mb`.** A job that exceeds it is OOM-killed
  by the guest kernel; there is no per-job memory override. If a toolchain
  needs more, the image needs rebuilding with a bigger `memory_mb`.
- **Disk is the image's `disk_mb` of free space in its own rootfs.**
  Filling it fails that job only.
- **Every start is a snapshot restore**, so a job's first command runs
  with the services the template already had running (the scylla image's
  database, for example) — no `sleep`-until-ready in the workflow.

Already handled by the runner, so jobs need not:

- `CI=true` is set in the step environment (interactive prompts otherwise
  hang on `/dev/console`, which never EOFs).
- Step exec timeouts: `EXEC_TIMEOUT_SECS` / `MAX_EXEC_TIMEOUT_SECS` on the
  backend (default 300; the production host sets both to 5400 to cover
  long builds). Do not reintroduce a ceiling by hardcoding a shorter timeout
  in a step.
- A database or peer service is a separate lease with `expose_ports`.
  Whether the job can reach it depends on the job's own policy: the
  runner's default (`LEASE_NETPOL`, `internet`) reaches every exposing
  peer's published ports with no opt-in; under `restricted` the job's
  allowlist must name that lease (by id, name or `lease:<id>`) to reach
  them. See [api.md](api.md#network-policy).

## Host prerequisites

- The E2B host: orchestrator, firewall, registry and hugepages sized for
  the sum of every concurrent sandbox's `memory_mb`. See
  [install.md](install.md).
- `GC_DELETE=1` when you want unreferenced builds reclaimed; the default
  dry-run only logs them. See [operations.md](operations.md).
- Nothing else. There are no per-image netns pools to provision and no
  rootfs cache to size: network slots and rootfs overlays are created per
  sandbox by the orchestrator.

## Using the concurrency the sandbox layer allows

Concurrency is bounded by host configuration, not by workflow content:

- `RUNNER_MAX` is the number of **registered runners** (not sandboxes).
- Sandboxes from different images were always safe to run concurrently;
  sandboxes from the same image are too, since each has its own rootfs
  overlay and its own memory. Raising `RUNNER_MAX` is safe up to the
  host's hugepage budget — each concurrent job holds its image's
  `memory_mb` for its whole lifetime.
- The admission check refuses a grant with `503 capacity` rather than
  overcommitting, so an over-subscribed host makes jobs queue rather than
  die; the runner retries.

## When a build fails, look for the job record

A failed job writes `/var/lib/spoond/jobs/job-<id>.json` naming the step
that died, its exit code, and the tail of its output. Forgejo exposes no
readable log API, so without this a red run reaches the consumer as a
bare `failure` — no step name, no exit code, nothing to act on. The
runner's own journal line carries the same fields:

```
executor: job 1729 final result=1 steps=5 failed_step=4(build) exit=1
```

Records are written on failure only and capped at 500 files. `JOB_RECORD_DIR`
moves them (`/var/lib/spoond/jobs` by default); empty disables writing.

Workflow authors should not need to add their own failure logging for
this: the runner already has the step name, exit code and output at the
point it fails. If a red build still leaves you guessing, that is a gap
in the runner, not something to route around in the workflow.

## The sandbox integrity probe

Every sandbox is checked from the inside before it is pooled or leased
(`SANDBOX_PROBE`, on by default; `SANDBOX_PROBE=0` disables;
`SANDBOX_PROBE_TIMEOUT_SECS` bounds it, default 20 s). This exists
because a sandbox can carry a corrupted toolchain that is invisible from
outside: it answers a health check, `uname --version` exits 0, and the
job then fails 48 seconds into a dependency build with an error that
names neither the sandbox nor the corruption.

The probe checks **behaviour, not exit status**, because a swapped binary
is a valid ELF of plausible size that exits 0. Observed carriers, and
what the probe does with each:

| sandbox state | `uname --version` | probe |
|---|---|---|
| healthy | `uname (GNU coreutils) 9.1` | `PROBE_OK` |
| `uniq` swapped into `/usr/bin/uname` | `uniq (GNU coreutils) 9.1`, exit 0 | `PROBE_FAIL uname -s -> option requires an argument -- 's'` |
| garbage bytes | exec format error | `PROBE_FAIL` |

A failing sandbox is deleted on the spot and the grant either retries or
fails with a message naming the probe — a bad sandbox is never handed to
a job. Because a bad sandbox means a bad build of the image, the remedy
is `spoond images build <name>` again, not attention to the sandbox
layer. It is a detection net, not a fix.
