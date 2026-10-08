#!/bin/bash
# worker-start — a project-scoped agent worker. Takes tasks over Agent Mail,
# one at a time, until none arrive within the idle timeout, then exits.
#
# The sandbox lives for the project; the agent's context lives for one task:
# every task runs fresh Pi processes on a freshly reset checkout. Repos and
# toolchain caches persist between tasks. Part of the worker layer
# (images/worker.dockerfile), so it runs on any base image.
#
# While idle, a "[WAIT <minutes>]" from the orchestrator extends the idle
# deadline by that many minutes (more work is coming).
#
# Started by swarm-spawn via the lease exec API, detached, with params:
#   SWARM_NAME            this worker's Agent Mail identity, e.g. swarm-3
#   AMAIL_URL, AMAIL_PROJECT   Agent Mail server and project
#   SWARM_IMPL_MODEL      model for implement passes (llm.lacy.casa name)
#   SWARM_VERIFY_MODEL    model for verify passes
# secrets (environment only; never put in prompts or mail):
#   AMAIL_TOKEN           Agent Mail bearer token
#   SWARM_DEPLOY_KEY_B64  base64 private key that can push the project's repos
# optional:
#   SWARM_ORCH (orch-1), SWARM_CC (jason-0), SWARM_THINKING (high),
#   SWARM_MAX_ROUNDS (3), SWARM_PASS_TIMEOUT seconds per Pi pass (3600),
#   SWARM_VERIFY_TIMEOUT seconds per verify round (1200); a task's
#                       'Verify-Timeout:' line (minutes) overrides it,
#   SWARM_LLM_MAX_WAIT seconds a pass may wait in the gateway's queue before
#                         the task blocks (default 3600; 0 = no bound),
#   SWARM_LLM_REQUEST_TIMEOUT seconds for one model request inside Pi (600),
#   SWARM_REGISTER_BACKOFF base seconds between boot registration retries (15),
#   SWARM_IDLE_TIMEOUT seconds without a task before leaving (900),
#   SWARM_GATES           the project's gates, one command per line (from
#                         hive.yaml); every task must pass them
#
# LLM capacity is 4 concurrent committed requests with a burst to 8 from
# an as-available queue (slower). A pass that spends its wall clock waiting
# on a saturated gateway has not used the model, so the wait is measured
# and charged to LLM_WAIT_SECS, not VERIFY_SECS; a pass may also be given a
# longer wall clock while the gateway is queueing (SWARM_LLM_MAX_WAIT). A
# single slow response never fails the task: per-request timeouts are
# generous and transient 429/503/timeouts are retried with backoff inside
# Pi (see the settings.json written below).
#
# A task is a mail whose subject starts "[TASK <id>]". Its body may begin with
#   Repo: <git url>
#   Branch: <branch>
#   Base: <branch>
#   Verify-Timeout: <minutes>
# lines; the rest is the task text. Without Branch:, the branch is swarm/<id>.
# Base: names the branch the task builds on (default main): a new task
# branch starts from origin/<base>, and the verifier's diff, the commit
# count and the push decision are all measured against it. Before the
# branch is verified (and again before a DONE is pushed) it is rebased onto
# the fetched base; a conflicted rebase is resolved by the implementer in a
# separate implement pass and every gate is rerun. A round whose gates fail
# (including the migration guard) does not spend a verify round: it goes
# straight to another implement pass, bounded by a separate iteration cap.
# Verify-Timeout: caps one verify round in minutes (default 20).
set -uo pipefail
# Mail is read through a few pipelines whose last stage is a sed/grep;
# lastpipe keeps that stage in the current shell so the intent is explicit
# (the loops here do not need to mutate the parent, so behaviour does not
# depend on it). Bash-only, like the pipefail above.
shopt -s lastpipe

: "${SWARM_NAME:?}" "${AMAIL_URL:?}" "${AMAIL_PROJECT:?}" "${SWARM_IMPL_MODEL:?}" \
  "${SWARM_VERIFY_MODEL:?}" "${AMAIL_TOKEN:?}" "${SWARM_DEPLOY_KEY_B64:?}"
ORCH=${SWARM_ORCH:-orch-1}
CC=${SWARM_CC:-jason-0}
THINKING=${SWARM_THINKING:-high}
MAX_ROUNDS=${SWARM_MAX_ROUNDS:-3}
PASS_TIMEOUT=${SWARM_PASS_TIMEOUT:-3600}
VERIFY_TIMEOUT=${SWARM_VERIFY_TIMEOUT:-1200}
IDLE_TIMEOUT=${SWARM_IDLE_TIMEOUT:-900}
# LLM capacity 4 committed + burst to 8 from a queue. Waiting on a
# saturated gateway is not model work: bound how long a pass may wait
# (0 = unbounded) and give each model request a generous timeout so a
# slow response never fails the task.
LLM_MAX_WAIT=${SWARM_LLM_MAX_WAIT:-3600}
LLM_REQUEST_TIMEOUT=${SWARM_LLM_REQUEST_TIMEOUT:-600}
# The gateway health probe used to tell "waiting in its queue" from model
# work, and how often run_pass samples it. Overridable for tests.
LLM_HEALTH_URL=${SWARM_LLM_HEALTH_URL-https://llm.lacy.casa/v1/models}
LLM_POLL=${SWARM_LLM_POLL:-15}
REGISTER_BACKOFF=${SWARM_REGISTER_BACKOFF:-15}
# How long the model-service retry waits for the gateway to come back
# before trying anyway (bounded so a wrong health URL cannot hang the loop).
LLM_RECOVER_TRIES=${SWARM_LLM_RECOVER_TRIES:-20}

# The git and migration rules (worker_rebase, worker_migration_guard) live
# beside this script so they can be tested without a model or Agent Mail.
W=/work
WGIT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/worker-git.sh
if [ -r "$WGIT" ]; then
  # shellcheck source=/dev/null
  . "$WGIT"
else
  echo "worker-git.sh not found beside $0" >&2
fi
mkdir -p "$W/repos" "$W/tasks"
exec >>"$W/worker.log" 2>&1
echo "worker-start $SWARM_NAME $(date -Is)"
status() { echo "$1" > "$W/status"; echo "status: $1"; }
status starting

export HOME=/root AMAIL_AS=$SWARM_NAME AMAIL_URL AMAIL_PROJECT
# No public egress: toolchains use the caches the image's warm step filled.
if command -v go >/dev/null; then export GOPROXY=off GOFLAGS=-mod=readonly; fi
if command -v mix >/dev/null; then export HEX_OFFLINE=1; fi

# Resolve *.lacy.casa through Technitium (LAN answers); the template's
# router resolver returns the public edge address, which the restricted
# network does not allow.
printf 'nameserver 10.1.0.2\nnameserver 10.1.0.1\noptions timeout:2 attempts:2\n' > /etc/resolv.conf

# --- credentials and model config, from the environment only ---------------
# Secrets move out of the environment into private files, so the agent's
# shell commands (env, printenv) do not surface them.
install -d -m 700 /root/.config/amail
printf '%s\n' "$AMAIL_TOKEN" > /root/.config/amail/bearer && chmod 600 /root/.config/amail/bearer
unset AMAIL_TOKEN
install -d -m 700 /root/.ssh
printf '%s' "$SWARM_DEPLOY_KEY_B64" | base64 -d > /root/.ssh/swarm_key && chmod 600 /root/.ssh/swarm_key
unset SWARM_DEPLOY_KEY_B64
export GIT_SSH_COMMAND="ssh -i /root/.ssh/swarm_key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
git config --global user.name jrimmer
git config --global user.email jason@rimmer.net

export PI_CODING_AGENT_DIR="$W/pi"
mkdir -p "$PI_CODING_AGENT_DIR"
python3 - "$SWARM_NAME" "$SWARM_IMPL_MODEL" "$SWARM_VERIFY_MODEL" > "$PI_CODING_AGENT_DIR/models.json" <<'PY'
import json, sys
name, *models = sys.argv[1:]
print(json.dumps({"providers": {"llm": {
    "baseUrl": "https://llm.lacy.casa/v1",
    "api": "openai-completions",
    "apiKey": name,  # not a secret: identifies this worker in llm.lacy.casa's logs
    "models": [{"id": m, "contextWindow": 200000, "maxTokens": 32768, "reasoning": True}
               for m in dict.fromkeys(models)],
}}}, indent=1))
PY

# Per-request timeout and retry live in Pi, not in this loop: one slow
# response (the gateway queueing behind its 4 committed + 8 burst
# capacity) must never fail the task, and a transient 429/503/timeout is
# retried with backoff. A generous request timeout is paired with
# Pi's own retries so a long generation is never cut off.
python3 - "$LLM_REQUEST_TIMEOUT" > "$PI_CODING_AGENT_DIR/settings.json" <<'PY'
import json, sys
timeout_ms = int(sys.argv[1]) * 1000
print(json.dumps({
    "retry": {
        "enabled": True,
        "maxRetries": 6,
        "baseDelayMs": 2000,
        "maxAgentDelayMs": 60000,
        "provider": {"timeoutMs": timeout_ms, "maxRetries": 3, "maxRetryDelayMs": 60000},
    },
}, indent=1))
PY

# The first registration call at boot retries with backoff: one DNS or
# network blip must not fail the worker before it has done any work.
register_worker() {
  local attempt
  for attempt in 1 2 3 4 5; do
    if amail register "$SWARM_NAME" --program pi --model "$SWARM_IMPL_MODEL" \
         --task "project worker for $AMAIL_PROJECT"; then
      return 0
    fi
    echo "amail register attempt $attempt failed; retrying"
    sleep $(( attempt * REGISTER_BACKOFF ))
  done
  return 1
}
register_worker || { status failed; exit 1; }
printf 'model: implement %s, verify %s\nidle timeout: %ss\n' "$SWARM_IMPL_MODEL" "$SWARM_VERIFY_MODEL" "$IDLE_TIMEOUT" \
  | amail send --to "$ORCH" --cc "$CC" --subject "[HELLO] $SWARM_NAME"

# --- helpers -------------------------------------------------------------------
say() {  # say THREAD SUBJECT  (body on stdin)
  amail send --to "$ORCH" --cc "$CC" --thread "$1" --subject "$2" || echo "amail send failed: $2"
}

# Read unread mail; act on overrides and cancels; print the id of the first
# [TASK] message, if any. Messages from anyone but the orchestrator or Jason
# are read and ignored.
next_task_msg() {
  local ids id line
  ids=$(amail inbox | sed -nE 's/^#([0-9]+) .*/\1/p')
  for id in $ids; do
    line=$(amail inbox --all --limit 200 | grep -m1 "^#$id ")
    case "$line" in
      *"from $ORCH"*"[TASK "*|*"from jason"*"[TASK "*)
        echo "$id"; return ;;
      *"from $ORCH"*"[WAIT "*)
        local mins; mins=$(sed -nE 's/.*\[WAIT ([0-9]+)\].*/\1/p' <<<"$line")
        case $mins in
          ''|*[!0-9]*) : ;;  # no (or malformed) minutes: ignore the override
          *) echo $(( $(date +%s) + mins * 60 )) > "$W/wait_until" ;;
        esac
        amail read "$id" >/dev/null ;;
      *) amail read "$id" >/dev/null ;;  # mark read; nothing else to do between tasks
    esac
  done
}

cancelled() {  # cancelled ID -> true if a [CANCEL ID] is waiting
  amail inbox | grep -q "\[CANCEL $1\]"
}

# heartbeat: tell spoond this lease is in use. A bee never calls the lease
# API, so without this the backend's idle sweep suspends its persistent
# lease. The lease id is the capability (guest-service port, like the LLM
# gateway); spoond rate-limits writes. Failure is harmless.
heartbeat() {
  [ -n "${SPOOND_GATEWAY_URL:-}" ] && [ -n "${SPOOND_LEASE_ID:-}" ] || return 0
  curl -s -m 10 -o /dev/null -X POST "$SPOOND_GATEWAY_URL/lease/$SPOOND_LEASE_ID/active" || true
}

# recent_tools LOG N: how many tool calls a pass made, and its latest N.
recent_tools() {
  python3 - "$1" "$2" <<'PY'
import json, sys
n, recent = 0, []
try:
    f = open(sys.argv[1], errors="replace")
except OSError:
    f = []
for line in f:
    if '"tool_execution_start"' not in line: continue
    n += 1
    try:
        e = json.loads(line); a = e.get("args") or {}
        what = a.get("command") or a.get("path") or a.get("file_path") or ""
        recent.append(f'{e.get("toolName")} {" ".join(str(what).split())[:70]}')
    except ValueError: pass
print(f"tool calls: {n}; latest:")
for r in recent[-int(sys.argv[2]):]: print("  " + r)
PY
}

# watch_pass PID DIR LOGNAME: while the pass runs, every PROGRESS_EVERY
# seconds report facts from its own log: elapsed time, tool calls so far and
# the latest ones, the branch's diff size, and whether the log is still
# growing ("no activity for N min" is the honest stuck signal). Lives and
# dies with the pass.
watch_pass() {
  local pid=$1 dir=$2 name=$3 start now last_size=0 size idle=0
  start=$(date +%s)
  while sleep "${PROGRESS_EVERY:-600}"; do
    kill -0 "$pid" 2>/dev/null || return
    heartbeat
    now=$(date +%s); size=$(stat -c %s "$W/logs-$name.jsonl" 2>/dev/null || echo 0)
    if [ "$size" -eq "$last_size" ]; then idle=$(( idle + ${PROGRESS_EVERY:-600} )); else idle=0; fi
    last_size=$size
    { printf '%s: %d min elapsed' "$name" $(( (now - start) / 60 ))
      [ "$idle" -gt 0 ] && printf ', NO log activity for %d min' $(( idle / 60 ))
      echo
      recent_tools "$W/logs-$name.jsonl" 4
      echo "branch: $(git -C "$dir" log --oneline "$BASE_REF"..HEAD 2>/dev/null | wc -l) commit(s); diff vs $BASE_REF: $(git -C "$dir" diff --shortstat "$BASE_REF" 2>/dev/null | sed 's/^ //')"
    } | say "$TASK_ID" "[PROGRESS $TASK_ID] $name: $(( (now - start) / 60 )) min"
  done
}

# llm_gateway_busy: true when the model gateway is queueing the request
# (HTTP 429/503) rather than refusing or serving it. A 200 means the
# request was accepted; any other or missing answer is *not* treated as
# queueing, so a dead gateway cannot extend a pass forever (that case is
# the model-service error path below). Overridable for tests via
# SWARM_LLM_HEALTH_URL; an empty URL disables the probe.
llm_gateway_busy() {
  local code
  [ -n "$LLM_HEALTH_URL" ] || return 1
  code=$(curl -s -m 4 -o /dev/null -w '%{http_code}' "$LLM_HEALTH_URL" 2>/dev/null) || return 1
  case $code in 429|503) return 0 ;; *) return 1 ;; esac
}

# llm_retrying LOG: true while Pi's own log shows an automatic retry in
# flight (an auto_retry_start with no matching auto_retry_end), i.e. it hit
# a 429/503/overload and is waiting to try again. This is the direct
# signal that the gateway (4 committed + 8 burst) is queueing us, and it
# does not depend on the health endpoint reporting queue state.
llm_retrying() {
  local last
  last=$(tail -c 40000 "$1" 2>/dev/null \
    | grep -oE '"type":"auto_retry_(start|end)"' | tail -1) || return 1
  [ "$last" = '"type":"auto_retry_start"' ]
}

# pass_waiting LOG: true when the pass is waiting on the model rather than
# doing work — the gateway is queueing (llm_gateway_busy) or Pi is between
# retries (llm_retrying).
pass_waiting() {
  llm_gateway_busy && return 0
  llm_retrying "$1"
}

# llm_error LOG: the model service's error message if the pass ended on one
# (Pi exits 0 even then; its JSON log records stopReason "error").
llm_error() {
  tail -c 4000 "$1" | grep -q '"stopReason":"error"' || return 1
  tail -c 4000 "$1" | grep -o '"errorMessage":"[^"]*"' | tail -1 | cut -d'"' -f4
}

# run_pass DIR LOGNAME MODEL PROMPT LIMIT: one Pi run under a wall-clock
# limit. Time the gateway spends queueing the request (llm_gateway_busy) is
# charged to LLM_WAIT_SECS and does not count against LIMIT, so a pass
# waiting behind the gateway's 4 committed + 8 burst capacity is not cut
# off as if it had used the model. Sets PASS_TIMED_OUT=1 and returns 124
# when the non-waiting clock runs out. A generous hard ceiling (LIMIT plus
# the queue allowance) is the backstop; the monitor kills at LIMIT of
# non-waiting time.
run_pass() {
  local dir=$1 name=$2 model=$3 prompt=$4 limit=$5
  local hard pid wpid rc now last dt work=0 waited=0
  if [ "$LLM_MAX_WAIT" -gt 0 ]; then hard=$(( limit + LLM_MAX_WAIT )); else hard=$(( limit + 43200 )); fi
  # exec so the background pid is `timeout` itself: killing it forwards the
  # signal to Pi, and its own ceiling is only a backstop for the monitor.
  ( cd "$dir" && exec timeout "$hard" pi -p "$prompt" --provider llm --model "$model" \
      --thinking "$THINKING" --no-session --mode json </dev/null ) \
      > "$W/logs-$name.jsonl" 2>&1 &
  pid=$!
  watch_pass "$pid" "$dir" "$name" &
  wpid=$!
  last=$(date +%s)
  while kill -0 "$pid" 2>/dev/null; do
    sleep "$LLM_POLL"
    now=$(date +%s); dt=$(( now - last )); last=$now
    # A gateway queueing us or a Pi retry in flight is model-busy time,
    # not model work: charge it to the wait bucket. Each sample is
    # attributed wholly to one bucket so every wall second lands once.
    if pass_waiting "$W/logs-$name.jsonl"; then waited=$(( waited + dt )); else work=$(( work + dt )); fi
    if [ "$work" -ge "$limit" ] || { [ "$LLM_MAX_WAIT" -gt 0 ] && [ "$waited" -ge "$LLM_MAX_WAIT" ]; }; then
      PASS_TIMED_OUT=1
      kill "$pid" 2>/dev/null || true
      sleep 2
      kill -9 "$pid" 2>/dev/null || true
      break
    fi
  done
  wait "$pid"; rc=$?
  kill "$wpid" 2>/dev/null; wait "$wpid" 2>/dev/null
  add_llm_wait_secs "$waited"
  [ "$PASS_TIMED_OUT" = 1 ] && rc=124
  return "$rc"
}

pass() {  # pass DIR LOGNAME MODEL PROMPT [TIMEOUT]
  # PASS_TIMED_OUT is 1 when the pass ran out of time (exit 124): it ended
  # normally as far as Pi knows, but did not finish. Callers read it right
  # after the call (e.g. the verify round treats PASS_TIMED_OUT=1 as a
  # timeout even when the partial run managed to write PASS).
  # A model-service failure (e.g. llm.lacy.casa 502 while its host is
  # stalled) is retried here with backoff and does not use up a round; the
  # backoff time is charged to LLM_WAIT_SECS, since it is waiting on the
  # model, not model work. Only after 6 failed attempts does the task fail,
  # as an infrastructure block.
  local attempt err limit=${5:-$PASS_TIMEOUT} rc t0 t1
  for attempt in 1 2 3 4 5 6; do
    echo "pass $2 ($3) attempt $attempt $(date -Is)"
    PASS_TIMED_OUT=0
    run_pass "$1" "$2" "$3" "$4" "$limit"; rc=$?
    echo "pass $2 exit=$rc $(date -Is)"
    [ "$rc" -eq 124 ] && PASS_TIMED_OUT=1 && echo "pass $2: timed out after ${limit}s (excluding model wait)"
    err=$(llm_error "$W/logs-$2.jsonl") || return 0
    echo "pass $2: model service error: $err"
    [ "$attempt" -eq 1 ] && printf 'Model service error in %s: %s. Retrying with backoff.\n' "$2" "$err" \
      | say "$TASK_ID" "[PROGRESS $TASK_ID] waiting for llm.lacy.casa"
    t0=$(date +%s)
    local tries=0
    until curl -s -m 20 -o /dev/null -w '%{http_code}' "$LLM_HEALTH_URL" 2>/dev/null | grep -q 200; do
      tries=$(( tries + 1 ))
      [ "$tries" -ge "$LLM_RECOVER_TRIES" ] && break
      sleep 30
    done
    sleep $(( attempt * 60 ))
    t1=$(date +%s); add_llm_wait_secs $(( t1 - t0 ))
  done
  LLM_DOWN="$err"
  return 1
}

# --- rebase, gates and timings --------------------------------------------
# The worker rebases the task branch onto the fetched base before every
# verify and again before a DONE is pushed, then reruns the gates. The
# durations are accumulated here and reported in DONE/BLOCKED so the next
# optimisation is measured.
IMPL_SECS=0 REBASE_SECS=0 GATE_SECS=0 VERIFY_SECS=0 LLM_WAIT_SECS=0

# add_impl_secs / add_rebase_secs / add_gate_secs / add_verify_secs /
# add_llm_wait_secs N: accumulate a duration into its bucket. Named helpers
# rather than an indirection so nothing has to be eval'd.
add_impl_secs()     { IMPL_SECS=$(( IMPL_SECS + ${1:-0} )); }
add_rebase_secs()   { REBASE_SECS=$(( REBASE_SECS + ${1:-0} )); }
add_gate_secs()     { GATE_SECS=$(( GATE_SECS + ${1:-0} )); }
add_verify_secs()   { VERIFY_SECS=$(( VERIFY_SECS + ${1:-0} )); }
add_llm_wait_secs() { LLM_WAIT_SECS=$(( LLM_WAIT_SECS + ${1:-0} )); }

timing_line() {
  printf 'timings: implement %ss, rebase %ss, gates %ss, verify %ss, llm wait %ss' \
    "$IMPL_SECS" "$REBASE_SECS" "$GATE_SECS" "$VERIFY_SECS" "$LLM_WAIT_SECS"
}

# verify_minutes N: a seconds limit as whole minutes, rounded up, so a
# limit that is not a multiple of 60 does not read as less than it is.
verify_minutes() { echo $(( (${1:-0} + 59) / 60 )); }

# no_pass_line VCOUNT ROUNDS_ALLOWED MAX: the BLOCKED headline for a task
# that never reached PASS. When no verify actually ran (every round's gates
# failed before the verifier was asked) it says so instead of claiming N
# verify rounds.
no_pass_line() {
  if [ "${1:-0}" -eq 0 ]; then
    printf 'no verify ran: gates failed'
  else
    printf 'No PASS after %s verify round(s) out of %s' "${2:-0}" "${3:-0}"
  fi
}

# report_already_on_base WT ID ROUND: the honest DONE when a rebase dropped
# every branch commit (the change is already on the base). Reports the base
# and the verifier's verdict instead of the no-commit BLOCKED block.
report_already_on_base() {
  local wt=$1 id=$2 round=$3 base
  base=$(worker_base_sha "$wt" "$BASE_REF")
  { echo "The change is already on the base: the rebase onto $BASE_REF dropped every branch commit, so there is nothing to push."
    echo "already on base: ${base:0:7} ($BASE_REF)"
    [ -n "$regated_base" ] && echo "re-gated on $regated_base ($BASE_REF)"
    echo "verifier: PASS in round $round ($SWARM_VERIFY_MODEL)"
    echo
    cat "$W/verdict.md"
    echo
    echo "$(timing_line)"; } \
    | say "$id" "[DONE $id] already on base ${base:0:7}; nothing to push"
}

# diff_lines WT: added plus deleted lines in git diff BASE_REF...HEAD.
# A binary file's "-" counts as zero so the total stays a number.
diff_lines() {
  git -C "$1" diff --numstat "$BASE_REF...HEAD" 2>/dev/null \
    | awk '{ a += ($1 == "-" ? 0 : $1); d += ($2 == "-" ? 0 : $2) } END { print a + d + 0 }'
}

# rebase_onto WT: fetch the base and rebase the task branch onto it.
# Sets REBASE_STATE (UPTODATE, REBASED, CONFLICT, FETCH or ERROR). On
# CONFLICT the rebase is left in progress so an implement pass can resolve
# it; the caller must finish or abort it. Accumulates REBASE_SECS.
rebase_onto() {
  local wt=$1 t0 t1 out
  t0=$(date +%s)
  if ! worker_fetch "$wt" "$BASE_REF"; then
    REBASE_STATE=FETCH
    return 1
  fi
  out=$(worker_rebase "$wt" "$BASE_REF")
  t1=$(date +%s); add_rebase_secs $(( t1 - t0 ))
  echo "rebase onto $BASE_REF: $out"
  case $out in
    UPTODATE)   REBASE_STATE=UPTODATE; return 0 ;;
    REBASED)    REBASE_STATE=REBASED;  return 0 ;;
    CONFLICT*)  REBASE_STATE=CONFLICT; return 3 ;;
    *)          REBASE_STATE=ERROR;    return 1 ;;
  esac
}

# run_gates WT: the migration guard and the project's gates on the just
# rebased HEAD. Sets GATE_SUMMARY for the verifier prompt, GATE_FAILED=1
# and GATE_OUT (findings for the next implement round) on failure.
run_gates() {
  local wt=$1 out rc line
  GATE_FAILED=0 GATE_OUT=""
  local t0 t1; t0=$(date +%s)
  if ! out=$(worker_migration_guard "$wt" "$BASE_REF" 2>&1); then
    GATE_FAILED=1
    GATE_OUT="Migration guard failed:
$out"
    echo "migration guard failed:"
    printf '%s\n' "$out"
  fi
  if [ -n "${SWARM_GATES:-}" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      out=$( cd "$wt" && bash -c "$line" 2>&1 ); rc=$?
      printf 'gate: %s -> exit %s\n' "$line" "$rc"
      if [ "$rc" -ne 0 ]; then
        GATE_FAILED=1
        GATE_OUT="$GATE_OUT
Gate failed: $line (exit $rc)
$out"
        printf '%s\n' "$out"
      fi
    done <<<"$SWARM_GATES"
  fi
  t1=$(date +%s); add_gate_secs $(( t1 - t0 ))
  if [ "$GATE_FAILED" = 1 ]; then
    GATE_SUMMARY="FAILED after the rebase"
  else
    GATE_SUMMARY="passed after the rebase"
  fi
}

# push_and_report WT BRANCH ID SUBJECT: the one way out of a finished task, called on
# every exit path. origin keeps only what we last pushed, so whatever the
# exit, the current HEAD must land there: the whole point of the round loop
# is a branch the orchestrator (or a retry) can pick up. HEAD is pushed to
# the task branch under --force-with-lease, so a push can never overwrite a
# commit this worker has not seen (another worker's retry, or a fix pushed
# by hand since our clone). The lease expectation is what the worker last
# fetched (empty when the task branch is not on origin yet, which refuses to
# create it if someone else raced us there). When HEAD would have to force
# (history rewritten by a later round, or the branch moved on origin), the
# task branch is left untouched and the commits go to a "-wip-<sha>" branch
# instead, named after the HEAD that needs saving, so a retry that rewrites
# history again lands on a fresh name. It is created under a create-only
# lease and never force-pushed, so the last round is always recoverable and
# nothing can clobber an earlier -wip. The report names exactly what landed
# and where, so a pushed sha is never only in the lease.
push_and_report() {  # push_and_report WT BRANCH ID SUBJECT  (report body on stdin)
  local wt=$1 branch=$2 id=$3 subject=$4 sha tip expect pushes ok out
  sha=$(git -C "$wt" rev-parse --short=7 HEAD)
  tip="refs/heads/$branch"
  expect=$(git -C "$wt" rev-parse -q --verify "refs/remotes/origin/$branch" || true)
  # git needs the lease as "<ref>:<expect>"; empty <expect> is its own form
  # (create-only, not an empty string), so build the arguments as an array.
  # Fast-forward of origin's branch plus a matching lease is the only safe
  # push to the task branch; anything else would have to force, so it goes
  # to a "-wip-<sha>" branch instead, named for the HEAD being saved so a
  # retry with a new rewrite gets a fresh name. Create-only and never
  # force-pushed, it cannot clobber anything, not even another -wip of the
  # same task. The one way its lease can refuse is when origin already has
  # that branch — which for a HEAD-named branch means those same commits are
  # already there, so the push has nothing left to do.
  if [ -n "$expect" ] && ! git -C "$wt" merge-base --is-ancestor "$expect" HEAD 2>/dev/null; then
    branch="$branch-wip-$sha"
    pushes=("--force-with-lease=refs/heads/$branch:")
  else
    pushes=("--force-with-lease=$tip:$expect")
  fi
  echo "pushing $sha to $branch $(date -Is)"
  if out=$(git -C "$wt" push -u origin "${pushes[@]}" HEAD:"refs/heads/$branch" 2>&1); then
    ok=succeeded
  elif [ "$(git -C "$wt" ls-remote origin "refs/heads/$branch" | cut -f1)" = "$(git -C "$wt" rev-parse HEAD)" ]; then
    ok=succeeded; out="origin already has $branch at $sha"
  else
    ok=FAILED
  fi
  echo "push to $branch $ok: $out"
  # A task whose work did not land is not done, whatever the verifier said.
  [ "$ok" = FAILED ] && case $subject in "[DONE "*) subject="[BLOCKED $id] retryable: push failed";; esac
  { echo "pushed: $sha -> $branch ($ok under --force-with-lease)"
    [ "$branch" != "$2" ] && echo "note: history was rewritten, so the task branch was left at its old tip; HEAD went to $branch instead"
    echo
    cat
  } | say "$id" "$subject"
}

# run_task MSGID: one complete task, reported in its own thread.
run_task() {
  local msg=$1 raw subject id body repo branch slug wt base rules round verdict findings commits
  local vt_min VERIFY_LIMIT rounds_allowed verified_base cfl dl t0 t1 regated_base=""
  LLM_DOWN=""
  raw=$(amail read "$msg")
  subject=$(head -1 <<<"$raw")
  id=$(sed -nE 's/.*\[TASK ([^]]+)\].*/\1/p' <<<"$subject")
  TASK_ID=$id
  body=$(tail -n +3 <<<"$raw")
  repo=$(sed -nE 's/^Repo: *([^ ]+).*/\1/p' <<<"$body" | head -1)
  branch=$(sed -nE 's/^Branch: *([^ ]+).*/\1/p' <<<"$body" | head -1)
  : "${branch:=swarm/$id}"
  base_branch=$(sed -nE 's/^Base: *([^ ]+).*/\1/p' <<<"$body" | head -1)
  base_branch=${base_branch#origin/}
  : "${base_branch:=main}"
  BASE_REF=origin/$base_branch
  # A task may cap one verify round in minutes with a 'Verify-Timeout:' line.
  vt_min=$(sed -nE 's/^Verify-Timeout: *([0-9]+).*/\1/p' <<<"$body" | head -1)
  VERIFY_LIMIT=$VERIFY_TIMEOUT
  [ -n "$vt_min" ] && VERIFY_LIMIT=$(( vt_min * 60 ))
  amail ack "$msg" >/dev/null 2>&1 || true
  status "task $id"
  printf '%s\n' "$body" > "$W/tasks/$id.md"

  if [ -z "$repo" ]; then
    echo "The task names no Repo: line." | say "$id" "[BLOCKED $id] permanent: no repo given"
    return
  fi
  slug=$(basename "$repo" .git)
  wt=$W/repos/$slug
  if [ -d "$wt" ]; then git -C "$wt" fetch -q origin; else git clone -q "$repo" "$wt"; fi || {
    printf 'Could not clone or fetch %s (no credentials for it, or unreachable).\n' "$repo" \
      | say "$id" "[BLOCKED $id] permanent: cannot access repo"
    return
  }
  if ! git -C "$wt" rev-parse -q --verify "refs/remotes/$BASE_REF" >/dev/null; then
    printf 'The task names Base: %s, but %s has no such branch.\n' "$base_branch" "$repo" \
      | say "$id" "[BLOCKED $id] permanent: no base branch $base_branch"
    return
  fi
  # Clean start for every task, nothing left over. A retried task resumes
  # from its pushed branch (earlier attempts push their wip); otherwise the
  # branch starts at the task's base (Base:, default origin/main).
  local start=$BASE_REF
  git -C "$wt" rev-parse -q --verify "refs/remotes/origin/$branch" >/dev/null && start=origin/$branch
  if ! { git -C "$wt" switch -q -C "$branch" "$start" && git -C "$wt" reset -q --hard \
         && git -C "$wt" clean -q -fdx; }; then
    printf 'Could not create branch %s from %s.\n' "$branch" "$start" \
      | say "$id" "[BLOCKED $id] retryable: branch setup failed"
    return
  fi
  base=$(git -C "$wt" rev-parse --short HEAD)
  printf 'Repo %s, branch %s from %s %s. Implement with %s, verify with %s, up to %s rounds.\n' \
    "$slug" "$branch" "$start" "$base" "$SWARM_IMPL_MODEL" "$SWARM_VERIFY_MODEL" "$MAX_ROUNDS" | say "$id" "[START $id]"

  gates_rule="."
  if [ -n "${SWARM_GATES:-}" ]; then
    gates_rule=", and always the project's gates:
$(printf '%s\n' "$SWARM_GATES" | sed 's/^/    /')"
  fi
  rules="Rules:
- Work only in $wt, on branch $branch (already checked out). Never switch to, commit to, or push main. Do not push at all; the harness pushes.
- Commit your work with clear conventional commit messages. Never mention AI, assistants, or any model or tool name in commits, code comments or docs.
- Run every gate the task names$gates_rule
- Do not touch production systems or other hosts."

  findings="" verdict="" rounds_allowed=$MAX_ROUNDS verified_base=""
  IMPL_SECS=0 REBASE_SECS=0 GATE_SECS=0 VERIFY_SECS=0 LLM_WAIT_SECS=0
  # The budget is spent on verifies, not loop iterations: an implement
  # round whose gates fail (including the migration guard) restarts the
  # implement pass without using a verify round, so the size-based cap
  # counts only rounds that actually reach the verifier. The cap is
  # recomputed from the diff at each verify, so a diff that grows is
  # still bounded. A separate iteration bound keeps a persistently
  # failing gate from looping forever.
  local round=0 iters=0 vround=0 vlimit=$MAX_ROUNDS
  while [ "$vround" -lt "$vlimit" ] && [ "$iters" -lt $(( MAX_ROUNDS * 2 )) ]; do
    round=$(( round + 1 ))
    iters=$(( iters + 1 ))
    if cancelled "$id"; then
      git -C "$wt" add -A && git -C "$wt" commit -q -m "wip: $id" || true
      push_and_report "$wt" "$branch" "$id" "[CANCELLED $id]" <<EOF
Cancelled; work in progress pushed.
$(timing_line)
EOF
      return
    fi
    status "task $id: implementing (round $round)"
    local t0 t1
    t0=$(date +%s)
    if [ "$round" -eq 1 ]; then
      pass "$wt" "$id-implement-$round" "$SWARM_IMPL_MODEL" "Implement this task completely.

$(cat "$W/tasks/$id.md")

$rules
When finished, reply with a short summary of what you changed." || break
    else
      pass "$wt" "$id-implement-$round" "$SWARM_IMPL_MODEL" "You are continuing a task. A reviewer found problems with the current branch. Fix every finding, then re-run the gates.

TASK:
$(cat "$W/tasks/$id.md")

REVIEWER FINDINGS:
$findings

$rules" || break
    fi
    t1=$(date +%s); add_impl_secs $(( t1 - t0 ))

    # Rebase onto the latest base before verifying (and before reporting
    # DONE). A conflicted rebase is resolved as part of the implement
    # round: the implementer sees the conflict and the base's commits,
    # then every gate is rerun below.
    rebase_onto "$wt"
    if [ "$REBASE_STATE" = CONFLICT ]; then
      local cfl
      cfl=$(git -C "$wt" diff --name-only --diff-filter=U | tr '\n' ' ')
      echo "Round $round rebase conflict in: $cfl; asking the implementer to resolve" \
        | say "$id" "[PROGRESS $id] round $round: resolving rebase conflict"
      status "task $id: resolving rebase conflict (round $round)"
      t0=$(date +%s)
      pass "$wt" "$id-rebase-$round" "$SWARM_IMPL_MODEL" "A rebase of this branch onto the latest $BASE_REF stopped on conflicts. Resolve them now, keeping both the base's intent and this branch's work, then complete the rebase.

TASK:
$(cat "$W/tasks/$id.md")

Conflicted files: $cfl
Finish with \`git rebase --continue\` (set GIT_EDITOR=true) so no rebase is left in progress, then run every gate again.

$rules" || { git -C "$wt" rebase --abort >/dev/null 2>&1 || true; break; }
      t1=$(date +%s); add_impl_secs $(( t1 - t0 ))
      if worker_rebase_in_progress "$wt"; then
        GIT_EDITOR=true git -C "$wt" rebase --continue >/dev/null 2>&1 || true
      fi
      if worker_rebase_in_progress "$wt"; then
        git -C "$wt" rebase --abort >/dev/null 2>&1 || true
        printf 'The rebase onto %s could not be completed cleanly.\n%s\n' "$BASE_REF" "$(timing_line)" \
          | say "$id" "[BLOCKED $id] retryable: rebase conflict"
        return
      fi
    elif [ "$REBASE_STATE" != UPTODATE ] && [ "$REBASE_STATE" != REBASED ]; then
      printf 'Could not fetch or rebase %s onto %s (%s).\n%s\n' "$branch" "$BASE_REF" "$REBASE_STATE" "$(timing_line)" \
        | say "$id" "[BLOCKED $id] retryable: rebase failed"
      return
    fi

    # Migration guard and the project's gates, on the rebased HEAD. A
    # gate failure does not consume a verify round: it goes straight to
    # another implement pass (bounded by the iteration cap above).
    run_gates "$wt"
    if [ "$GATE_FAILED" = 1 ]; then
      echo "task $id round $round: gates failed after rebase; next implement round (no verify round used)"
      findings="$GATE_OUT"
      continue
    fi

    # Size-based round count: a small diff gets a single verify round,
    # a large one up to MAX_ROUNDS. Recomputed at each verify so a diff
    # that grows is still bounded; only verifies consume the count.
    dl=$(diff_lines "$wt")
    if [ "$dl" -lt 200 ]; then vlimit=1; else vlimit=$MAX_ROUNDS; fi
    rounds_allowed=$vlimit
    echo "task $id: diff is $dl changed line(s); $vlimit verify round(s)"
    if [ "$vround" -ge "$vlimit" ]; then
      echo "task $id: round $round past the $vlimit allowed verify round(s); stopping"
      break
    fi
    vround=$(( vround + 1 ))
    verified_base=$(worker_base_sha "$wt" "$BASE_REF")

    # One verify pass per round. Findings first, gates second; the
    # verifier reviews only git diff $BASE_REF...HEAD, never the whole
    # history. A timeout hands the partial notes to the orchestrator at
    # once instead of a second identical try.
    echo "Round $round implement pass finished; verifying with $SWARM_VERIFY_MODEL." \
      | say "$id" "[PROGRESS $id] round $round: verifying"
    status "task $id: verifying (round $round)"
    rm -f "$W/verdict.md"
    t0=$(date +%s)
    pass "$wt" "$id-verify-$round" "$SWARM_VERIFY_MODEL" "You are an independent reviewer with fresh eyes. Review the change on branch $branch against its task.

TASK:
$(cat "$W/tasks/$id.md")

You have $(verify_minutes "$VERIFY_LIMIT") minutes. Keep your notes in $W/verdict.md as you go, so nothing is lost if you run out of time. Review only git diff $BASE_REF...HEAD (plus the task text); never the whole repository history.

Steps:
1. Create $W/verdict.md now with the single line PENDING.
2. Read only the change: git diff $BASE_REF...HEAD
3. Findings first. Check every requirement in the task is met exactly, and look hard for bugs. Add each finding to $W/verdict.md as soon as you have it, one per line, starting with its class:
   BLOCKER: file:line and what is wrong — a requirement not met exactly, a failing gate, a correctness bug, a race or data race, a security gap, or a missing test the task names.
   NOTE: file:line and what is wrong — style, naming, dead code, performance, or a judgement call.
   When unsure, it is a BLOCKER.
4. Gates second. The implementer already ran the project's gates after the rebase: $GATE_SUMMARY. If the diff since that run is empty, summarise those results instead of rerunning every gate; otherwise rerun the gates the task names and record the results.
5. Do not fix anything yourself.
6. Replace the first line of $W/verdict.md with exactly PASS or FAIL: FAIL if any line starts with BLOCKER:, otherwise PASS.

$rules" "$VERIFY_LIMIT" || break
    t1=$(date +%s); add_verify_secs $(( t1 - t0 ))
    verdict=$(head -1 "$W/verdict.md" 2>/dev/null | tr -d '[:space:]')
    findings=$(tail -n +2 "$W/verdict.md" 2>/dev/null)
    # A PASS that lists a blocker is a FAIL: the findings go to the next
    # implement round instead of out as [DONE].
    if [ "$verdict" = PASS ] && grep -qiE '^[[:space:]-]*BLOCKER:' <<<"$findings"; then
      echo "task $id round $round: verifier wrote PASS with blockers; treating as FAIL"
      verdict=FAIL
    fi
    case $verdict in PASS|FAIL) ;; *) verdict=TIMEOUT ;; esac
    # A pass that ran out of wall clock did not finish, whatever partial
    # verdict file it managed to write: treat it as no verdict so the
    # partial notes go out instead of a claimed PASS.
    if [ "${PASS_TIMED_OUT:-0}" = 1 ]; then
      echo "task $id round $round: verify pass timed out; treating as no verdict"
      verdict=TIMEOUT
    fi
    echo "task $id round $round verdict: $verdict"
    [ "$verdict" = PASS ] && break
    [ "$verdict" = TIMEOUT ] && break
  done
  [ "$round" -gt 0 ] || round=1

  commits=$(git -C "$wt" log --format='%h %s' "$BASE_REF"..HEAD)
  if [ "$verdict" = PASS ] && [ -n "$commits" ]; then
    # The base may have moved since the verify; if it did, rebase and
    # re-gate once more so the DONE states the base it was verified on.
    rebase_onto "$wt"
    if [ "$REBASE_STATE" = REBASED ]; then
      echo "task $id: base moved before the push; re-gating on the new base"
      run_gates "$wt"
      if [ "$GATE_FAILED" = 1 ]; then
        printf 'The base moved after the verify and the gates failed again.\n%s\n%s\n' "$GATE_OUT" "$(timing_line)" \
          | say "$id" "[BLOCKED $id] retryable: gates failed on the moved base"
        return
      fi
      verified_base=$(worker_base_sha "$wt" "$BASE_REF")
      regated_base=$verified_base
      commits=$(git -C "$wt" log --format='%h %s' "$BASE_REF"..HEAD)
      # The push-stage rebase can drop the commits too, when the moved
      # base already carries the change. Report the honest DONE rather
      # than pushing a branch with nothing on it.
      if [ -z "$commits" ]; then report_already_on_base "$wt" "$id" "$round"; return; fi
    elif [ "$REBASE_STATE" = CONFLICT ]; then
      git -C "$wt" rebase --abort >/dev/null 2>&1 || true
      printf 'The base moved before the push and the rebase conflicted.\n%s\n' "$(timing_line)" \
        | say "$id" "[BLOCKED $id] retryable: rebase conflict before push"
      return
    elif [ "$REBASE_STATE" != UPTODATE ]; then
      # A fetch/rebase error here must not fall through to the push: the
      # base may have moved and the branch is now neither rebased nor
      # re-gated, so a DONE would name a base it was not verified on.
      printf 'Could not fetch or rebase %s onto %s before the push (%s).\n%s\n' \
        "$branch" "$BASE_REF" "$REBASE_STATE" "$(timing_line)" \
        | say "$id" "[BLOCKED $id] retryable: rebase failed before push"
      return
    fi
    push_and_report "$wt" "$branch" "$id" "[DONE $id]" <<EOF
commits:
$(echo "$commits" | sed 's/^/  /')
verified on base: $verified_base ($BASE_REF)
$([ -n "$regated_base" ] && echo "re-gated on $regated_base ($BASE_REF) after the base moved before the push")
verifier: PASS in round $round ($SWARM_VERIFY_MODEL)
$(cat "$W/verdict.md")

$(timing_line)
EOF
    return
  fi
  # EMPTY-AFTER-REBASE: a PASS whose rebase dropped every branch commit
  # (the change is already on the base). That is an honest DONE, not the
  # "verifier did not pass / nothing to push" block the generic path
  # below would send; the verifier's verdict is reported as it was.
  if [ "$verdict" = PASS ]; then
    report_already_on_base "$wt" "$id" "$round"
    return
  fi
  worker_commit_dirty "$wt" >/dev/null 2>&1 || true
  git -C "$wt" add -A && git -C "$wt" commit -q -m "wip: $id" >/dev/null 2>&1 || true
  sha=$(git -C "$wt" rev-parse --short=7 HEAD)
  if [ -n "$(git -C "$wt" log --format=%h "$BASE_REF"..HEAD)" ]; then
    if [ -n "$LLM_DOWN" ]; then
      push_and_report "$wt" "$branch" "$id" "[BLOCKED $id] retryable: infrastructure (model service)" <<EOF
The model service kept failing ($LLM_DOWN).
HEAD: $sha, pushed to $branch. Retry when the model service is healthy.
$(timing_line)
EOF
      return
    fi
    if [ "$verdict" = TIMEOUT ]; then
      push_and_report "$wt" "$branch" "$id" "[BLOCKED $id] retryable: verifier gave no verdict" <<EOF
The verifier timed out after $(verify_minutes "$VERIFY_LIMIT") min in round $round; review it by hand.
HEAD: $sha, pushed to $branch.

Partial notes:
${findings:-none}
$(recent_tools "$W/logs-$id-verify-$round.jsonl" 8)
$(timing_line)
EOF
      return
    fi
    push_and_report "$wt" "$branch" "$id" "[BLOCKED $id] retryable: verifier did not pass" <<EOF
$(no_pass_line "$vround" "$rounds_allowed" "$MAX_ROUNDS") (last verdict: ${verdict:-none}).
HEAD: $sha, pushed to $branch.

Last findings:
${findings:-none recorded}
$(timing_line)
EOF
  else
    if [ -n "$LLM_DOWN" ]; then
      printf 'The model service kept failing (%s); nothing was committed, so there is nothing to push.\n%s\n' "$LLM_DOWN" "$(timing_line)" \
        | say "$id" "[BLOCKED $id] retryable: infrastructure (model service)"
      return
    fi
    if [ "$verdict" = TIMEOUT ]; then
      { echo "The verifier timed out after $(verify_minutes "$VERIFY_LIMIT") min in round $round; review it by hand. Nothing was committed, so there is nothing to push."; echo
        echo "Partial notes:"; echo "${findings:-none}"; echo "$(timing_line)"; } \
        | say "$id" "[BLOCKED $id] retryable: verifier gave no verdict"
      return
    fi
    { echo "$(no_pass_line "$vround" "$rounds_allowed" "$MAX_ROUNDS") (last verdict: ${verdict:-none}). Nothing was committed, so there is nothing to push."; echo
      echo "Last findings:"; echo "${findings:-none recorded}"; echo "$(timing_line)"; } \
      | say "$id" "[BLOCKED $id] retryable: verifier did not pass"
  fi
}

# --- main loop: ready -> task -> ready ... until idle ---------------------------
while true; do
  status idle
  echo "Ready for a task." | amail send --to "$ORCH" --subject "[READY] $SWARM_NAME" || true
  deadline=$(( $(date +%s) + IDLE_TIMEOUT ))
  rm -f "$W/wait_until"
  msg=""
  while [ -z "$msg" ] && [ "$(date +%s)" -lt "$deadline" ]; do
    left=$(( deadline - $(date +%s) )); [ "$left" -gt 600 ] && left=600
    amail wait --timeout "$left" --interval 20 >/dev/null || sleep 20
    heartbeat
    msg=$(next_task_msg)
    if [ -f "$W/wait_until" ]; then
      deadline=$(cat "$W/wait_until"); rm -f "$W/wait_until"
      status "idle (holding until $(date -d @$deadline -Is))"
    fi
  done
  [ -z "$msg" ] && break
  run_task "$msg"
done

status left
echo "No task arrived within ${IDLE_TIMEOUT}s; leaving." | amail send --to "$ORCH" --cc "$CC" --subject "[BYE] $SWARM_NAME"
