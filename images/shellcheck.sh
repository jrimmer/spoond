#!/usr/bin/env bash
# shellcheck.sh — the worker scripts' lint gate.
#
# The worker loop is shellcheck-clean and must stay that way. This runs
# shellcheck when it is available and fails loudly when the scripts drift;
# when the tool is absent (a dev box with no egress, such as the sandbox
# that wrote the change) it prints SKIP and exits 0 rather than blocking,
# and the arm that lacks a binary is covered by the worker image itself:
# images/worker.dockerfile installs shellcheck and its build fails if the
# binary is missing.
#
#   bash images/shellcheck.sh
set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
scripts=(worker-start.sh worker-git.sh worker-git_test.sh worker-start_test.sh)

if ! command -v shellcheck >/dev/null; then
  echo "shellcheck not installed; SKIP (the worker image installs it)"
  exit 0
fi

rc=0
for s in "${scripts[@]}"; do
  if shellcheck -x "$here/$s"; then
    echo "  ok: $s"
  else
    rc=1
  fi
done
if [ "$rc" -eq 0 ]; then
  echo "shellcheck clean"
else
  echo "shellcheck findings above" >&2
fi
exit "$rc"
