# elixir-base — Elixir 1.17 / OTP 27 toolchain (capability: elixir).
FROM elixir:1.17.3-otp-27
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates curl build-essential \
 && rm -rf /var/lib/apt/lists/*
RUN for b in elixir mix erl git; do command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; done
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
RUN mkdir -p /etc/spoond/init.d
