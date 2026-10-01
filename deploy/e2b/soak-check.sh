#!/usr/bin/env bash
# soak-check.sh — daily E2B cutover soak check (U12 step 16, automated).
#
# Appends ONE JSON line to /var/lib/spoond/soak.log with:
#   date                  UTC RFC 3339
#   doctor                PASS/WARN/FAIL counts and every non-PASS line
#   leases                lease counts by state (read-only SQLite)
#   ci_24h                runner jobs from the last 24h (ok/failed/failures)
#   ci_24h_excl_renovate  same counts, jobs whose failed_step contains
#                         "Renovate" excluded (nightly exit 127, pre-cutover)
#   orchestrator_restarts systemctl NRestarts of e2b-orchestrator
#   hugepages             HugePages_Total/HugePages_Free from /proc/meminfo
#
# Exits non-zero when doctor reports a FAIL or any lease is in state "lost",
# so failures surface in `systemctl --failed`. Writes nothing outside the log.

set -euo pipefail

# Doctor: source the deployed backend env so the checks see the real config.
# A doctor crash must not abort the run — the counts below carry the signal.
doctor_output=$(set -a; . /etc/spoond/backend.env; set +a; /opt/spoond/spoond doctor 2>&1) || true

SOAK_DOCTOR_OUTPUT="$doctor_output" python3 - <<'PY'
import json
import os
import re
import sqlite3
import subprocess
import sys
from datetime import datetime, timezone

DB_URI = "file:/var/lib/spoond/spoond.db?mode=ro"
LOG_PATH = "/var/lib/spoond/soak.log"


def run(cmd):
    """Run cmd, return stdout; degrade to empty output on any failure."""
    try:
        r = subprocess.run(cmd, capture_output=True, text=True)
    except OSError:
        return ""
    return r.stdout or ""


def to_int(s):
    try:
        return int(s)
    except ValueError:
        return 0


# CI jobs from the runner journal: `job <id> final result=<n>`, failures carry
# `failed_step=<...>` on the final-result line.
ok = failed = ok_ren = failed_ren = 0
failures = []
for line in run(["journalctl", "-u", "spoond-runner", "--since", "-24h"]).splitlines():
    m = re.search(r"final result=(\S+)", line)
    if not m:
        continue
    jm = re.search(r"job (\d+) final result=", line)
    if m.group(1) == "0":
        ok += 1
        ok_ren += 1
        continue
    sm = re.search(r"failed_step=(.*)", line)
    step = sm.group(1).strip() if sm else ""
    failed += 1
    failures.append({"id": int(jm.group(1)) if jm else None, "failed_step": step})
    if "Renovate" not in step:
        failed_ren += 1

# Lease counts by state, straight from the store, read-only.
con = sqlite3.connect(DB_URI, uri=True)
try:
    states = dict(con.execute("SELECT state, COUNT(*) FROM leases GROUP BY state").fetchall())
finally:
    con.close()

hugepages = {}
with open("/proc/meminfo") as meminfo:
    for line in meminfo:
        key, _, value = line.partition(":")
        if key in ("HugePages_Total", "HugePages_Free"):
            hugepages[key] = to_int(value.split()[0])

p = w = f = 0
non_pass = []
for line in os.environ.get("SOAK_DOCTOR_OUTPUT", "").splitlines():
    if line.startswith("PASS"):
        p += 1
    elif line.startswith("WARN"):
        w += 1
    elif line.startswith("FAIL"):
        f += 1
    if not line.startswith("PASS") and line.strip():
        non_pass.append(line)

report = {
    "date": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "doctor": {"pass": p, "warn": w, "fail": f, "non_pass_lines": non_pass},
    "leases": states,
    "ci_24h": {"ok": ok, "failed": failed, "failures": failures},
    "ci_24h_excl_renovate": {"ok": ok_ren, "failed": failed_ren},
    "orchestrator_restarts": to_int(
        run(["systemctl", "show", "e2b-orchestrator", "-p", "NRestarts", "--value"]).strip()
    ),
    "hugepages": {
        "total": hugepages.get("HugePages_Total", 0),
        "free": hugepages.get("HugePages_Free", 0),
    },
}

line = json.dumps(report)
with open(LOG_PATH, "a") as log:
    log.write(line + "\n")
print(line)

# Non-zero so failures surface in `systemctl --failed`.
sys.exit(1 if f > 0 or states.get("lost", 0) > 0 else 0)
PY
