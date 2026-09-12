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

**The destination model.** Each sandbox has writable storage of its own, and the
tag's own image is never written by a sandbox. Two mechanisms satisfy that model,
and the plan's first job is to establish which one we need:

- **P1 — per-child clone of the rootfs drive.** Each child gets its own reflink
  clone of the tag rootfs, and the drive is re-pointed at that clone before the
  guest resumes. Built and deployed; the base is byte-identical through boot.
  Correct for R1–R9, and its per-child cost is one rootfs clone.
- **P2 — read-only base plus a separate writable volume.** The tag's rootfs is
  baked read-only and shared; each child's writable state is a small volume, with
  the guest's root made writable by an overlay whose upper is that volume. Costs a
  volume clone per child instead of a rootfs clone.

**Why both exist, and what decides.** If block cloning works, P1's per-child cost
is near zero and P2 buys nothing we need. If it does not, P1 pays a full rootfs
copy per child, and P2's value is that the per-child cost scales with what the
guest actually writes rather than with the image size. So the deciding measurement
is U7's, and it must be taken before P2's machinery is built.

**A constraint that governs P2 only.** A drive must exist in the baked snapshot to
be usable per child: Firecracker cannot add a drive after boot without PCI
(developer preview, not default), and `PATCH /drives` cannot change
`is_read_only`. So P2's volume must be a **placeholder drive baked into the tag**,
and its read-only-ness fixed at bake. P1 needs none of that — and, importantly,
P1 must **not** bake its rootfs read-only: the child's writable root *is* the
re-pointed rootfs clone, and a read-only device stays read-only after its path is
swapped, so a read-only bake would leave every sandbox without a writable root and
fail R6. Under P1, the base is protected by KTD4's ordering, not by a flag.

**Authority.** The maintainer of `deeplethe/forkd` owns the upstream shape. This
plan owns our fork's shape, our deployment, and the decision of which track a
given change belongs to.

**Stop conditions.** Stop and re-decide if the writable layer cannot be provided
without a *job-visible* change (a read-only root, a path allowlist, or a write
budget the guest must respect). That is the failure mode that killed the
withdrawn option A. Stop and escalate if a step's verification cannot be run on
hardware — this work has repeatedly been wrong when reasoned instead of measured.
Re-scope rather than execute if U7 shows P2 is required: P2 is a larger change
than the units below carry.

**Revision 3 (2026-09-12).** A research pass and a four-lens review corrected the
plan in four places, all recorded below: the read-only bake is P2-only (revision 2
had it breaking P1's writable root); a clone's writable layer is settled as a
clone of the source's at branch time; the clone-cost measurement's provenance is
unverified, so U7 now re-measures before diagnosing; and P2's units are marked as
conditional rather than listed as ordinary work.

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
- **R9.** Suspending a sandbox preserves its writable state, and a controller
  restart does not destroy it. This is R2 applied to the persistent-dev-sandbox
  use case, which the first revision left uncovered.

### Out of scope

Job-side behaviour, runner concurrency limits, the tag re-bake schedule for
existing images, and quota enforcement. Those are recorded in spoond's own
tickets. Note the coupling: the re-bake schedule is out of scope here but is a
prerequisite for any tag re-baked read-only, so P2's rollout cannot start until
that ticket is scheduled.

## Planning Contract

### Key Technical Decisions

- **KTD1. Writable state is never shared; the tag's image is never written.**
  A sandbox's writable storage belongs to that sandbox alone, and no sandbox ever
  writes the tag's own image. This is the decision that makes R1–R4 expressible.
  Two mechanisms satisfy it and the plan deliberately carries both until U7
  chooses between them: a per-child clone of the rootfs drive (P1, built), or a
  read-only base plus a per-child writable volume (P2, larger). The earlier
  framing — "writable state must move off the rootfs drive" — described P2's
  mechanism rather than the requirement, and it made P1 read as a violation of a
  settled decision when P1 in fact satisfies every requirement.
  (session-settled: user-directed — chosen over the guest-RAM writable layer
  (option A), withdrawn because a job can outgrow a RAM write budget.)
- **KTD2. Read-only bakes belong to P2, never to P1.** A read-only base is what
  lets P2 share one tag image across children, and its flag must be chosen at bake
  because `PATCH /drives` accepts only `path_on_host` and Firecracker opens a
  non-read-only drive read-write at load. P1 must leave the flag writable: the
  guest's root *is* the re-pointed clone, and the read-only bit is frozen per
  device, so a read-only bake yields a read-only root and fails R6. Under P1, R4
  is protected by KTD4's ordering — the clone is bound before the guest ever runs.
- **KTD3. (P2 only) The guest's writable layer rides on the volume.** The base is
  immutable, so the guest's writes must land on the volume, provided as an overlay
  whose upper is the volume rather than a path allowlist (R6). **The guest cannot
  be told about a per-child path at fork time** — the kernel cmdline is frozen in
  the vmstate too — so the mount must be baked into the tag and must already exist
  in the snapshot's RAM image. U4 is a bake-time change, not a per-child one.
- **KTD4. Bind the child's storage at load, while paused.** Verified on v1.17.0:
  `PATCH /drives/{drive_id}` moves a restored VM's storage, works while the VM is
  paused, and leaves the shared base byte-identical through boot. The order is
  load `resume_vm: false` → PATCH → resume, and it is load-bearing: at load the
  child opens the *baked* path read-write, so the base is protected only by never
  letting the guest run in between. Fail closed: a child whose PATCH does not
  succeed is killed, never resumed.
- **KTD5. A clone's writable layer is a clone of its source's, taken at clone
  time.** A branch snapshots a *running* sandbox: its RAM image is the source's,
  with the source's filesystem mounted and its superblock and journal in memory.
  The clone's storage must therefore match that moment, and it must be
  materialized somewhere durable at branch time so it survives the source being
  reaped (R3). This settles the question the previous revision left open, and it
  is consistent with what a branch means to callers and with the job-facing
  contract that writes survive a branch. Isolation is then "writes made *after*
  branch time do not cross", not "the clone never sees the source's writes". The
  same rule covers KTD3's placeholder: whatever the RAM image was taken with is
  what the per-child storage must contain. It follows that branching costs a clone
  of the source's writable layer, and a chain of branches deepens that chain.
- **KTD6. Per-sandbox storage lives under the forkd data dir.** Durable,
  CoW-capable, and out of `/tmp` (R7). Work dirs stay scratch for sockets and
  consoles only.
- **KTD7. Two tracks.** Upstream takes the model change only if the maintainer
  wants it; the fork track carries the same commits with a rebase-friendly shape
  (small isolated commits, no deployment coupling).
- **KTD8. A per-child clone must share a dataset with its source, at matching
  recordsize.** ZFS block cloning silently falls back to a full read/write copy
  when source and destination differ in dataset or recordsize, when only part of a
  block would be cloned, when the data is not yet on disk, or when the module's
  bclone switch is off; most Linux kernels refuse cross-dataset clones outright,
  and a zvol cannot be block-cloned at all. A silent fallback is indistinguishable
  from success from the outside. This is a candidate cause of the measured cost,
  not a confirmed one — U7's ladder discriminates, and its first rung is the
  running build's provenance rather than placement.
- **KTD9. The clone-cost measurement chooses the mechanism; it does not define
  the destination.** Both P1 and P2 satisfy R1–R9. What U7 decides is whether
  P1's per-child cost is small enough to stop there, or whether P2's volume-shaped
  write layer is needed to keep per-child cost proportional to writes. Neither
  outcome leaves a requirement unmet, so this is a cost decision with a scope
  consequence, not a gate on correctness.

### Assumptions

- Two surfaces are shared and writable today, not one: the tag's rootfs, and any
  volume attached at bake, which every child re-attaches from the same host file.
  U2's per-child backing covers the rootfs; U3's drive rebind is what would cover
  a baked volume, and it is P2-gated, so today a volume-bearing tag still has
  concurrent children writing one filesystem. Any plan that claims isolation for
  volume-bearing tags must say which unit fixes that.
- `POOL_SIZE` and `RUNNER_MAX` are deployment policy, not part of this design.
- The host runs OpenZFS 2.4.1. Block cloning needs both the pool feature *and* the
  module's bclone switch; the switch has been off by default for stretches of the
  2.2–2.3 line, so a host can satisfy a version test and still copy. U7 checks the
  switch, not just the feature.
- Firecracker is 1.17.0; a future release that lands `drive_overrides` would
  collapse KTD4's three calls into one load-time override.

### High-Level Technical Design

The bind sequence, and why the paused window carries the weight:

```text
     tag: rootfs  [recordsize R, dataset D]   -- source only, never a live drive
                     |
   per child:  1. clone base -> child-<id>.<stem>     (KTD8: same dataset)
               2. PUT /snapshot/load {resume_vm: false}  <-- child opens the BAKED
                     |                                      path read-write here
               3. PATCH /drives/rootfs {path_on_host: clone}  <-- the only door
               4. PATCH /vm {"state": "Resumed"}          <-- guest's first write
                     v                                        lands in the clone (R4)
                guest runs with an ordinary writable root (R6)
```

Step 2 is the one to hold onto: there is no way to stop the child opening the
baked path, so the base is protected by never letting the guest run between steps
2 and 3 — not by a flag (KTD2). A child that fails step 3 is killed.

What each mechanism needs, and what only P2 needs:

```text
                        P1 (built)                  P2 (conditional)
  tag rootfs            writable flag, shared        baked read-only
  per-child storage     clone of the rootfs          clone of a small volume
  guest's root          the clone itself             overlay: base RO, upper=volume
  needs a baked drive   no                           yes -- the placeholder (KTD3)
  per-child cost        one rootfs clone             one volume clone
  fails when            clones are full copies       never on cost, but adds the
                                                     overlay + frozen-cmdline work
```

Lifecycle, with the two defects the research confirmed:

```text
  spawn ──► running ──► killed            (Drop reclaims the backing)
              │
              ├──► branch ─────────────► clone
              │      DEFECT: the branch's snapshot records rootfs: None, so the
              │      clone loads the vmstate-frozen path — the LIVE source's
              │      backing. Two VMs, one writable disk, while the source is
              │      still running. KTD5 says the fix materializes a durable
              │      clone of the source's storage at branch time.
              │
              └──► suspend ────────────► resume
                     DEFECT: suspend snapshots then drops the VM, and drop
                     unlinks the backing — so resume loads a deleted path, or
                     whatever later reused the name. R9 fails.
```

### Sequencing

1. U7's diagnostic first: it is cheap, and its number decides whether P2 is built
   at all.
2. U1 and U2 next — they are prerequisites for every gate. U1 is small under P1
   (record and refuse unknown provenance only).
3. U5 and U6 are independent of the P1/P2 choice and fix live defects that violate
   R2, R3 and R9 today; they can land in parallel with the diagnostic.
4. U3 and U4 are P2-gated: build them only if U7 shows a full copy per child.

### Open Questions

- Why E2B moved from guest-side overlayfs to a block-device COW design. The
  prior-art survey records the move but not the reason; if it was correctness
  rather than performance, KTD3's overlay inherits an unexamined risk, and U4's
  job-shaped gate would not catch it.
- Which commits form the isolation half upstream would take (R8), and whether U1's
  flag and P2's bake-time changes are separable from it in practice.
- Whether P2's per-child volume reclaim has an owner. U2's RAII-plus-sweep wording
  is written for P1's backing; if P2 lands, the volume needs the same treatment or
  R5 is unowned.
- If the isolated child is bound to the tag by a per-child mount namespace instead
  of a PATCH, the base is never opened read-write at all. The daemon already
  namespaces the network per child, so this is plausible; it is recorded as an
  alternative KTD4 was chosen over, not as a pending change.
- What a spawn from an existing tag does once U1 lands. Every tag deployed today
  was baked writable with no recorded flag; scoping the refusal to *unknown*
  provenance keeps them spawnable under P1.

## Implementation Units

- **U1. Record the base's provenance and refuse the unknown** (KTD2).
  `snapshot.json` carries the tag's read-only flag; the loader treats an absent
  flag as unknown and refuses, rather than assuming either way. A tag recorded
  **writable** is not refused under P1 — it is what P1 expects. The read-only bake
  itself is U4's prerequisite, not this unit's.
  *Verification:* the flag round-trips through `snapshot.json`; a tag with no
  recorded flag is refused at spawn with a clear error; an existing writable tag
  still spawns.
- **U2. Per-sandbox storage, correctly placed and owned** (KTD1, KTD6, KTD8).
  Create the backing in the same dataset as its source, at a path that is a pure
  function of a globally unique sandbox id — never of the tag or the batch index.
  Key the **work directory** the same way: two spawns of one tag can currently
  resolve to one directory, and the entering restore unlinks every non-directory
  entry in it, so a concurrent spawn can delete a running VM's socket. Reclaim
  with the sandbox (RAII on the child), plus a startup sweep over every work-dir
  shape the daemon uses, and attribution through a recorded path on the registry
  row — a live VM's drive path never appears in `/proc/<pid>/cmdline`, so process
  archaeology cannot attribute a backing.
  *Verification:* spawn, write in the guest, confirm writes land in the backing;
  kill, confirm it is gone; `SIGKILL` the controller, restart, confirm the backing
  count and bytes return to baseline; spawn two children of one tag via both tap
  modes and confirm neither unlinks the other's socket; unit-assert the path
  derivation.
- **U3. (P2-gated) Re-point a second drive at load** (KTD4). Extend the
  load-paused → PATCH → resume sequence to the writable volume's drive id; today
  only `rootfs` is rebound. Note the residual risk this unit exists to test: a
  PATCH that *succeeds* without moving storage is not covered by the fail-closed
  rule, and the guest would then run on the wrong disk.
  *Verification:* the marker test — a guest write must appear in the re-pointed
  volume; assert the device's advertised content changed, not merely that the call
  returned success.
- **U4. (P2-gated) Guest writable layer on the volume, baked** (KTD3). The overlay
  and its mount must exist in the snapshot's RAM image, because a restored VM
  never re-runs init and the cmdline is frozen. This unit carries two bake-path
  changes the current code cannot do: a controller bake that attaches a placeholder
  volume at all (the daemon bake path is volume-less today), and a read-only boot
  config that keeps `init=/forkd-init.sh` — the existing read-only boot route
  omits it, and it is what consumes the volume-mount hint.
  *Verification:* a job-shaped workload writes where it always has; the base stays
  byte-identical; the mount is present in a restore with no guest-side setup step;
  a baked volume is visible to every child and unwritten by any of them.
- **U5. Clone semantics** (KTD5, R2, R3). A branch materializes a durable clone of
  the source's writable layer at branch time and records it, so the clone does not
  depend on the source's lifetime and does not share the live source's disk.
  *Verification:* spawn the clone **while the source is alive**, assert both are
  isolated and that a write made *after* the branch does not appear in the other;
  then spawn → branch → kill source → spawn from the branch; and the clone's disk
  must be consistent with its restored RAM image (its filesystem checks clean, and
  files its memory says it wrote are present).
- **U6. Suspend/resume durability** (KTD5, R9). Suspension transfers the writable
  layer to the workspace row so it outlives the child's drop; resume re-attaches
  it. A resumed workspace needs the *written* layer, not a fresh clone — the two
  must not be confused, and the difference is exactly KTD5's rule.
  *Verification:* write in the guest → suspend → resume → the file is intact;
  suspend → `SIGKILL` the controller → the layer survives and a resume still
  yields the writes; assert the resumed disk is the one that was written, not a
  fresh clone.
- **U7. Clone-cost measurement** (KTD8, KTD9). Report a number for per-child
  growth on a build whose clone fix is verified *running*, then diagnose if it is
  still a full copy. The ladder is in the Verification Contract; its first rung is
  build provenance, because a deploy that does not restart the daemon keeps the
  old binary and a second measurement can repeat the first's result without
  testing anything new. The number decides P1 vs P2 in writing.
  *Verification:* the number, plus the storage-level evidence that produced it,
  plus an explicit statement of whether P2 is required.
- **U8. Track split** (KTD7). A short document naming which commits are the
  upstream proposal and which are ours.
- **U9. Documentation** (R6, R8). `install.md` (Firecracker floor, and the ZFS
  feature/switch floor if U7 shows it matters), `operations.md` (disk model,
  placement, and the forensics command, which currently points at `/tmp` only),
  `ci-jobs.md` (what a job may rely on — it must keep saying "an ordinary writable
  filesystem", and its branch claim must match U5's settled semantics), CHANGELOG,
  and `forkd/DESIGN.md` §4, which still describes an overlayfs-on-host design that
  was never shipped.
  *Verification:* each edited claim is checked against the gate that establishes
  it — the branch claim against U5's isolation gate, the floor against the
  diagnostic's storage checks — so no document asserts behaviour nothing measured.

## Verification Contract

Build gates for every unit touching Rust: `cargo fmt --all -- --check`,
`cargo clippy --all-targets --all-features -- -D warnings`, the crate test suites.

Behavioural gates, all of which have caught a real defect in this work already:

| Gate | What it must show |
|---|---|
| Two-child isolation | each child's marker in its own disk, the other's absent |
| Base integrity | the tag's rootfs sha256 identical before and after children write |
| Reclaim | disk count returns to baseline after kill, and after a controller `SIGKILL` — covering every work-dir shape the daemon uses, not just one |
| Clone durability | spawn → branch → kill source → spawn from the branch succeeds |
| Branch isolation | a clone spawned while its source is **alive** shares no disk with it, and writes made after branch time do not cross |
| Clone/RAM consistency | a branched clone's filesystem is consistent with its restored memory image |
| Suspend durability | write → suspend → resume → the write is intact, from the written layer and not a fresh clone; suspend → `SIGKILL` → still intact |
| Name uniqueness | two spawns of one tag, via both tap modes, share no work dir, socket or backing name |
| Placement (R7) | per-sandbox storage lives on the data-dir dataset, and no per-sandbox write lands in a work dir under `/tmp` |
| Separability (R8) | the isolation commits build and pass their gates with the model change reverted |
| No job-visible change | a real job (package install + build) runs unmodified, plus a job that uses mounts, `df`, hardlinks and cross-directory renames |
| Cost | per-child disk growth is measured, not assumed |

The cost ladder (U7), cheapest rung first — each rung can end the inquiry:

```text
 0. Provenance: is the running daemon the build that contains the clone fix?
      systemctl show / /proc/<pid>/exe, or restart and re-measure.
      A deploy without a restart keeps the old binary; a re-measurement can
      then reproduce the first number without testing anything new.
 1. Is the switch on, not just the feature?
      zpool get feature@block_cloning <pool>
      zfs get bclone_enabled <pool>          # module switch, off on parts of 2.2-2.3
      feature active + switch off -> a one-line runtime change, not placement.
 2. Are source and destination cloneable to each other?
      zfs list -o name,mountpoint,recordsize  <both paths>
      Different datasets or recordsizes -> that alone is the answer.
 3. Did a block clone actually happen?
      zpool get bcloneused,bclonesaved,bcloneratio <pool>   # before and after
      zdb -T <pool>
      bcloneratio == 1.0 -> every "clone" was a full copy.
 4. What did the file actually cost?
      du -sh <backing>  vs  du --apparent-size -sh <backing>
      parity == full image size -> the clone fell back.
      filefrag -e -k <backing>               # a `shared` flag means it landed
 5. Is it an accounting artifact instead?
      zfs list -o name,used,usedbydataset,referenced,written -r <pool>/<dataset>
      `used` is apparent per-dataset usage, so a real clone still inflates it —
      a large `used` alone proves nothing. `zfs destroy -nv` is the reclaim truth.
```

Rung 0 exists because the plan's own measured history is ambiguous: the 12291 MiB
figure was recorded against the wrong `FICLONE` constant, and a later run on a
redeployed build read 12303 MiB — but the running build was never verified, so
neither figure is attributable yet.

## Definition of Done

R1–R9 hold, each with a measured gate above, or the plan states for a given
requirement that its gate is deferred because P2 was not built. U5's sequence
passes, because that is the regression this plan exists to fix, and U6's does too,
because the persistent use case depends on it. U7 reports a number and the plan
states in writing whether P2 is needed. The upstream/fork split is written down,
so a maintainer's "no" changes deployment rather than the design.

## Sources & Research

External findings that shaped a decision, rather than a detached reading list:

- **Firecracker has no released load-time drive override.** `drive_overrides`
  exists only as an open PR, tracking a parked issue; `PATCH /drives/{id}` accepts
  only `path_on_host`, and cannot change `is_read_only`. This is what makes KTD2's
  bake-time choice permanent and P1's read-only bake impossible.
- **Firecracker cannot add a drive after boot without PCI** (developer preview,
  not default). This is why P2's volume must be a baked placeholder.
- **A forkd-based caller documents the same three-call bind** — bake a placeholder
  drive per volume, then rebind each drive with `PATCH` before the guest mounts it
  — convergent evidence that KTD4 is the intended reading of the API.
- **Firecracker's own docs state the supported `PATCH` case is an unmounted
  device** and warn a mounted one can fail silently. Our paused window avoids
  in-flight I/O, but the residual risk lands exactly on KTD3's overlay case, which
  is why U3's gate asserts content changed rather than a 2xx return.
- **Prior art splits two ways.** Share the base read-only and put writes elsewhere
  — E2B (read-only base + per-sandbox NBD copy-on-write block device, after
  abandoning guest-side overlayfs), Fly Machines (`dm-clone`), Kata (devmapper
  thin snapshots), Firecracker's own discussion (squashfs lower + writable upper).
  Or clone at the block layer, which is P1. Nobody adds a drive at restore,
  because Firecracker cannot.
- **ZFS block cloning silently falls back** on a dataset or recordsize mismatch,
  on partial blocks, when data is not yet on disk, or when the bclone switch is
  off — and cannot clone a zvol at all. This is KTD8 and the ladder's rungs 1–4.
- **The guest cannot discover a per-child mount path** (frozen cmdline), which is
  why KTD3's overlay is a bake-time artifact.

## Appendix

Findings this plan rests on, all measured on a KVM host at Firecracker v1.17.0:

- `drive_overrides` is absent from v1.16.2 and v1.17.0 — the load-time override is
  not available in a released build.
- `PATCH /drives/{drive_id}` moves a restored VM's storage, works while paused, and
  a guest write afterwards lands in the new file.
- Loading paused, re-pointing and resuming leaves the shared base byte-identical
  through boot.
- A branch records the drive path in effect at branch time, so a branch of a child
  whose disk is scratch names scratch — the current regression. Confirmed in code:
  all three branch paths write `rootfs: None`, and the restore pass treats that as
  "no backing, resume immediately", so the child reopens the frozen path. When the
  source was itself restored, that path is the *live source's* backing.
- Workspace suspend snapshots with `rootfs: None`, then drops the VM, and `Drop`
  unlinks the backing the snapshot names. Resume therefore loads a path that no
  longer exists, or one a later spawn has reused.
- `chain::reflink_copy` passed a wrong `FICLONE` number (`0x40209409` yields
  ENOTTY; `0x40049409` clones) and so had always streamed full copies, silently.
- One spawn of a 12 GiB rootfs grew the pool by 12291 MiB; a later run on a
  redeployed build read 12303 MiB. Neither figure is yet attributed — the running
  build was not verified, and a raw reflink ioctl on the same pair succeeds, so the
  clone fix alone does not explain the cost. U7's ladder starts here.
- Backings placed under the work dir inherit `/tmp`'s filesystem, which on a tmpfs
  `/tmp` would charge multi-GiB writes to RAM.
- The startup sweep visits one work-dir prefix only and matches one filename shape,
  so the daemon's other work-dir shapes are invisible to it, and a live VM's drive
  path never appears in `/proc/<pid>/cmdline`.
- The daemon's work dir is keyed by `(tag, netns_offset)`; a shared-tap spawn and a
  per-child spawn on an idle pool can both reserve offset 0, and the entering
  restore unlinks the peer's socket and backing of the same name.
- The daemon's bake path attaches no volumes at all, and the read-only boot route
  omits `init=/forkd-init.sh` — both are prerequisites U4 must build rather than
  assume (P2 only).
