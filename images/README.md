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
python3 images/validate-image.py https://code.lacy.casa/org/repo.git
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
