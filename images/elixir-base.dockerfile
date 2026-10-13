# elixir-base — Elixir 1.19.6 / Erlang OTP 28.5.0.7 toolchain (capability: elixir),
# with a PostgreSQL 17 server running in the guest for Ecto test suites and
# Python 3 (stdlib) for build-time generators. Exact versions come from the
# hexpm tag, so a repo pinning the same versions with mise/asdf matches.
# Versions are checked from files: the BEAM does not start inside the image
# build's container (spawn_init EACCES on sb); it is exercised in a lease.
FROM hexpm/elixir:1.19.6-erlang-28.5.0.7-debian-trixie-20261005
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends git ca-certificates curl build-essential \
      python3 postgresql-17 postgresql-client-17 locales \
 && rm -rf /var/lib/apt/lists/*
RUN for b in elixir mix erl git python3 psql; do command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; done \
 && grep -q 'vsn,"1\.19\.6"' /usr/local/lib/elixir/lib/elixir/ebin/elixir.app \
 && test "$(cat /usr/local/lib/erlang/releases/28/OTP_VERSION)" = 28.5.0.7 \
 && test -x /usr/lib/postgresql/17/bin/postgres
# A fresh cluster (main, port 5432) whose postgres superuser has a known
# password, reachable on loopback only. Built with initdb's --pwfile, since
# no server can start inside the image build (unix sockets are refused there).
# Tests create and migrate their own databases; a repo that wants its own
# role creates it as postgres.
RUN pg_dropcluster 17 main \
 && printf 'postgres\n' > /tmp/pgpw && chown postgres /tmp/pgpw \
 && pg_createcluster 17 main -- --auth-local=peer --auth-host=scram-sha-256 --pwfile=/tmp/pgpw \
 && rm -f /tmp/pgpw \
 && sed -i "s/^#\?listen_addresses.*/listen_addresses = '127.0.0.1'/" /etc/postgresql/17/main/postgresql.conf \
 && grep -q "^listen_addresses = '127.0.0.1'" /etc/postgresql/17/main/postgresql.conf
COPY --chmod=755 elixir-base-postgres-hook.sh /etc/spoond/init.d/50-postgres
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
ARG SPOOND_GUEST_DNS_ADDR=
RUN mkdir -p /etc/spoond/init.d \
 && printf '%s\n' "$SPOOND_GUEST_DNS_ADDR" > /etc/spoond/guest-dns
