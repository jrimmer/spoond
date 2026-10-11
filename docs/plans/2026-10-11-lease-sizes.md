# Per-lease sizes (#151) — 3.1 design

Status: accepted for 3.1 (orch-1, 2026-10-10 PT). The defaults below are
proposals the owner can change; each is one setting.

## What the spike found

- A Firecracker memory snapshot resumes at the vCPU count and memory size
  it was built with. The orchestrator sends no machine config after a
  snapshot load (e2b-runtime `fc/client.go:74-84`); only a cold boot sets
  it (`process.go:439`).
- A cold boot at any size is already reachable from Create: the
  orchestrator's `filesystem_boot` flag (`orchestrator.proto:155`) boots
  the template's root filesystem with the request's vCPU and memory.
  spoond's generated client has the field; spoond never sets it.
- A second build of the same image at another size reuses the cached
  base and step layers and reruns only finalize.
- Measured on sb (go-base-worker, 2026-10-10): a cold boot reaches
  guest-ready in about 3-4 s; an extra size's build costs about 5-10 s
  and about 90 MB of snapshot disk (274 MB apparent, ZFS-compressed).

## Decisions

1. **The lease owns its size.** A lease stores its vCPU, memory and disk
   at create. Every later path — pause, resume, clone, fork, restore,
   recovery — uses the lease's stored size, never the image row. A
   restart boots a fresh guest: a lease created at a chosen size
   (`leases.sized`, migration 27) keeps its size, and any other lease
   takes the image's current default, as before 3.1 (review: keeping a
   default size forever would cold-boot old leases on every restart). (Today clone, fork, restore, recovery and cold restart
   re-read the image row, so a rebuilt image changes what an old lease is
   charged while its VM keeps the old size.)
2. **Presets are global names for sizes**, from `SIZE_PRESETS`
   (default `small=1:2048,medium=2:4096,large=4:8192,xlarge=8:16384`,
   as `name=vcpu:memory_mb`). `GET /api/sizes` lists them.
3. **Warm sizes are per image.** An image's own `vcpu`/`memory_mb` is its
   default size and is always warm. A manifest's `warm_sizes: [medium,
   large]` builds one more snapshot per listed preset; builds record the
   preset in `builds.size` (`""` = the image default).
4. **Any other size cold-boots.** A create whose size has no warm build
   boots the image's current build with `filesystem_boot` at the requested
   size. The lease reports `start: "warm"` or `"cold"`.
5. **Requests.** Create takes `size` (a preset name) or `memory_mb` and
   `vcpu` (either may be given alone; the other comes from the image
   default). `memory_mib` stays as an alias of `memory_mb`; the old rule
   that it must equal the image's memory goes. Clone and fork keep the
   source's size. A named-snapshot start keeps the snapshot's size.
6. **Limits (400 `invalid_size`).** `vcpu` is 1 or even and at most
   `MAX_LEASE_VCPU` (default: the host's cores). `memory_mb` is even, at
   least the image's `min_memory_mb` (manifest field; default 512, or the
   image's own memory if smaller) and at most `MAX_LEASE_MEMORY_MB`
   (default 32768) and the box's hugepage total.
7. **Accounting.** Admission, take-back and fair shares charge the
   lease's memory. vCPU is passed to the VM and never accounted (#146).
8. **The warm pool** keeps serving image-default sizes only.

## Work

- Z1 lease owns its size: migration 25 (`leases.vcpu`, and existing
  stamps corrected from each VM's sandbox row or resume build), every
  path charges and stamps the size its VM runs, pause and checkpoint
  builds record the lease's size, GET lease shows `vcpu`, `memory_mb`,
  `disk_mb`. Until Z4, a cold restart (and a non-persistent warm
  restart) takes the image's current build and its size.
- Z2 substrate: `CreateRequest.FilesystemBoot`, the e2b client sets it
  and reads `filesystem_boot_applied`; the fake records it.
- Z3 presets: migration 26 (`image_sizes` warm builds per preset, GC
  roots; `images.min_memory_mb`), `SIZE_PRESETS`, `MAX_LEASE_*`,
  `GET /api/sizes`; `spoond images build` builds each `warm_sizes`
  preset.
- Z4 create API: `size`/`memory_mb`/`vcpu`, warm-or-cold resolution,
  validation, `start` on the lease, docs and status codes for Honey.
