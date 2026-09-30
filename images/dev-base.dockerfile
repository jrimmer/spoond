# dev-base — interactive development sandbox (capability: interactive-dev).
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      tmux openssh-server git python3 build-essential ca-certificates curl locales \
 && mkdir -p /run/sshd \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 guest/spoond-tmux.sh /etc/profile.d/spoond-tmux.sh
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
RUN mkdir -p /etc/spoond/init.d
