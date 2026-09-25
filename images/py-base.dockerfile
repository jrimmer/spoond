# py-base — the runner's DEFAULT_IMAGE fallback (IMAGE_MAP: ubuntu-latest ->
# py-base; any label without an explicit mapping lands here). "Python 3.12
# slim base for Python build/test/CI jobs" per images/manifest.yaml; first
# baked 2026-08-07, re-created 2026-09-25 after the vm1 consolidation left
# only elixir-release registered (runner jobs on other labels 404'd —
# lacy-infra#26 decision b: bake the missing images).
#
# git is REQUIRED: the runner's built-in checkout execs `git clone` inside
# the sandbox. ca-certificates for HTTPS clones. bash ships with slim.
FROM python:3.12-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      git ca-certificates curl xz-utils file \
 && rm -rf /var/lib/apt/lists/*

# Guard: fail the BUILD loudly if anything the runner or guest agent needs
# is missing (the ext4 conversion can silently drop files when the host
# disk runs tight — same guard pattern as elixir-release).
RUN for b in git python3 pip3 curl; do \
      command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; \
    done \
 && git --version && python3 --version
