---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
---

# Safe writable filesystems for forkd microVMs

## Goal Capsule

**Objective.** Make writable storage in forkd microVMs correct under concurrency,
and restore clone/branch semantics that the intermediate fix regressed — without
changing what a job sees inside the sandbox.

**Means.** Move the writable layer off the rootfs drive, where Firecracker freezes
the path and the read-only flag into the binary vmstate. The base image becomes
read-only and shared; each sandbox's writable state becomes its own volume, and
the volume is re-pointed per child at load time. Clone then means "shared
read-only base plus a fresh writable layer", which needs no branch-time
manipulation.

**Authority.** The maintainer of `deeplethe/forkd` owns the upstream shape. This
plan owns our fork's shape, our deployment, and the decision of which track a
given change belongs to.

**Stop conditions.** Stop and re-decide if the writable layer cannot be provided
without a *job-visible* change (a read-only root, a path allowlist, or a write
budget the guest must respect). That is the failure mode that killed the
withdrawn option A. Stop and escalate if a step's verification cannot be run on
hardware — this work has repeatedly been wrong when reasoned instead of measured.

## Product Contract

### Requirements

- **R1.** Concurrent sandboxes restored from one tag must not corrupt each other.
- **R2.** A sandbox's writable state is durable state belonging to that sandbox —
  not scratch, and not deleted out from under an artifact that references it.
- **R3.** Branching or cloning a *running* sandbox must produce a tag that
  survives its source being reaped. (Today it does not; see U5.)
- **R4.** The shared base image must never be written by a sandbox.
- **R5.** Per-sandbox disk cost must be bounded, accountable and reclaimable.
- **R6.** A job inside the sandbox sees an ordinary writable filesystem. No
  read-only root, no allowlist of writable paths, no RAM budget for writes.
- **R7.** The design must not depend on `/tmp` being a disk (tmpfs `/tmp` is a
  common configuration and would charge writable state to RAM).
- **R8.** The isolation half must be separable, so upstream can take it without
  also taking the model change.

### Out of scope

Job-side behaviour, runner concurrency limits, and the tag re-bake schedule for
existing images. Those are recorded in spoond's own tickets.

## Planning Contract

### KTDs

- **KTD1. Writable state moves off the rootfs drive.** The rootfs drive's path
  and `is_read_only` are serialized into the vmstate and cannot be overridden at
  load — released Firecracker has no `drive_overrides` (checked in the v1.16.2 and
  v1.17.0 binaries). Any scheme that keeps writable state on that drive has to
  work around a frozen path. Moving the writable layer to a separate device makes
  the *base* shareable and the *state* per-child, which is what makes R1, R2 and
  R3 expressible at all.
- **KTD2. The base is baked read-only.** A read-only base can be shared by any
  number of children forever. The flag is frozen at bake, so it must be chosen
  then and recorded in `snapshot.json` — this is where the metadata field
  contemplated earlier becomes load-bearing rather than diagnostic.
- **KTD3. The guest's writable layer rides on the volume.** The base is immutable,
  so the guest's writes must land on the volume. Provide it as an overlay whose
  upper is the volume, not as a path allowlist (R6). Where a volume is absent,
  fall back to scratch without pretending it is durable.
- **KTD4. Re-point the volume per child at load, paused.** Verified on v1.17.0:
  `PATCH /drives/{drive_id}` moves a restored VM's storage, works while the VM is
  paused, and leaves the shared base byte-identical through boot. This is the same
  mechanism already built for the intermediate fix; here it is applied to the
  writable drive rather than to the rootfs.
- **KTD5. Clone is defined, not engineered.** A clone gets the shared read-only
  base and a *fresh* writable volume, re-pointed at load by KTD4. So a branch
  records the durable base plus the source's volume path, and the clone's loader
  replaces the volume — no pause-time PATCH dance on the child, no refcounting,
  and no artifact referring to scratch.
- **KTD6. Volumes live under the forkd data dir.** Durable, CoW-capable, and out
  of `/tmp` (R7). Work dirs stay scratch for sockets and consoles only.
- **KTD7. Two tracks.** Upstream takes the model change only if the maintainer
  wants it; the fork track carries the same commits with a rebase-friendly shape
  (small isolated commits, no deployment coupling).

### Assumptions

- The tag's rootfs is the only shared writable surface today; volumes attached at
  bake are already shared the same way and are covered by the same fix.
- `POOL_SIZE` and `RUNNER_MAX` are deployment policy, not part of this design.
- ZFS on the host clones when asked; where it does not, per-sandbox copies are
  affordable at the pool sizes we run (measured headroom: 153 GB).

## Implementation Units

- **U1. Bake read-only bases and record the flag.** Forkd CLI and controller bake
  with `rootfs_read_only: true`; `snapshot.json` carries the flag; the loader
  treats "unknown" as unsafe rather than assuming either way.
  *Verification:* bake a tag, spawn two children, assert the base's sha256 is
  unchanged; assert the flag round-trips through `snapshot.json`.
- **U2. Per-sandbox writable volume.** Create a volume per sandbox under the data
  dir; attach it at bake; reclaim it with the sandbox (RAII on the child, plus a
  startup sweep for the crash case).
  *Verification:* spawn, write in the guest, confirm writes land in the volume
  file; kill, confirm the volume is gone; kill -9 the controller, confirm the
  sweep reclaims.
- **U3. Re-point the volume at load.** Extend the existing load-paused →
  PATCH → resume sequence to the writable drive; fail closed (a child that cannot
  be re-pointed is not resumed).
  *Verification:* the marker test — a guest write must appear in the re-pointed
  volume and not in the source.
- **U4. Guest writable layer on the volume.** Guest init sets up the overlay with
  the volume as its upper. No allowlist, no read-only root.
  *Verification:* a job-shaped workload (package install, build output) writes
  where it always has, and the base stays byte-identical.
- **U5. Clone semantics.** Branch records the base plus the source's volume path;
  the clone's load replaces the volume with a fresh one.
  *Verification:* the sequence that fails today — spawn, branch, kill the source,
  spawn from the branch — must succeed, and the clone must not see the source's
  writes.
- **U6. Track split.** A short document naming which commits are the upstream
  proposal and which are ours.
- **U7. Documentation.** `install.md` (Firecracker floor), `operations.md` (disk
  model and placement), `ci-jobs.md` (what a job may rely on — it must keep
  saying "an ordinary writable filesystem"), CHANGELOG.

## Verification Contract

Build gates for every unit touching Rust: `cargo fmt --all -- --check`,
`cargo clippy --all-targets --all-features -- -D warnings`, the crate test suites.

Behavioural gates, all of which have caught a real defect in this work already:

| Gate | What it must show |
|---|---|
| Two-child isolation | each child's marker in its own disk, the other's absent |
| Base integrity | the tag's rootfs sha256 identical before and after children write |
| Reclaim | disk count returns to baseline after kill, and after a controller `SIGKILL` |
| Clone durability | spawn → branch → kill source → spawn from the branch succeeds |
| No job-visible change | a real job (package install + build) runs unmodified |
| Cost | per-child disk growth is measured, not assumed |

## Definition of Done

R1–R8 hold, each with a measured gate above. U5's sequence passes, because that
is the regression this plan exists to fix. The upstream/fork split is written
down, so a maintainer's "no" changes deployment rather than the design.

## Appendix

Findings this plan rests on, all measured on a KVM host at Firecracker v1.17.0:

- `drive_overrides` is absent from v1.16.2 and v1.17.0 — the load-time override
  is not available in a released build.
- `PATCH /drives/{drive_id}` moves a restored VM's storage, works while paused,
  and a guest write afterwards lands in the new file.
- Loading paused, re-pointing and resuming leaves the shared base byte-identical
  through boot.
- A branch records the drive path in effect at branch time, so a branch of a child
  whose disk is scratch names scratch — the current regression.
- `chain::reflink_copy` passed a wrong `FICLONE` number (`0x40209409` yields
  ENOTTY; `0x40049409` clones) and so had always streamed full copies, silently.
- Backings placed under the work dir inherit `/tmp`'s filesystem, which on a
  tmpfs `/tmp` would charge multi-GiB writes to RAM.
