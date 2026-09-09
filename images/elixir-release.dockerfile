# elixir-release — capability image for Phoenix/Elixir release builds that
# need the full NIF toolchain (per images/README.md doctrine: shared
# toolbox, name by capability, re-bake in place, never per-repo).
#
# Origin: the cytale deploy pipeline (2026-09-08) — its build job needs
# Elixir 1.18 + Rust (muninn/Tantivy rustler NIF) + Node 22/pnpm in ONE
# job, which no existing image covers (elixir-base is 1.17, no Rust/Node).
# Any Phoenix+NIF repo can reuse it; if a future job needs a different
# single toolchain, prefer a language base instead of growing this one.
#
# Bake (on the forkd host, see deploy/bake-elixir-release.sh):
#   docker build -f images/elixir-release.dockerfile -t elixir-release-tools:local .
#   forkd from-image elixir-release-tools:local --tag elixir-release \
#     --extra python3 --size-mib 12288 --mem-size-mib 4096
#
# Sizing follows the rust-base notes: Rust toolchain + cargo artifacts +
# hex deps + node_modules + _build peak well past 8 GiB; memory 4 GiB
# (512 MiB OOMs Rust compilation).
FROM elixir:1.18.4-otp-27

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      build-essential pkg-config libssl-dev libsrtp2-dev ca-certificates \
      curl git python3 jq xz-utils \
 && rm -rf /var/lib/apt/lists/*

# Rust — copied wholesale from the stock toolchain image instead of
# rustup-installed: curl/getaddrinfo is unreliable in this host's docker
# build containers (default seccomp denies pthread creation), and `curl | sh` once "passed"
# with no Rust installed when the download failed silently. Pinned homes
# so cache mounts elsewhere can never mask the toolchain binaries.
# The trailing rustc invocation fails the layer if the copy is broken.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:$PATH
COPY --from=rust:1-bookworm /usr/local/rustup /usr/local/rustup
COPY --from=rust:1-bookworm /usr/local/cargo /usr/local/cargo
RUN /usr/local/cargo/bin/rustc --version

# Node 22 + corepack pnpm for SPA builds. Selective copy — a wholesale
# `COPY --from=node /usr/local` would clobber erl/elixir, which live in
# /usr/local/bin on the elixir image.
COPY --from=node:22-bookworm-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22-bookworm-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -sf ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && ln -sf ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
 && corepack enable \
 && corepack prepare pnpm@11.24.0 --activate

# kaniko executor — assembles and pushes OCI images with no docker daemon
# (the sandbox has none; kaniko is pure userspace).
COPY --from=gcr.io/kaniko-project/executor:v1.23.2 /kaniko/executor /usr/local/bin/executor

# NOTE: no `mix local.hex` here — Erlang cannot boot inside this host's
# docker build containers (AF_UNIX denied at spawn_init, EACCES) but runs
# fine in the forkd microVM; CI jobs bootstrap hex/rebar themselves in ~5s.

# Guest agent (forkd-agent.py) reads /etc/environment for PATH — but only
# the FIRST PATH= line (appending a second one is silently ignored, which
# once cost us rustc). Rewrite /etc/environment wholesale: cargo first,
# one canonical PATH line; RUSTUP_HOME/CARGO_HOME for good measure.
RUN printf 'PATH=/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\nRUSTUP_HOME=/usr/local/rustup\nCARGO_HOME=/usr/local/cargo\n' > /etc/environment

# The rust:1 image's cargo/bin entries are symlinks to the `rustup` proxy,
# which needs RUSTUP_HOME resolved at runtime — the guest agent may not
# pass it through. Point rustc/cargo straight at the toolchain binary.
RUN ln -sf /usr/local/rustup/toolchains/*/bin/rustc /usr/local/cargo/bin/rustc \
 && ln -sf /usr/local/rustup/toolchains/*/bin/cargo /usr/local/cargo/bin/cargo

# The agent's exec PATH is a hardcoded default that does NOT include
# /usr/local/cargo/bin and (evidence of several failed bakes) does not
# reliably come from /etc/environment either — the go-base bake hit the
# same wall and solved it by symlinking into /usr/local/bin. Do that.
RUN ln -sf /usr/local/cargo/bin/rustc /usr/local/bin/rustc \
 && ln -sf /usr/local/cargo/bin/cargo /usr/local/bin/cargo

# Final guard: fail the BUILD (loudly, before the costly conversion) if
# any tool the CI pipeline needs is missing. The docker→ext4 conversion in
# build-rootfs.sh can silently drop files when the host disk runs tight.
RUN for b in pkg-config rustc cargo node corepack git python3 elixir mix executor curl; do \
      command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; \
    done && echo ALL_TOOLS_PRESENT

# NOTE: DNS/registry reachability is fixed at the INIT level, not here:
# forkd-init.sh (injected post-conversion, lives on the forkd host) now
# lists the LAN resolvers first, so code.lacy.casa resolves to the LAN
# edge whose /v2/ path is not SSO-gated. Image-level /etc/hosts pinning
# does not survive guest boot.
