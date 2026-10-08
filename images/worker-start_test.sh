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
helper=$here/worker-git.sh
fails=0

command -v git >/dev/null || { echo "git is required"; exit 1; }
command -v unshare >/dev/null || { echo "unshare (with mount namespaces) is required"; exit 1; }

T=$(mktemp -d /tmp/worker-start-test.XXXXXX)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin"
: > "$T/resolv.conf"
cp "$worker" "$T/worker-start.sh"  # the real /work is shadowed inside the namespace
cp "$helper" "$T/worker-git.sh"   # sourced by worker-start.sh from its own directory

# --- stubs ------------------------------------------------------------------
# amail stub: a tiny file-backed mail queue under $TEST_MAIL. Sending appends
# to outbox.log (what the tests assert on); reading consumes the inbox.
cat > "$T/bin/amail" <<'STUB'
#!/bin/bash
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
        [ -n "${TEST_VERIFY_TIMEOUT_LINE:-}" ] && printf 'Verify-Timeout: %s\n' "$TEST_VERIFY_TIMEOUT_LINE"
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
#!/bin/bash
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
    # A rebase the harness left in progress is resolved here, as an
    # implement round would: write the resolved file and continue.
    if [ -d "$wt/.git/rebase-merge" ] || [ -d "$wt/.git/rebase-apply" ]; then
      [ -n "${TEST_CONFLICT_FILE:-}" ] && printf 'resolved\n' > "$wt/conflict.txt"
      git -C "$wt" add -A >/dev/null 2>&1 || true
      GIT_EDITOR=true git -C "$wt" rebase --continue >/dev/null 2>&1 || true
      git -C "$wt" rev-parse HEAD >> "$D/heads.log"
      printf '{}\n'; exit 0
    fi
    n=$(( $(cat "$D/impl-round" 2>/dev/null || echo 0) + 1 )); printf '%s\n' "$n" > "$D/impl-round"
    if [ "$n" -ge 2 ] && [ "${TEST_AMEND:-0}" = "1" ]; then
      git -C "$wt" reset -q --hard "$(git -C "$wt" merge-base HEAD "origin/${TEST_BASE:-main}")"
    fi
    lines=${TEST_LINES:-300}
    : > "$wt/change-$n.txt"
    i=1; while [ "$i" -le "$lines" ]; do printf 'change %s line %s\n' "$n" "$i" >> "$wt/change-$n.txt"; i=$((i + 1)); done
    [ -n "${TEST_CONFLICT_FILE:-}" ] && printf 'branch side\n' > "$wt/conflict.txt"
    # TEST_MIGRATION=dup|below plants a bad migration for the guard.
    # TEST_MIGRATION_ONCE=1 plants it only in round 1 and removes it in
    # later rounds, so a retry can fix the guard and reach a verify.
    case ${TEST_MIGRATION:-} in
      dup)   mkdir -p "$wt/store/migrations"; printf 'dup\n' > "$wt/store/migrations/0002_dup.sql" ;;
      below)
        if [ "${TEST_MIGRATION_ONCE:-0}" = "1" ] && [ "$n" -ge 2 ]; then
          rm -f "$wt/store/migrations/0001_below.sql"
        else
          mkdir -p "$wt/store/migrations"; printf 'below\n' > "$wt/store/migrations/0001_below.sql"
        fi ;;
    esac
    git -C "$wt" add -A
    git -C "$wt" commit -q -m "work round $n"
    git -C "$wt" rev-parse HEAD >> "$D/heads.log"
    # TEST_MOVE_BASE=N: after the Nth implement round, advance
    # origin's base branch so the base moves before the rebase/push.
    if [ -n "${TEST_MOVE_BASE:-}" ] && [ "$n" = "$TEST_MOVE_BASE" ]; then
      mb=$D/movebase; rm -rf "$mb"; git clone -q -b "${TEST_BASE:-main}" "$TEST_ORIGIN" "$mb"
      git -C "$mb" config user.name test; git -C "$mb" config user.email test@example.com
      printf 'main side\n' >> "$mb/${TEST_MOVE_FILE:-README.md}"
      git -C "$mb" add -A
      git -C "$mb" commit -qm "main moved"
      git -C "$mb" push -q origin "${TEST_BASE:-main}"
    fi
    ;;
  "$TEST_VERIFY_MODEL")
    vn=$(( $(cat "$D/verify-round" 2>/dev/null || echo 0) + 1 )); printf '%s\n' "$vn" > "$D/verify-round"
    verdict=${TEST_VERDICT:-FAIL}
    # TEST_VERDICT_SEQ is a comma list chosen by verify call number, so a
    # verifier can fail then pass (or time out) across rounds.
    if [ -n "${TEST_VERDICT_SEQ:-}" ]; then
      verdict=$(cut -d, -f"$vn" <<<"$TEST_VERDICT_SEQ")
    fi
    case $verdict in
      TIMEOUT) sleep "${TEST_VERIFY_SLEEP:-2}" ;;  # let the pass timeout
      "") : > "$W/verdict.md" ;;                    # no verdict at all
      *) printf '%s\nfinding one\nfinding two\n' "$verdict" > "$W/verdict.md" ;;
    esac
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
  git -C "$o" symbolic-ref HEAD refs/heads/main
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

# make_origin_migrations DIR: a bare repo whose main has two migrations,
# for exercising the migration guard through the worker.
make_origin_migrations() {
  local o=$1 s=$T/seed-mig
  git init -q --bare "$o"
  git -C "$o" symbolic-ref HEAD refs/heads/main
  rm -rf "$s"; git init -q -b main "$s"
  mkdir -p "$s/store/migrations"
  printf 'a\n' > "$s/store/migrations/0001_init.sql"
  printf 'b\n' > "$s/store/migrations/0002_x.sql"
  printf 'base\n' > "$s/README.md"
  git -C "$s" add -A
  git -C "$s" commit -q -m base
  git -C "$s" push -q "$o" main
}

# run_worker ORIGIN BRANCH TASKID MAXROUNDS VERDICT AMEND CANCEL
run_worker() {
  local origin=$1 branch=$2 tid=$3 rounds=$4 verdict=$5 amend=$6 cancel=$7
  rm -rf "$T/mail" "$T/state"
  mkdir -p "$T/mail" "$T/state"
  printf '#1 from orch-1 [TASK %s] do the thing\n' "$tid" > "$T/mail/inbox"
  TEST_MAIL=$T/mail TEST_STATE=$T/state TEST_ORIGIN=$origin TEST_BRANCH=$branch TEST_BASE=${TEST_BASE:-} \
    TEST_TASK_ID=$tid TEST_VERDICT=$verdict TEST_AMEND=$amend TEST_CANCEL=$cancel \
    TEST_LINES=${TEST_LINES:-300} TEST_VERDICT_SEQ=${TEST_VERDICT_SEQ:-} \
    TEST_MOVE_BASE=${TEST_MOVE_BASE:-} TEST_MOVE_FILE=${TEST_MOVE_FILE:-} \
    TEST_CONFLICT_FILE=${TEST_CONFLICT_FILE:-} TEST_MIGRATION=${TEST_MIGRATION:-} \
    TEST_MIGRATION_ONCE=${TEST_MIGRATION_ONCE:-} \
    TEST_VERIFY_TIMEOUT_LINE=${TEST_VERIFY_TIMEOUT_LINE:-} \
    TEST_VERIFY_SLEEP=${TEST_VERIFY_SLEEP:-2} \
    TEST_IMPL_MODEL=test-impl TEST_VERIFY_MODEL=test-verif \
    TEST_WORK=/work TEST_TMP=$T TEST_WORKER=$T/worker-start.sh \
    SWARM_NAME=tester AMAIL_URL=test AMAIL_PROJECT=testproj \
    SWARM_IMPL_MODEL=test-impl SWARM_VERIFY_MODEL=test-verif \
    AMAIL_TOKEN=test-token SWARM_DEPLOY_KEY_B64=$(printf test-key | base64) \
    SWARM_MAX_ROUNDS=$rounds SWARM_PASS_TIMEOUT=60 SWARM_VERIFY_TIMEOUT=${TEST_SWARM_VERIFY_TIMEOUT:-60} \
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

# --- REBASE-BEFORE-VERIFY: origin/main advances after the implement pass;
# the branch is rebased onto the new base before the verify, and the DONE
# states the base it was verified on and the timings. ----------------------
o=$T/o-move.git
make_origin "$o" ""
TEST_MOVE_BASE=1
scenario_prologue REBASE "$o" swarm/tm8 tm8 1 PASS 0 0
TEST_MOVE_BASE=
head=$(tail -1 "$T/state/heads.log")
tip=$(git -C "$o" rev-parse refs/heads/swarm/tm8)
base_tip=$(git -C "$o" rev-parse refs/heads/main)
check "branch builds on the moved base" git -C "$o" merge-base --is-ancestor "$base_tip" "$tip"
check "report names the verified base commit" \
  grep -q "^verified on base: $base_tip (origin/main)" "$T/mail/outbox.log"
check "report includes timings" grep -q '^timings: implement .*s, rebase .*s, gates .*s, verify .*s' "$T/mail/outbox.log"
check "no task branch commit predates the moved base" \
  git -C "$o" merge-base --is-ancestor "$base_tip" "$tip"

# --- SMALL DIFF, ONE ROUND: a <200-line diff gets a single verify round even
# when MAX_ROUNDS is 3, so a FAIL ends it rather than looping. ---------------
o=$T/o-small.git
make_origin "$o" ""
TEST_LINES=20
scenario_prologue SMALL-ONE-ROUND "$o" swarm/ts9 ts9 3 FAIL 0 0
TEST_LINES=
check "one verify pass only" expect_eq "$(grep -c 'pass ts9-verify-.*attempt' "$T/last-worker.log")" "1"
check "the log says one round is allowed" grep -q '1 verify round(s)' "$T/last-worker.log"
check "report is [BLOCKED" grep -q '\[BLOCKED ts9\]' "$T/mail/outbox.log"

# --- LARGE DIFF, UP TO THREE ROUNDS: a >200-line diff may use MAX_ROUNDS.
o=$T/o-large.git
make_origin "$o" ""
TEST_LINES=300
scenario_prologue LARGE-THREE-ROUNDS "$o" swarm/tl10 tl10 3 FAIL 0 0
TEST_LINES=
check "all three verify rounds ran" expect_eq "$(grep -c 'pass tl10-verify-.*attempt' "$T/last-worker.log")" "3"

# --- VERIFY TIMEOUT: a timed-out verify reports BLOCKED at once (no second
# identical try) and keeps the partial notes. --------------------------------
o=$T/o-timeout.git
make_origin "$o" ""
TEST_SWARM_VERIFY_TIMEOUT=1 TEST_VERIFY_SLEEP=5
scenario_prologue TIMEOUT "$o" swarm/tt11 tt11 3 TIMEOUT 0 0
TEST_SWARM_VERIFY_TIMEOUT= TEST_VERIFY_SLEEP=
check "only one verify pass ran" expect_eq "$(grep -c 'pass tt11-verify-.*attempt' "$T/last-worker.log")" "1"
check "report is BLOCKED with the timeout" grep -q 'BLOCKED tt11\] retryable: verifier gave no verdict' "$T/mail/outbox.log"
check "report says it timed out" grep -q 'timed out after' "$T/mail/outbox.log"

# --- VERIFY-TIMEOUT LINE: the task's 'Verify-Timeout:' overrides the default.
o=$T/o-vtline.git
make_origin "$o" ""
TEST_VERIFY_TIMEOUT_LINE=1 TEST_VERIFY_SLEEP=5 TEST_SWARM_VERIFY_TIMEOUT=600
scenario_prologue VERIFY-TIMEOUT-LINE "$o" swarm/tv12 tv12 1 TIMEOUT 0 0
TEST_VERIFY_TIMEOUT_LINE= TEST_VERIFY_SLEEP= TEST_SWARM_VERIFY_TIMEOUT=
check "the task timeout was honoured" grep -q 'timed out after 1 min' "$T/mail/outbox.log"

# --- PASS AFTER A REBASE CONFLICT: the implement pass adds a file and main
# adds the same file (TEST_MOVE_BASE); the rebase conflicts, the implementer
# is asked to resolve it, and the task passes with no rebase left in
# progress. -------------------------------------------------------------------
o=$T/o-conflict.git
make_origin "$o" ""
TEST_CONFLICT_FILE=1 TEST_MOVE_BASE=1 TEST_MOVE_FILE=conflict.txt
scenario_prologue CONFLICT-RESOLVED "$o" swarm/tc13 tc13 1 PASS 0 0
TEST_CONFLICT_FILE= TEST_MOVE_BASE= TEST_MOVE_FILE=
check "report is [DONE after resolving" grep -q '\[DONE tc13\]' "$T/mail/outbox.log"
check "the resolved file landed on origin's branch" \
  git -C "$o" cat-file -e refs/heads/swarm/tc13:conflict.txt
check "the log shows a rebase conflict was resolved" \
  grep -q 'rebase conflict' "$T/last-worker.log"
check "no rebase is left in progress in the final report" \
  bash -c '! grep -q "rebase --abort" "$1"' _ "$T/mail/outbox.log"

# --- MIGRATION GUARD: a branch that adds a migration below the base's
# highest fails the gate after the rebase; a gate failure does not consume
# a verify round, so the implement pass reruns. With MAX_ROUNDS=1 the
# iteration bound stops after two guard failures, and no verify pass runs.
# ---------------------------------------------------------------------------
o=$T/o-mig.git
make_origin_migrations "$o"
TEST_MIGRATION=below
run_worker "$o" swarm/tmig tmig 1 PASS 0 0
TEST_MIGRATION=
check "worker exits cleanly" expect_eq "$?" "0"
check "the migration guard failed the gate" grep -q 'is not numbered above the base highest' "$T/last-worker.log"
check "the guard failure did not consume a verify round" \
  expect_eq "$(grep -c 'tmig-verify' "$T/last-worker.log")" "0"
check "report is [BLOCKED with no verdict" grep -q 'BLOCKED tmig\] retryable: verifier did not pass' "$T/mail/outbox.log"
check "no DONE was reported" bash -c '! grep -q "\[DONE tmig\]" "$1"' _ "$T/mail/outbox.log"

# --- MIGRATION GUARD RETAKE: round 1 fails the guard, round 2 fixes the
# migration, and the verifier does run and pass — DONE is reachable after a
# gate failure. This is the regression the old counting defect blocked.
# ---------------------------------------------------------------------------
o=$T/o-mig-retry.git
make_origin_migrations "$o"
TEST_MIGRATION=below TEST_MIGRATION_ONCE=1 TEST_LINES=20
scenario_prologue MIGRATION-GUARD-RETRY "$o" swarm/tmigr tmigr 3 PASS 0 0
TEST_MIGRATION= TEST_MIGRATION_ONCE= TEST_LINES=
check "the guard failed once" grep -q 'is not numbered above the base highest' "$T/last-worker.log"
check "the verifier ran once after the retry" \
  expect_eq "$(grep -c 'pass tmigr-verify-.*attempt' "$T/last-worker.log")" "1"
check "no verify round was spent on the guard failure" \
  expect_eq "$(grep -c 'next implement round (no verify round used)' "$T/last-worker.log")" "1"
check "report is [DONE" grep -q '\[DONE tmigr\]' "$T/mail/outbox.log"

if [ "$fails" -eq 0 ]; then
  echo "all scenarios passed"
  exit 0
fi
echo "$fails check(s) failed; last worker log follows:" >&2
cat "$T/last-worker.log" >&2 || true
exit 1
