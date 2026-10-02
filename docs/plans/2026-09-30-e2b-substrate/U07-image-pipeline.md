# U07 — Image pipeline: Dockerfiles → local registry → E2B templates

## Purpose

Every spoond image is defined by a Dockerfile in `images/`, built with
docker, pushed to the local registry, and turned into an E2B template
build. The build is recorded in SQLite. This replaces forkd's bake scripts.

## Preconditions

- U04 is done: registry `127.0.0.1:5000` is up and the orchestrator is healthy.
- U06 is done: `substrate/e2b` passes its live test.

## Step 0: spoond checkout and staging binary on vm2 (Ops runner)

Run after the unit's commits are merged into `feat/e2b-substrate`, and
before the vm2 tests below:

```bash
export PATH=/usr/local/go/bin:$PATH
test -d /root/src/spoond || git clone https://code.lacy.casa/lacy.casa/spoond.git /root/src/spoond
cd /root/src/spoond && git fetch && git checkout feat/e2b-substrate && git pull --ff-only
install -d -m 755 /opt/spoond-staging
go build -o /opt/spoond-staging/spoond ./cmd/spoond
/opt/spoond-staging/spoond images list --db /var/lib/spoond/staging.db
```

The last command creates `/var/lib/spoond/staging.db` (migrations run on
open) and prints an empty list. Every vm2 command in U07–U11 uses
`/opt/spoond-staging/spoond` and the checkout `/root/src/spoond`.

## Facts relied on

- **The manifest** (`images/manifest.yaml`, A1 §10.1) has top-level `images:`,
  a list of entries.
  - Its fields are `name`, `capability`, `labels`, `description`, `baked`,
    `rootfs` and `notes`.
  - `images/validate-image.py` reads `name`, `capability` and `baked` only.
- **Images in scope:**
  - `py-base`
  - `go-base`
  - `dev-base`
  - `elixir-base`
  - `elixir-release`
  - `llm-review`
  - `scylla`

  `rust-base` is `baked: false` and stays unbuilt (out of scope).
- **Existing Dockerfiles:**
  - `images/py-base.dockerfile` (FROM `python:3.12-slim`);
  - `images/elixir-release.dockerfile` (FROM `elixir:1.18.4-otp-27`);
  - `images/scylla.dockerfile` (FROM `scylladb/scylla:2026.2.6`, **RHEL 9.8
    UBI, which E2B rejects**).

  The others were built by bake scripts from `golang:1.25` (now 1.27.1),
  `ubuntu:24.04`, `elixir:1.17` and `python:3.12-slim` (A1 §10.1 notes).
- **E2B provisioning:**
  - It supports Debian/Ubuntu, Fedora-family, Arch, Alpine and NixOS, and
    rejects RHEL (A2 §6, A3).
  - It installs systemd, openssh-server, sudo, chrony, socat, curl,
    ca-certificates, fuse3, iptables, git and nfs-common when missing.
  - Guest `/etc/resolv.conf` is `8.8.8.8`, written at build.
- **Template commands:**
  - they run as `/bin/bash -l -c <cmd>` as root;
  - the ready command retries every 2 s for up to 10 minutes;
  - the snapshot is taken after ready succeeds (A2 §6).
- **DNS:** guests must resolve `code.lacy.casa` to the LAN edge (the
  registry path there is not SSO-gated). vm2's LAN resolver is `10.1.0.1`.
  The forkd guest init listed LAN resolvers first (A1 §10.5,
  `elixir-release.dockerfile` final note).
- **dev-base behaviour to keep:**
  - `/etc/profile.d/forkd-tmux.sh` attaches or creates tmux session `dev` on
    SSH login, when `$SSH_CONNECTION` is set and `$FORKD_NO_TMUX` is unset
    (`deploy/rebuild-dev-base.sh`).
  - `sshd` is present.
- **Base tags** verified to exist on 2026-09-30: `golang:1.27.1`,
  `elixir:1.17.3-otp-27`, `elixir:1.18.4-otp-27`, `ubuntu:24.04`,
  `python:3.12-slim`, `debian:12`, `rust:1-bookworm`,
  `node:22-bookworm-slim`.
- **ScyllaDB apt repository:**
  - list file:
    `https://downloads.scylladb.com/deb/debian/scylla-2026.2.list`;
  - signing key fingerprint `6C6ECC84F42AF147BD2A65AEC503C686B007F39E`, on
    `keyserver.ubuntu.com`;
  - package version `2026.2.6-0.20260824.c06236b53803-1` is available.

## Deliverables

1. `images/guest/spoond-guest-init` (new, exact below).
2. The Dockerfiles below, each in `images/<name>.dockerfile`.
3. `images/manifest.yaml`, rewritten per the schema below.
4. The Go package `cmd/spoond-images` (`Main(args []string) int`), registered
   as subcommand `images` in `cmd/spoond/images.go` with build tag
   `//go:build !noimages`. In `cmd/spoond/main.go`, add `"images"` to the
   `usage()` command list, and add `noimages` to the build-tags line of
   `usage()` and to the "Supported exclusion tags" line of the package doc.
5. A store API for images, builds and sandboxes in `store/catalog.go`.

## `images/guest/spoond-guest-init` (exact; mode 0755)

```bash
#!/bin/bash
# spoond-guest-init — E2B template start command. Runs once while the
# template is built; the template is snapshotted after the ready command
# (test -f /run/spoond-guest-ready) succeeds, so everything done here is in
# every sandbox's restored memory and disk.
set -u
# Resolve only through the LAN resolver (Technitium). No public fallback:
# public DNS answers *.lacy.casa with the public edge, where credentialed
# calls must never go, and the router (10.1.0.1) gives stale LAN answers.
rm -f /etc/resolv.conf
printf 'nameserver 10.1.0.2\noptions timeout:2 attempts:3\n' > /etc/resolv.conf
# E2B's template provisioning installs chrony, which adds dpkg statoverride
# entries for the _chrony group. Tools that unpack a base image over the
# live root (kaniko) keep this file while replacing /etc/group, and apt then
# fails on the unknown group. The image's own entries stay.
if [ -f /var/lib/dpkg/statoverride ]; then
  sed -i '/ _chrony /d' /var/lib/dpkg/statoverride
fi
# A container marker, as forkd's docker-export rootfs had: container-aware
# tools (kaniko) otherwise warn that they run outside a container.
: > /.dockerenv
for h in /etc/spoond/init.d/*; do
  [ -x "$h" ] || continue
  echo "spoond-guest-init: running $h"
  if ! "$h"; then
    echo "spoond-guest-init: hook $h failed" >&2
    touch /run/spoond-guest-failed
    exec sleep infinity
  fi
done
touch /run/spoond-guest-ready
exec sleep infinity
```

Every image uses start command `/usr/local/bin/spoond-guest-init` and ready
command `test -f /run/spoond-guest-ready`.

## Common tail (append to every Dockerfile, exactly)

```dockerfile
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
RUN mkdir -p /etc/spoond/init.d
```

The docker build context is the `images/` directory.

## Dockerfiles

**`py-base.dockerfile`:** keep the existing file unchanged, and append the
common tail.

**`elixir-release.dockerfile`:** keep the existing file unchanged, append
the common tail, and change only the comment block that mentions
`forkd from-image` bake commands to:

```
# Build: spoond images build elixir-release   (U07; E2B template)
```

**`go-base.dockerfile`** (new):

```dockerfile
# go-base — Go toolchain for build/test/CI jobs (capability: golang).
FROM golang:1.27.1
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
 && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
 && go version | grep -q 'go1.27.1'
```

Then the common tail.

**`dev-base.dockerfile`** (new):

```dockerfile
# dev-base — interactive development sandbox (capability: interactive-dev).
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      tmux openssh-server git python3 build-essential ca-certificates curl locales \
 && mkdir -p /run/sshd \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 guest/spoond-tmux.sh /etc/profile.d/spoond-tmux.sh
```

Then the common tail. Create `images/guest/spoond-tmux.sh` (exact):

```bash
if [ -z "$TMUX" ] && [ -z "${SPOOND_NO_TMUX:-}" ] && [ -z "${FORKD_NO_TMUX:-}" ] && [ -n "${SSH_CONNECTION:-}" ]; then
  case "$TERM" in
    xterm-kitty|alacritty|wezterm|dumb) export TERM=xterm-256color ;;
  esac
  if tmux new -A -s dev 2>/dev/null; then
    exit 0
  fi
  echo "spoond: tmux unavailable (TERM=$TERM) — plain shell"
fi
```

**`elixir-base.dockerfile`** (new):

```dockerfile
# elixir-base — Elixir 1.17 / OTP 27 toolchain (capability: elixir).
FROM elixir:1.17.3-otp-27
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates curl build-essential \
 && rm -rf /var/lib/apt/lists/*
RUN for b in elixir mix erl git; do command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; done
```

Then the common tail.

**`llm-review.dockerfile`** (new):

```dockerfile
# llm-review — LLM code review jobs (capability: llm-review). Pure Python stdlib + git.
FROM python:3.12-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
```

Then the common tail.

**`scylla.dockerfile`** (rewritten, Debian base; the RHEL image is not
supported by E2B):

```dockerfile
# scylla — ScyllaDB as a SERVICE image (spoond #70). A job leases it with
# expose_ports [9042]. Debian 12 + ScyllaDB's apt repository (the upstream
# scylladb/scylla image is RHEL UBI, which E2B's template builder rejects).
FROM debian:12
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg python3 \
 && install -d -m 0755 /etc/apt/keyrings \
 && gpg --homedir /tmp --no-default-keyring --keyring /etc/apt/keyrings/scylladb.gpg \
        --keyserver hkps://keyserver.ubuntu.com --recv-keys 6C6ECC84F42AF147BD2A65AEC503C686B007F39E \
 && gpg --no-default-keyring --keyring /etc/apt/keyrings/scylladb.gpg --list-keys --with-colons \
      | grep -q '^fpr:::::::::6C6ECC84F42AF147BD2A65AEC503C686B007F39E:' \
 && curl -fsSL -o /etc/apt/sources.list.d/scylla.list https://downloads.scylladb.com/deb/debian/scylla-2026.2.list \
 && apt-get update -qq \
 && apt-get install -y --no-install-recommends scylla=2026.2.6-0.20260824.c06236b53803-1 \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 scylla-init-hook.sh /etc/spoond/init.d/50-scylla
```

Then the common tail. Note that `RUN mkdir -p /etc/spoond/init.d` is
harmless after the COPY.

**`scylla-init-hook.sh`:** keep the file, with exactly two changes:
- in the scylla command line, replace `--broadcast-rpc-address=10.42.0.2`
  with `--broadcast-rpc-address=169.254.0.21` (E2B's fixed guest IP);
- replace the header comment's path `/etc/forkd/init.d/50-scylla` with
  `/etc/spoond/init.d/50-scylla`, and `forkd-init.sh` with
  `spoond-guest-init`.

If `apt-get install scylla=…` does not provide `/usr/bin/scylla` and
`/etc/scylla/scylla.yaml`, STOP.

## `images/manifest.yaml` (new schema; keep the header comment, updated to describe these fields)

Each entry has these fields:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | unique image name (lease API `image`) |
| `capability` | string | unchanged meaning |
| `labels` | [string] | unchanged meaning |
| `description` | string | unchanged meaning |
| `baked` | bool | `true` for every image built by U07 (keeps `validate-image.py` working) |
| `dockerfile` | string | file name in `images/` |
| `vcpu` | int | 1 or an even number |
| `memory_mb` | int | even, ≥ 128 |
| `disk_mb` | int | free disk space (MB) guaranteed in the guest rootfs; sent as `diskSizeMB` (A2 §7.2). The final rootfs size is `rootfsSizeKey` |
| `env` | map | env vars passed on every sandbox create (process defaults) |
| `notes` | string | freeform |

Remove `rootfs` from every entry.

Values (exact):

| name | dockerfile | vcpu | memory_mb | disk_mb | env |
|---|---|---|---|---|---|
| py-base | py-base.dockerfile | 2 | 1024 | 4096 | `PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin` |
| go-base | go-base.dockerfile | 2 | 2048 | 6144 | `PATH=/usr/local/go/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin`, `GOCACHE=/root/.cache/go-build`, `GOPATH=/root/go` |
| dev-base | dev-base.dockerfile | 2 | 2048 | 8192 | `PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin`, `LANG=C.UTF-8` |
| elixir-base | elixir-base.dockerfile | 2 | 2048 | 6144 | `PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin`, `LANG=C.UTF-8` |
| elixir-release | elixir-release.dockerfile | 4 | 4096 | 12288 | `PATH=/usr/local/cargo/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin`, `RUSTUP_HOME=/usr/local/rustup`, `CARGO_HOME=/usr/local/cargo`, `LANG=C.UTF-8` |
| llm-review | llm-review.dockerfile | 2 | 1024 | 4096 | `PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin` |
| scylla | scylla.dockerfile | 2 | 3072 | 6144 | `PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin` |

Keep the existing `capability`, `labels`, `description` and `notes` text of
each entry. Keep `rust-base` with `baked: false` and no build fields.

## `store/catalog.go` (exact API)

```go
type ImageRow struct {
	Name, TemplateID, CurrentBuildID, Digest string
	VCPU, MemoryMB, DiskMB                   int
	StartCmd, ReadyCmd                       string
	UpdatedAt                                time.Time
}
func (db *DB) GetImage(ctx context.Context, name string) (ImageRow, error) // ErrNotFound
func (db *DB) ListImages(ctx context.Context) ([]ImageRow, error)
func (db *DB) UpsertImage(ctx context.Context, r ImageRow) error

type BuildRow struct {
	BuildID, Kind, TemplateID, Image, ParentBuildID, SourceSandboxID, State string
	KernelVersion, FirecrackerVersion, EnvdVersion                           string
	VCPU, MemoryMB, DiskMB                                                   int
	SizeBytes                                                                int64
	Error                                                                    string
	CreatedAt, UpdatedAt                                                     time.Time
}
func (db *DB) GetBuild(ctx context.Context, id string) (BuildRow, error) // ErrNotFound
func (db *DB) InsertBuild(ctx context.Context, r BuildRow) error
func (db *DB) UpdateBuildState(ctx context.Context, id, state, errMsg string, r *BuildRow) error // r non-nil: also copy versions/size fields
func (db *DB) ListBuilds(ctx context.Context) ([]BuildRow, error)
func (db *DB) ChildBuilds(ctx context.Context, parentID string) ([]BuildRow, error)

type SandboxRow struct {
	SandboxID, LeaseID, BuildID, ExecutionID, HostIP string // LeaseID "" = pool sandbox
	VCPU, MemoryMB                                  int
	StartedAt, EndAt                                time.Time
}
// UpsertSandbox is INSERT ... ON CONFLICT(sandbox_id) DO UPDATE SET every column:
// resume and crash recovery reuse a sandbox id.
func (db *DB) UpsertSandbox(ctx context.Context, r SandboxRow) error
func (db *DB) DeleteSandbox(ctx context.Context, sandboxID string) error // no error when absent
func (db *DB) GetSandbox(ctx context.Context, sandboxID string) (SandboxRow, error) // ErrNotFound
func (db *DB) GetSandboxByLease(ctx context.Context, leaseID string) (SandboxRow, error) // ErrNotFound
func (db *DB) ListSandboxes(ctx context.Context) ([]SandboxRow, error)
```

The `sandboxes` table is created in U05's `0001_init.sql`. U07 adds only this
API and its store tests (upsert twice with the same id, get by lease, delete
of an absent id returns nil).

## `spoond images` subcommand (exact behaviour)

Usage:
- `spoond images build <name>` builds one image;
- `spoond images build --all` builds every entry with `baked: true`;
- `spoond images list`.

Flags:
- `--manifest` (default `images/manifest.yaml`);
- `--context` (default `images`);
- `--db` (default `$SPOOND_DB_PATH`);
- `--registry` (default `$IMAGE_REGISTRY`, which is `localhost:5000`).

Runs on vm2 as root. Uses `substrate/e2b.FromEnv()`.

For `build <name>`:
1. Load the manifest entry. Error if it is missing or has no `dockerfile`.
2. Build and push with docker:
   - `docker build --pull -f <context>/<dockerfile> -t <registry>/<name>:latest <context>`;
   - then `docker push <registry>/<name>:latest`.
   - Stream the output to stdout. Any non-zero exit is fatal.
3. Get the digest:
   `docker inspect --format '{{index .RepoDigests 0}}' <registry>/<name>:latest`.
   The value has the form `<registry>/<name>@sha256:<64 hex>`. Error if
   empty.
4. Image row:
   - `GetImage(name)`. If it is missing, `UpsertImage` a new row now with
     `TemplateID = e2b.NewTemplateID()`, `CurrentBuildID = ""`,
     `Digest = ""`, sizing from the manifest,
     `StartCmd = /usr/local/bin/spoond-guest-init`,
     `ReadyCmd = test -f /run/spoond-guest-ready`, `UpdatedAt = now`.
   - If it exists, do **not** write it in this step.
   - A template id is **stable for the life of the image name**; never
     regenerate it.
5. Start the build record:
   - `buildID := e2b.NewUUID()`;
   - `InsertBuild{kind: template, state: building, template_id, image: name, vcpu/mem/disk}`.
6. `BuildTemplate` with
   `{TemplateID, BuildID, FromImage: <digest ref from step 3>, VCPU, MemoryMB, DiskSizeMB, StartCmd, ReadyCmd}`.
   Use a context timeout of 60 minutes.
7. **On success:**
   - `UpdateBuildState(ready, "", &BuildRow{KernelVersion, FirecrackerVersion, EnvdVersion, DiskMB: result.DiskSizeMB})`;
   - one `UpsertImage` that sets, together, `Digest` (from step 3), `VCPU`,
     `MemoryMB`, `DiskMB` (manifest), `StartCmd`, `ReadyCmd`,
     `CurrentBuildID = buildID` and `UpdatedAt = now`;
   - print `built <name> build=<id> digest=<digest>`.

   **On failure:** `UpdateBuildState(failed, <error>)`, leave the image row
   unchanged (`current_build_id` and `digest` keep describing the last good
   build), print the error with the last 50 log lines,
   and exit 1.
8. Old builds are **not** deleted here. That is U11's GC.

For `list`: print `name, current_build_id, digest (short), vcpu, memory_mb, disk_mb, updated_at`.

## Tests

- **Unit tests** (`cmd/spoond-images`), with a fake Substrate and a fake
  docker runner:
  - inject the command runner as package variables with exactly these
    signatures:
    `var runCmd = func(ctx context.Context, stdout io.Writer, name string, args ...string) error`
    (streams output; used for `docker build` and `docker push`) and
    `var runOut = func(ctx context.Context, name string, args ...string) (string, error)`
    (captures stdout; used for `docker inspect`);
  - a successful build updates the image and build rows;
  - a failed build keeps the old `current_build_id`;
  - the template id is reused across builds.
- **On vm2, against the staging DB `/var/lib/spoond/staging.db`:**
  - `cd /root/src/spoond && SPOOND_DB_PATH=/var/lib/spoond/staging.db /opt/spoond-staging/spoond images build --all --manifest images/manifest.yaml --context images`
    completes for all 7 images (the `E2B_*` variables use their `FromEnv`
    defaults).
  - `/opt/spoond-staging/spoond images list --db /var/lib/spoond/staging.db`
    shows 7 rows with build ids.
  - The substrate live check: create a sandbox from each image's
    `current_build_id` with the manifest `env`, and run
    `cat /etc/resolv.conf`. The first line is `nameserver 10.1.0.1`.
    Delete it.
  - Add this as a `-tags e2blive` test in `cmd/spoond-images`.

## Commits

1. `feat(images): Dockerfiles and guest init for E2B templates`
   (the Dockerfiles, guest files, manifest).
2. `feat(images): spoond images build — registry push and E2B template builds`
   (the store catalog, subcommand, tests).

## Done when

- All 7 images build on vm2 into the staging DB.
- Each image's live check passes.

## Do not

- Do not delete `deploy/bake-*`, `deploy/rootfs-init` or
  `deploy/rebuild-dev-base.sh` yet (that is U12).
- Do not build `rust-base`.
