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
#   SWARM_VERIFY_TIMEOUT seconds per verify pass (SWARM_PASS_TIMEOUT),
#   SWARM_IDLE_TIMEOUT seconds without a task before leaving (900),
#   SWARM_GATES           the project's gates, one command per line (from
#                         hive.yaml); every task must pass them
#
# A task is a mail whose subject starts "[TASK <id>]". Its body may begin with
#   Repo: <git url>
#   Branch: <branch>
#   Base: <branch>
# lines; the rest is the task text. Without Branch:, the branch is swarm/<id>.
# Base: names the branch the task builds on (default main): a new task
# branch starts from origin/<base>, and the verifier's diff, the commit
# count and the push decision are all measured against it.
set -uo pipefail

: "${SWARM_NAME:?}" "${AMAIL_URL:?}" "${AMAIL_PROJECT:?}" "${SWARM_IMPL_MODEL:?}" \
  "${SWARM_VERIFY_MODEL:?}" "${AMAIL_TOKEN:?}" "${SWARM_DEPLOY_KEY_B64:?}"
ORCH=${SWARM_ORCH:-orch-1}
CC=${SWARM_CC:-jason-0}
THINKING=${SWARM_THINKING:-high}
MAX_ROUNDS=${SWARM_MAX_ROUNDS:-3}
PASS_TIMEOUT=${SWARM_PASS_TIMEOUT:-3600}
VERIFY_TIMEOUT=${SWARM_VERIFY_TIMEOUT:-$PASS_TIMEOUT}
IDLE_TIMEOUT=${SWARM_IDLE_TIMEOUT:-900}

W=/work
mkdir -p $W/repos $W/tasks
exec >>$W/worker.log 2>&1
echo "worker-start $SWARM_NAME $(date -Is)"
status() { echo "$1" > $W/status; echo "status: $1"; }
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

export PI_CODING_AGENT_DIR=$W/pi
mkdir -p $PI_CODING_AGENT_DIR
python3 - "$SWARM_NAME" "$SWARM_IMPL_MODEL" "$SWARM_VERIFY_MODEL" > $PI_CODING_AGENT_DIR/models.json <<'PY'
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

amail register "$SWARM_NAME" --program pi --model "$SWARM_IMPL_MODEL" \
  --task "project worker for $AMAIL_PROJECT" || { status failed; exit 1; }
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
      *"from $ORCH"*"[TASK "*|*"from jason"*"[TASK "*|*"from HumanOverseer"*"[TASK "*)
        echo "$id"; return ;;
      *"from $ORCH"*"[WAIT "*)
        local mins; mins=$(sed -nE 's/.*\[WAIT ([0-9]+)\].*/\1/p' <<<"$line")
        [ -n "$mins" ] && echo $(( $(date +%s) + mins * 60 )) > $W/wait_until
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

# llm_error LOG: the model service's error message if the pass ended on one
# (Pi exits 0 even then; its JSON log records stopReason "error").
llm_error() {
  tail -c 4000 "$1" | grep -q '"stopReason":"error"' || return 1
  tail -c 4000 "$1" | grep -o '"errorMessage":"[^"]*"' | tail -1 | cut -d'"' -f4
}

pass() {  # pass DIR LOGNAME MODEL PROMPT [TIMEOUT]
  # PASS_TIMED_OUT is 1 when the pass ran out of time (timeout exit 124):
  # it ended normally as far as Pi knows, but did not finish.
  # A model-service failure (e.g. llm.lacy.casa 502 while its host is
  # stalled) is retried here with backoff and does not use up a round; only
  # after 6 failed attempts does the task fail, as an infrastructure block.
  local attempt err limit=${5:-$PASS_TIMEOUT}
  PASS_TIMED_OUT=0
  for attempt in 1 2 3 4 5 6; do
    echo "pass $2 ($3) attempt $attempt $(date -Is)"
    ( cd "$1" && timeout "$limit" pi -p "$4" --provider llm --model "$3" \
        --thinking "$THINKING" --no-session --mode json </dev/null > "$W/logs-$2.jsonl" 2>&1 ) &
    local pid=$!
    watch_pass "$pid" "$1" "$2" &
    local wpid=$!
    wait "$pid"; local rc=$?
    kill "$wpid" 2>/dev/null; wait "$wpid" 2>/dev/null
    echo "pass $2 exit=$rc $(date -Is)"
    [ "$rc" -eq 124 ] && PASS_TIMED_OUT=1 && echo "pass $2: timed out after ${limit}s"
    err=$(llm_error "$W/logs-$2.jsonl") || return 0
    echo "pass $2: model service error: $err"
    [ "$attempt" -eq 1 ] && printf 'Model service error in %s: %s. Retrying with backoff.\n' "$2" "$err" \
      | say "$TASK_ID" "[PROGRESS $TASK_ID] waiting for llm.lacy.casa"
    until curl -s -m 20 -o /dev/null -w '%{http_code}' https://llm.lacy.casa/v1/models | grep -q 200; do sleep 30; done
    sleep $(( attempt * 60 ))
  done
  LLM_DOWN="$err"
  return 1
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
  { echo "pushed: $sha -> $branch ($ok under --force-with-lease)"
    [ "$branch" != "$2" ] && echo "note: history was rewritten, so the task branch was left at its old tip; HEAD went to $branch instead"
    echo
    cat
  } | say "$id" "$subject"
}

# run_task MSGID: one complete task, reported in its own thread.
run_task() {
  local msg=$1 raw subject id body repo branch slug wt base rules round verdict findings commits
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
  amail ack "$msg" >/dev/null 2>&1 || true
  status "task $id"
  printf '%s\n' "$body" > $W/tasks/$id.md

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

  findings="" verdict=""
  for round in $(seq 1 "$MAX_ROUNDS"); do
    if cancelled "$id"; then
      git -C "$wt" add -A && git -C "$wt" commit -q -m "wip: $id" || true
      push_and_report "$wt" "$branch" "$id" "[CANCELLED $id]" <<EOF
Cancelled; work in progress pushed.
EOF
      return
    fi
    status "task $id: implementing (round $round)"
    if [ "$round" -eq 1 ]; then
      pass "$wt" "$id-implement-$round" "$SWARM_IMPL_MODEL" "Implement this task completely.

$(cat $W/tasks/$id.md)

$rules
When finished, reply with a short summary of what you changed." || break
    else
      pass "$wt" "$id-implement-$round" "$SWARM_IMPL_MODEL" "You are continuing a task. A reviewer found problems with the current branch. Fix every finding, then re-run the gates.

TASK:
$(cat $W/tasks/$id.md)

REVIEWER FINDINGS:
$findings

$rules" || break
    fi
    echo "Round $round implement pass finished; verifying with $SWARM_VERIFY_MODEL." \
      | say "$id" "[PROGRESS $id] round $round: verifying"

    # A verify pass that ends without a verdict (it ran out of time, or
    # never wrote the file) is not a FAIL: there are no findings to fix, so
    # another implement round would be blind. Verify once more; if that
    # also gives no verdict, stop and hand the branch to the orchestrator
    # with what the verifier had found so far.
    local vtry vlog
    for vtry in 1 2; do
      status "task $id: verifying (round $round, try $vtry)"
      vlog=$id-verify-$round; [ "$vtry" -gt 1 ] && vlog=$vlog-$vtry
      rm -f $W/verdict.md
      pass "$wt" "$vlog" "$SWARM_VERIFY_MODEL" "You are an independent reviewer with fresh eyes. Review the change on branch $branch against its task.

TASK:
$(cat $W/tasks/$id.md)

You have $(( VERIFY_TIMEOUT / 60 )) minutes. Keep your notes in $W/verdict.md as you go, so nothing is lost if you run out of time.

Steps:
1. Create $W/verdict.md now with the single line PENDING.
2. Read the whole diff: git diff $BASE_REF...HEAD
3. Check every requirement in the task is met exactly, and look hard for bugs. Add each finding to $W/verdict.md as soon as you have it, one per line (file:line and what is wrong).
4. Run every gate the task names (once each) and add the results to $W/verdict.md.
5. Do not fix anything yourself.
6. Replace the first line of $W/verdict.md with exactly PASS or FAIL.

$rules" "$VERIFY_TIMEOUT" || break 2
      verdict=$(head -1 $W/verdict.md 2>/dev/null | tr -d '[:space:]')
      findings=$(tail -n +2 $W/verdict.md 2>/dev/null)
      case $verdict in PASS|FAIL) break ;; esac
      local why="ended without a verdict"
      [ "$PASS_TIMED_OUT" = 1 ] && why="timed out after $(( VERIFY_TIMEOUT / 60 )) min"
      verdict=TIMEOUT
      findings="Verifier $why (round $round, try $vtry).
Partial notes:
${findings:-none}
$(recent_tools "$W/logs-$vlog.jsonl" 8)"
      echo "task $id round $round verify try $vtry: $why"
      [ "$vtry" -eq 1 ] && printf '%s\n' "$findings" | say "$id" "[PROGRESS $id] round $round: verifier $why; verifying again"
    done
    echo "task $id round $round verdict: $verdict"
    [ "$verdict" = PASS ] || [ "$verdict" = TIMEOUT ] && break
  done

  commits=$(git -C "$wt" log --format='%h %s' "$BASE_REF"..HEAD)
  if [ "$verdict" = PASS ] && [ -n "$commits" ]; then
    push_and_report "$wt" "$branch" "$id" "[DONE $id]" <<EOF
commits:
$(echo "$commits" | sed 's/^/  /')
verifier: PASS in round $round ($SWARM_VERIFY_MODEL)
$(cat $W/verdict.md)
EOF
    return
  fi
  git -C "$wt" add -A && git -C "$wt" commit -q -m "wip: $id" || true
  sha=$(git -C "$wt" rev-parse --short=7 HEAD)
  if [ -n "$(git -C "$wt" log --format=%h "$BASE_REF"..HEAD)" ]; then
    if [ -n "$LLM_DOWN" ]; then
      push_and_report "$wt" "$branch" "$id" "[BLOCKED $id] retryable: infrastructure (model service)" <<EOF
The model service kept failing ($LLM_DOWN).
HEAD: $sha, pushed to $branch. Retry when the model service is healthy.
EOF
      return
    fi
    if [ "$verdict" = TIMEOUT ]; then
      push_and_report "$wt" "$branch" "$id" "[BLOCKED $id] retryable: verifier gave no verdict" <<EOF
The verifier gave no verdict twice in round $round, so no implement round was spent on it.
HEAD: $sha, pushed to $branch; review it by hand.

$findings
EOF
      return
    fi
    push_and_report "$wt" "$branch" "$id" "[BLOCKED $id] retryable: verifier did not pass" <<EOF
No PASS after $MAX_ROUNDS rounds (last verdict: ${verdict:-none}).
HEAD: $sha, pushed to $branch.

Last findings:
${findings:-none recorded}
EOF
  else
    if [ -n "$LLM_DOWN" ]; then
      printf 'The model service kept failing (%s); nothing was committed, so there is nothing to push.\n' "$LLM_DOWN" \
        | say "$id" "[BLOCKED $id] retryable: infrastructure (model service)"
      return
    fi
    if [ "$verdict" = TIMEOUT ]; then
      { echo "The verifier gave no verdict twice in round $round, so no implement round was spent on it. Nothing was committed, so there is nothing to push."; echo
        echo "$findings"; } | say "$id" "[BLOCKED $id] retryable: verifier gave no verdict"
      return
    fi
    { echo "No PASS after $MAX_ROUNDS rounds (last verdict: ${verdict:-none}). Nothing was committed, so there is nothing to push."; echo
      echo "Last findings:"; echo "${findings:-none recorded}"; } | say "$id" "[BLOCKED $id] retryable: verifier did not pass"
  fi
}

# --- main loop: ready -> task -> ready ... until idle ---------------------------
while true; do
  status idle
  echo "Ready for a task." | amail send --to "$ORCH" --subject "[READY] $SWARM_NAME" || true
  deadline=$(( $(date +%s) + IDLE_TIMEOUT ))
  rm -f $W/wait_until
  msg=""
  while [ -z "$msg" ] && [ "$(date +%s)" -lt "$deadline" ]; do
    left=$(( deadline - $(date +%s) )); [ "$left" -gt 600 ] && left=600
    amail wait --timeout "$left" --interval 20 >/dev/null || sleep 20
    heartbeat
    msg=$(next_task_msg)
    if [ -f $W/wait_until ]; then
      deadline=$(cat $W/wait_until); rm -f $W/wait_until
      status "idle (holding until $(date -d @$deadline -Is))"
    fi
  done
  [ -z "$msg" ] && break
  run_task "$msg"
done

status left
echo "No task arrived within ${IDLE_TIMEOUT}s; leaving." | amail send --to "$ORCH" --cc "$CC" --subject "[BYE] $SWARM_NAME"
