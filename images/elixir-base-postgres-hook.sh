#!/bin/bash
# /etc/spoond/init.d/50-postgres — start PostgreSQL 17 at guest boot (elixir-base).
#
# Runs as a spoond-guest-init hook, before the agent: starts the cluster and
# returns once it accepts connections, so a bake snapshots it serving and a
# cold boot (#151 sizes) starts it the same way. A failed hook stops the
# guest from becoming ready, so the wait is bounded and the log is shown.
set -u
# /run is fresh at boot and the guest has no systemd-tmpfiles to make the
# socket directory the Debian package expects.
install -d -o postgres -g postgres -m 2775 /var/run/postgresql
pg_ctlcluster 17 main start || { echo "postgres: start failed"; tail -20 /var/log/postgresql/postgresql-17-main.log; exit 1; }
for _ in $(seq 1 60); do
  if pg_isready -q -h 127.0.0.1 -p 5432; then
    echo "postgres: ready on 127.0.0.1:5432"
    exit 0
  fi
  sleep 1
done
echo "postgres: not ready after 60 s"
tail -20 /var/log/postgresql/postgresql-17-main.log
exit 1
