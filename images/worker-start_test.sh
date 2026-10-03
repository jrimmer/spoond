#!/usr/bin/env bash
# End-to-end tests for worker-start.sh's task exit paths. Whatever the exit
# (PASS, BLOCKED, cancelled), the worker must push its final HEAD — under
# --force-with-lease on the task branch, or to a "-wip-<sha>" branch when
# history was rewritten — and the report must name the pushed sha. These
# tests run the worker for real and assert on what landed in origin and in
# the mail. Scenarios: PASS (new branch), PASS after a fast-forward onto an
# existing branch, BLOCKED with a history rewrite, CANCELLED mid-task, and
# a retry of the same task that rewrites history again (the second -wip
# must land on a fresh name, not collide with the first).
#
# How it runs without touching the host: the worker is started inside a
# mount namespace where /work and /root are private tmpfs and
# /etc/resolv.conf is a scratch file, pi and amail are stubs on PATH, and
# origin is a local bare repo. Nothing leaves the machine.
set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
worker=$here/worker-start.sh
fails=0

command -v git >/dev/null || { echo "git is required"; exit 1; }
command -v unshare >/dev/null || { echo "unshare (with mount namespaces) is required"; exit 1; }

T=$(mktemp -d /tmp/worker-start-test.XXXXXX)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin"
: > "$T/resolv.conf"
cp "$worker" "$T/worker-start.sh"  # the real /work is shadowed inside the namespace

# --- stubs ------------------------------------------------------------------
# amail stub: a tiny file-backed mail queue under $TEST_MAIL. Sending appends
# to outbox.log (what the tests assert on); reading consumes the inbox.
cat > "$T/bin/amail" <<'STUB'
set -u
D=${TEST_MAIL:?}
cmd=${1:-}; [ $# -gt 0 ] && shift
mkdir -p "$D"
case $cmd in
  register|ack)
    exit 0 ;;
  send)
    subj=""
    while [ $# -gt 0 ]; do
      case $1 in
        --subject) subj=$2; shift 2 ;;
        --to|--cc|--thread) shift 2 ;;
        *) shift ;;
      esac
    done
    { printf '== subject: %s\n' "$subj"; cat; printf '\n'; } >> "$D/outbox.log"
    # test hook: with TEST_CANCEL=1, a [CANCEL] arrives once round 1's
    # verifying notice goes out, so the worker sees it at the next round.
    case $subj in
      *"round 1: verifying"*)
        if [ "${TEST_CANCEL:-0}" = "1" ]; then
          n=$(( $(cat "$D/seq" 2>/dev/null || echo 0) + 1 )); printf '%s\n' "$n" > "$D/seq"
          printf '#%s from orch-1 [CANCEL %s]\n' "$n" "$TEST_TASK_ID" >> "$D/inbox"
        fi ;;
    esac
    exit 0 ;;
  inbox)
    [ -f "$D/inbox" ] && cat "$D/inbox"
    exit 0 ;;
  read)
    id=${1:?}
    line=$(grep -m1 "^#$id " "$D/inbox" 2>/dev/null) || exit 0
    [ -n "$line" ] || exit 0
    # Consume the message even when it is the last line: grep -v finds
    # nothing then and exits 1, so an "&& mv" would leave the inbox — and
    # the worker — stuck on the same message forever.
    grep -v "^#$id " "$D/inbox" > "$D/inbox.next" || true
    mv "$D/inbox.next" "$D/inbox"
    case $line in
      *"[TASK $TEST_TASK_ID]"*)
        printf 'Subject: [TASK %s] do the thing\n\nRepo: %s\nBranch: %s\n' \
          "$TEST_TASK_ID" "$TEST_ORIGIN" "$TEST_BRANCH"
        [ -n "${TEST_BASE:-}" ] && printf 'Base: %s\n' "$TEST_BASE"
        printf 'Fix the thing.\n' ;;
      *)
        printf 'Subject: %s\n\nack\n' "${line#* }" ;;
    esac
    exit 0 ;;
  wait)
    exit 0 ;;
  *)
    echo "amail-stub: unknown command: $cmd" >&2
    exit 1 ;;
esac
STUB

# pi stub: an implement pass commits to the checked-out branch (recording
# each HEAD in $TEST_STATE/heads.log; with TEST_AMEND=1 a second round
# rewrites history onto origin/main, as a cleanup round would); a verify
# pass writes the verdict file the worker reads.
cat > "$T/bin/pi" <<'STUB'
set -u
key=""; prev=""
while [ $# -gt 0 ]; do
  case $prev in
    --model) key=$1 ;;
  esac
  prev=$1; shift
done
W=${TEST_WORK:-/work}
D=${TEST_STATE:?}
mkdir -p "$D"
wt=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
case $key in
  "$TEST_IMPL_MODEL")
    n=$(( $(cat "$D/impl-round" 2>/dev/null || echo 0) + 1 )); printf '%s\n' "$n" > "$D/impl-round"
    if [ "$n" -ge 2 ] && [ "${TEST_AMEND:-0}" = "1" ]; then
      git -C "$wt" reset -q --hard "$(git -C "$wt" merge-base HEAD "origin/${TEST_BASE:-main}")"
    fi
    printf 'change %s\n' "$n" > "$wt/change-$n.txt"
    git -C "$wt" add -A
    git -C "$wt" commit -q -m "work round $n"
    git -C "$wt" rev-parse HEAD >> "$D/heads.log"
    ;;
  "$TEST_VERIFY_MODEL")
    printf '%s\nfinding one\nfinding two\n' "${TEST_VERDICT:-FAIL}" > "$W/verdict.md"
    ;;
esac
printf '{}\n'
exit 0
STUB
chmod +x "$T/bin/amail" "$T/bin/pi"

# runner.sh: the worker's private world, then run it and keep its log.
cat > "$T/runner.sh" <<'STUB'
set -u
mount -t tmpfs none /work
mount -t tmpfs none /root
mount --bind "$TEST_TMP/resolv.conf" /etc/resolv.conf
bash "$TEST_WORKER"
rc=$?
cp -a /work/worker.log "$TEST_TMP/last-worker.log" 2>/dev/null || true
exit $rc
STUB

# make_origin DIR PREPUSH_BRANCH: a bare repo with one commit on main; with
# PREPUSH_BRANCH, that branch also exists on origin (an earlier attempt's
# pushed wip), and PREPUSH_SHA holds its tip.
make_origin() {
  local o=$1 pre=$2 s=$T/seed
  git init -q --bare "$o"
  rm -rf "$s"; git init -q -b main "$s"
  printf 'base\n' > "$s/README.md"
  git -C "$s" add README.md
  git -C "$s" commit -q -m base
  git -C "$s" push -q "$o" main
  PREPUSH_SHA=""
  if [ -n "$pre" ]; then
    git -C "$s" switch -q -c "$pre"
    printf 'earlier attempt\n' > "$s/wip.txt"
    git -C "$s" add wip.txt
    git -C "$s" commit -q -m "wip from an earlier attempt"
    git -C "$s" push -q "$o" "$pre"
    PREPUSH_SHA=$(git -C "$o" rev-parse "refs/heads/$pre")
  fi
}

# run_worker ORIGIN BRANCH TASKID MAXROUNDS VERDICT AMEND CANCEL
run_worker() {
  local origin=$1 branch=$2 tid=$3 rounds=$4 verdict=$5 amend=$6 cancel=$7
  rm -rf "$T/mail" "$T/state"
  mkdir -p "$T/mail" "$T/state"
  printf '#1 from orch-1 [TASK %s] do the thing\n' "$tid" > "$T/mail/inbox"
  TEST_MAIL=$T/mail TEST_STATE=$T/state TEST_ORIGIN=$origin TEST_BRANCH=$branch TEST_BASE=${TEST_BASE:-} \
    TEST_TASK_ID=$tid TEST_VERDICT=$verdict TEST_AMEND=$amend TEST_CANCEL=$cancel \
    TEST_IMPL_MODEL=test-impl TEST_VERIFY_MODEL=test-verif \
    TEST_WORK=/work TEST_TMP=$T TEST_WORKER=$T/worker-start.sh \
    SWARM_NAME=tester AMAIL_URL=test AMAIL_PROJECT=testproj \
    SWARM_IMPL_MODEL=test-impl SWARM_VERIFY_MODEL=test-verif \
    AMAIL_TOKEN=test-token SWARM_DEPLOY_KEY_B64=$(printf test-key | base64) \
    SWARM_MAX_ROUNDS=$rounds SWARM_PASS_TIMEOUT=60 SWARM_VERIFY_TIMEOUT=60 \
    SWARM_IDLE_TIMEOUT=2 SWARM_ORCH=orch-1 \
    PATH=$T/bin:$PATH \
    unshare -m bash "$T/runner.sh"
}

expect_eq() { [ "$1" = "$2" ]; }

check() {  # check DESC CMD ARGS...
  local desc=$1; shift
  if "$@"; then echo "  ok: $desc"; else echo "  FAIL: $desc"; fails=$((fails + 1)); fi
}

# scenario NAME ORIGIN BRANCH TASKID: shared exit-code and report-shape checks
scenario_prologue() {
  echo "scenario: $1"
  run_worker "$2" "$3" "$4" "$5" "$6" "$7" "$8"
  check "worker exits cleanly" expect_eq "$?" "0"
}

short() { git -C "$1" rev-parse --short=7 "$2"; }

# --- PASS: one round, verifier passes, branch is new on origin ---------------
o=$T/o-pass.git
make_origin "$o" ""
scenario_prologue PASS "$o" swarm/tp1 tp1 1 PASS 0 0
head=$(tail -1 "$T/state/heads.log")
tip=$(git -C "$o" rev-parse refs/heads/swarm/tp1)
check "pushed sha equals the worker's final HEAD" expect_eq "$tip" "$head"
check "report names the pushed sha and branch" \
  grep -q "^pushed: $(short "$o" refs/heads/swarm/tp1) -> swarm/tp1 (succeeded" "$T/mail/outbox.log"
check "report is [DONE" grep -q "\[DONE tp1\]" "$T/mail/outbox.log"
check "no -wip branch was needed" \
  expect_eq "$(git -C "$o" for-each-ref --format='%(refname)' 'refs/heads/*wip*' | wc -l)" "0"

# --- PASS after a rewrite-free push onto an existing branch (explicit lease)
o=$T/o-pass-ff.git
make_origin "$o" swarm/tp2
scenario_prologue PASS-FF "$o" swarm/tp2 tp2 1 PASS 0 0
head=$(tail -1 "$T/state/heads.log")
tip=$(git -C "$o" rev-parse refs/heads/swarm/tp2)
check "pushed sha equals the worker's final HEAD" expect_eq "$tip" "$head"
check "push was a fast-forward of the old tip" expect_eq "$(git -C "$o" rev-parse refs/heads/swarm/tp2^)" "$PREPUSH_SHA"
check "report names the pushed sha and branch" \
  grep -q "^pushed: $(short "$o" refs/heads/swarm/tp2) -> swarm/tp2 (succeeded" "$T/mail/outbox.log"

# --- BLOCKED: verifier never passes, second round rewrites history -----------
o=$T/o-blocked.git
make_origin "$o" swarm/tb3
scenario_prologue BLOCKED "$o" swarm/tb3 tb3 2 FAIL 1 0
head=$(tail -1 "$T/state/heads.log")
wipname="swarm/tb3-wip-${head:0:7}"   # worker names -wip branches with the short sha
wip=$(git -C "$o" rev-parse "refs/heads/$wipname")
check "rewritten history went to a -wip branch" expect_eq "$wip" "$head"
check "old task-branch tip was not overwritten" expect_eq "$(git -C "$o" rev-parse refs/heads/swarm/tb3)" "$PREPUSH_SHA"
check "report names the pushed sha and the -wip branch" \
  grep -q "^pushed: $(short "$o" "refs/heads/$wipname") -> $wipname (succeeded" "$T/mail/outbox.log"
check "report is [BLOCKED" grep -q "\[BLOCKED tb3\]" "$T/mail/outbox.log"

# --- RETRY-REWRITE: the same task again rewrites history once more; the
# second -wip must get a fresh name (the first is create-only, so a fixed
# -wip name would be refused and the retry's last round would survive only
# in the lease) ----------------------------------------------------------------------------
o=$T/o-retry.git
make_origin "$o" swarm/tr5
run_worker "$o" swarm/tr5 tr5 2 FAIL 1 0   # first attempt: rewrite -> swarm/tr5-wip-<sha>
head1=$(tail -1 "$T/state/heads.log"); w1="swarm/tr5-wip-${head1:0:7}"
check "first attempt pushed its rewrite" \
  expect_eq "$(git -C "$o" rev-parse "refs/heads/$w1")" "$head1"
run_worker "$o" swarm/tr5 tr5 2 FAIL 1 0   # retry: another rewrite, must not be refused
head2=$(tail -1 "$T/state/heads.log"); w2="swarm/tr5-wip-${head2:0:7}"
check "worker exits cleanly" expect_eq "$?" "0"
check "retry's rewrite landed on a fresh -wip name" \
  expect_eq "$(git -C "$o" rev-parse "refs/heads/$w2")" "$head2"
check "the first attempt's -wip still holds its commits" \
  expect_eq "$(git -C "$o" rev-parse "refs/heads/$w1")" "$head1"
check "task branch still at its old tip through both attempts" \
  expect_eq "$(git -C "$o" rev-parse refs/heads/swarm/tr5)" "$PREPUSH_SHA"
check "retry's report names the pushed sha and branch" \
  grep -q "^pushed: $(short "$o" "refs/heads/$w2") -> $w2 (succeeded" "$T/mail/outbox.log"

# --- CANCELLED: a [CANCEL] lands mid-task ------------------------------------
o=$T/o-cancel.git
make_origin "$o" ""
scenario_prologue CANCELLED "$o" swarm/tc4 tc4 3 FAIL 0 1
head=$(tail -1 "$T/state/heads.log")
tip=$(git -C "$o" rev-parse refs/heads/swarm/tc4)
check "pushed sha equals the worker's final HEAD" expect_eq "$tip" "$head"
check "report names the pushed sha and branch" \
  grep -q "^pushed: $(short "$o" refs/heads/swarm/tc4) -> swarm/tc4 (succeeded" "$T/mail/outbox.log"
check "report is [CANCELLED" grep -q "\[CANCELLED tc4\]" "$T/mail/outbox.log"

echo
# --- BASE: the task names Base: v3; the branch starts from origin/v3, and
# the report counts only the worker's own commits, not v3's -------------------
o=$T/origin-base
make_origin "$o" ""
s2=$T/seed-v3; rm -rf "$s2"; git clone -q "$o" "$s2"
git -C "$s2" switch -q -c v3
printf 'v3 only\n' > "$s2/v3.txt"; git -C "$s2" add v3.txt; git -C "$s2" commit -q -m "v3 work"
git -C "$s2" push -q origin v3
V3_SHA=$(git -C "$o" rev-parse refs/heads/v3)
TEST_BASE=v3
scenario_prologue BASE "$o" swarm/tv6 tv6 1 PASS 0 0
TEST_BASE=
tip=$(git -C "$o" rev-parse refs/heads/swarm/tv6 2>/dev/null || true)
check "task branch was pushed" test -n "$tip"
check "task branch builds on origin/v3" git -C "$o" merge-base --is-ancestor "$V3_SHA" "$tip"
check "report counts one commit (not v3's)" expect_eq "$(grep -c '^  [0-9a-f]\{7\} ' "$T/mail/outbox.log")" "1"
check "start report names origin/v3" grep -q 'swarm/tv6 from origin/v3' "$T/mail/outbox.log"

# --- BASE-MISSING: Base: names a branch origin does not have -------------------
o=$T/origin-nobase
make_origin "$o" ""
TEST_BASE=no-such-base
run_worker "$o" swarm/tn7 tn7 1 PASS 0 0
TEST_BASE=
check "worker exits cleanly" expect_eq "$?" "0"
check "reported blocked: no base branch" grep -q 'BLOCKED tn7\] permanent: no base branch no-such-base' "$T/mail/outbox.log"
check "nothing was pushed for the task" expect_eq "$(git -C "$o" for-each-ref 'refs/heads/swarm/*' | wc -l)" "0"

if [ "$fails" -eq 0 ]; then
  echo "all scenarios passed"
  exit 0
fi
echo "$fails check(s) failed; last worker log follows:" >&2
cat "$T/last-worker.log" >&2 || true
exit 1
