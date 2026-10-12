# Free-page reporting (#136 stage 4) — next steps

Status: proposal by orch-1, 2026-10-12, for the owner's approval.

## What we know (window 2026-10-11 20:48–20:54 PT)

A 4 GiB lease touched 2 GiB and freed it. 45 s later the host still held:

| image | free-page reporting | host memory held |
|---|---|---|
| go-base-worker | off (production) | 2098 MiB |
| zz-fpr-probe (built with the flag on) | on | 78 MiB |

The guest kernel reports freed pages through the balloon device, and the host
returns them to the hugepage pool. The guest still sees its full size.
`memory_mb` becomes a ceiling, not a permanent charge on the host.

## What the test did not show

1. **Real workloads keep memory in the page cache.** A Go build leaves gigabytes
   of file cache, which the guest counts as "in use", so nothing is reported.
   The Python allocate-and-exit test was the easy case.
2. **Suspend/resume and snapshots** with the flag on are untested.
3. **Cost and stability:** CPU overhead and hugepage fragmentation over hours
   are unmeasured.

## Step 1 — measure a realistic workload (one window, two orchestrator restarts)

Run it the same way as the first window (flag on → probe image → measure →
flag off). Changes:

- **Probe image:** honey-go-worker plus the flag. That's a real Honey worker
  image with the Go module cache baked in.
- **Workload:** in a 4 GiB lease, `go build ./... && go test ./...` of Honey
  (offline). Record host-held memory at four points:
  - (a) right after the build;
  - (b) 60 s idle;
  - (c) after `sync; echo 1 > /proc/sys/vm/drop_caches` in the guest;
  - (d) 60 s after that.
- **Baseline:** the same on the production honey-go-worker.
- **Suspend/resume:** with the flag on, suspend and resume the probe lease,
  then check the exec still works and compare the snapshot's size with the
  baseline's.

**Decision rule:**
- **(b) shows most memory back:** reporting helps as it stands.
- **Only (d) shows it back:** the gain needs a guest-side cache drop. That's a
  Honey worker-image change, e.g. drop caches after each pass, or tune
  `vm.vfs_cache_pressure`. We'd agree it with Honey.

## Step 2 — turn it on (its own window)

Only if step 1 shows a real gain and suspend/resume is clean:

- Set `free-page-reporting` in `/etc/e2b/flags.json` and restart the
  orchestrator (drained). Rebuild every catalog image, because the flag only
  affects templates built while it is on.
- Leases created before the rebuild keep the old behaviour until recreated.
- Watch `memory_used_mb` (3.5) and host hugepages for a few days.

## Step 3 — a separate decision: admit by measured use

Steps 1–2 lower real host use but fit no extra leases. Admission and fair
shares still charge `memory_mb`. Fitting more leases means admitting against
measured use, which is overcommit:

- If many guests re-grow at once, the host can run out of hugepages. A
  Firecracker VM that can't fault in memory dies.
- The saturation gate (#136 stage 2) and take-back by suspend would have to
  act before the pool runs dry.

This changes how spoond fails, so it is the owner's call. It should rest on
some days of measured-use data from steps 1–2.

## What needs approval

- Step 1: one quiet window, timed with Honey, about 15 min. Two drained
  orchestrator restarts pause every guest for 1–3 min each. Standing #136 OK;
  confirm.
- Step 2: a second window, if step 1 passes.
- Step 3: an owner decision, later.
