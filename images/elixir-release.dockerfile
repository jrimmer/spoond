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

# Rust — pinned homes so cache mounts elsewhere can never mask the
# toolchain binaries (same reasoning as cytale's root Dockerfile).
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:$PATH
RUN curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal \
      --default-toolchain stable --no-modify-path

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
# LXC build containers (AF_UNIX denied at spawn_init, EACCES) but runs fine
# in the forkd microVM; CI jobs bootstrap hex/rebar themselves in ~5s.

# Guest agent (forkd-agent.py) reads /etc/environment for PATH — the
# go-base bake proved the default PATH misses /usr/local (issue #41).
RUN echo 'PATH=/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' >> /etc/environment
