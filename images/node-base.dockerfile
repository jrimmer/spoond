# node-base — Node.js 24 LTS on the dev-base environment (capability: nodejs).
# Node, npm, npx and corepack come from the official node image; the rest
# matches dev-base (tmux, sshd, git, python3, build tools for native
# modules). Rebuild to pick up a new Node 24 release.
FROM node:24-bookworm-slim AS node

FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      tmux openssh-server git python3 build-essential ca-certificates curl locales \
 && mkdir -p /run/sshd \
 && rm -rf /var/lib/apt/lists/*
# Copy only node and its bundled modules: `COPY --from=node /usr/local`
# would clobber anything else under /usr/local.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -sf ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && ln -sf ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
 && node --version | grep -q '^v24\.' \
 && npm --version >/dev/null \
 && corepack --version >/dev/null
COPY --chmod=755 guest/spoond-tmux.sh /etc/profile.d/spoond-tmux.sh
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
ARG SPOOND_GUEST_DNS_ADDR=
RUN mkdir -p /etc/spoond/init.d \
 && printf '%s\n' "$SPOOND_GUEST_DNS_ADDR" > /etc/spoond/guest-dns
