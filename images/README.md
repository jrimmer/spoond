# forkd image inquiry

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
3. **Don't version images** (`go-base:v1`). Re-bake in place when the
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
   bake a new one (named by capability) and update the manifest.

## The validation script

```bash
# local repo
python3 images/validate-image.py /path/to/repo

# remote repo (clones it)
python3 images/validate-image.py https://code.lacy.casa/org/repo.git
```

Output:
- **COVERED** (exit 0) — a baked image exists; use it
- **NEEDS** (exit 1) — no baked image; create one (name suggested)
- **UNKNOWN** (exit 2) — couldn't detect; needs human input

## The manifest

`images/manifest.yaml` is the source of truth for baked images. It lists
every tag, its capability, its `runs-on` labels, and whether it's baked.
**Keep it in sync with the forkd host** — when you bake a new image, mark
it `baked: true` here and add the tag to `KNOWN_IMAGES` on the backend
and `IMAGE_MAP` on the runner.

## Baking a new image

On the forkd host (vm2), from a Docker image:

```bash
forkd from-image <docker-image> --tag <name>
```

Then:
1. Add `<name>` to `KNOWN_IMAGES` in `/etc/forkd-backend.env`
2. Add `<label>=<name>` to `IMAGE_MAP` in `/etc/forkd-runner.env`
3. Mark `baked: true` in `images/manifest.yaml`

### Baking Rust images

Rust images need special attention due to the size of the toolchain and
build artifacts. See the `rust-base` entry in `manifest.yaml` for
detailed notes. Key requirements:

- **Rootfs**: 8+ GiB (`--size-mib 8192`). The Rust toolchain (~1.5 GiB)
  + cargo registry + build artifacts for `cargo test` exceed 4 GiB.
  See issue #38 for the sparse-rootfs proposal that would make this a
  non-issue.
- **Memory**: 4+ GiB (`--mem-size-mib 4096`). 512 MiB OOM-kills `cargo
  check` during tokio compilation. See issue #39 for `--mem-size-mib`
  support in `forkd from-image`.
- **python3**: Required for `forkd-agent.py` (PID 1 guest agent). Pass
  `--extra python3` to `forkd from-image`.
- **rustup**: Docker `rust:*` images ship rustup without a default
  toolchain. Run `rustup default stable` inside the sandbox **before
  snapshotting** so the toolchain is pre-installed and rustup doesn't
  try to download it at runtime. If the project has a
  `rust-toolchain.toml` with `channel = "stable"`, rustup will try to
  download the latest stable on first build — ensure it fits on the
  rootfs or pre-install it during bake.
- **PATH**: Ensure `/etc/environment` includes `/usr/local/cargo/bin`
  so `forkd-agent.py` (issue #41, PR #44) sets the correct container
  PATH for exec commands.

Recommended bake command (once #38/#39 are resolved):

```bash
forkd from-image rust:1.85-bookworm --tag rust-base \
  --extra python3 --size-mib 8192 --mem-size-mib 4096
```

After baking, run `rustup default stable` inside the sandbox before
snapshotting so the stable toolchain is pre-installed.
