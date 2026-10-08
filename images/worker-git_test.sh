#!/usr/bin/env bash
# Tests for images/worker-git.sh: the rebase and migration-guard rules the
# worker loop uses. These run against throwaway local git repos (no model,
# no Agent Mail, no network) so the numbering and rebase logic is covered
# without the full worker.
#
#   bash images/worker-git_test.sh
set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=worker-git.sh
. "$here/worker-git.sh"

fails=0
check() {  # check DESC CMD ARGS...
  local desc=$1; shift
  if "$@"; then echo "  ok: $desc"; else echo "  FAIL: $desc"; fails=$((fails + 1)); fi
}
expect_eq() { [ "$1" = "$2" ]; }

command -v git >/dev/null || { echo "git is required"; exit 1; }

T=$(mktemp -d /tmp/worker-git-test.XXXXXX)
trap 'rm -rf "$T"' EXIT

# seed REPO: a repo with two migrations on main.
seed() {
  local s=$1
  git init -q -b main "$s"
  mkdir -p "$s/store/migrations"
  printf 'a\n' > "$s/store/migrations/0001_init.sql"
  printf 'b\n' > "$s/store/migrations/0002_x.sql"
  printf 'base\n' > "$s/README.md"
  git -C "$s" add -A
  git -C "$s" commit -q -m base
}

# make_bare ORIGIN: a bare repo whose HEAD points at main (a bare `git
# init` defaults to master, so a clone warns and checks nothing out).
make_bare() {
  git init -q --bare "$1"
  git -C "$1" symbolic-ref HEAD refs/heads/main
}

# clone_work ORIGIN WORK: a working clone with main fetched as origin/main.
clone_work() {
  local o=$1 w=$2
  rm -rf "$w"; git clone -q "$o" "$w"
  git -C "$w" config user.name test; git -C "$w" config user.email test@example.com
}

# --- rebase: UPTODATE, REBASED, CONFLICT, helper state -----------------------
o=$T/o1.git; s=$T/s1; seed "$s"; make_bare "$o"; git -C "$s" push -q "$o" main
w=$T/w1; clone_work "$o" "$w"
v=$(worker_rebase "$w" origin/main)
check "rebase is UPTODATE when the base is an ancestor" expect_eq "$v" "UPTODATE"

git -C "$w" switch -q -c feat
printf 'branch\n' > "$w/branch.txt"; git -C "$w" add -A; git -C "$w" commit -q -m work
s2=$T/s1b; rm -rf "$s2"; git clone -q "$o" "$s2"
git -C "$s2" config user.name test; git -C "$s2" config user.email test@example.com
printf 'moved\n' >> "$s2/README.md"; git -C "$s2" commit -qam "main moved"; git -C "$s2" push -q origin main
git -C "$w" fetch -q origin
v=$(worker_rebase "$w" origin/main)
check "rebase reports REBASED after the base moves" expect_eq "$v" "REBASED"
check "rebase brought the base in" \
  git -C "$w" merge-base --is-ancestor origin/main HEAD
if worker_rebase_in_progress "$w"; then rp=1; else rp=0; fi
check "no rebase is left in progress" expect_eq "$rp" "0"

# conflict: both sides change the same file and the branch is rebased onto
# the moved base; the rebase is left in progress for the resolve pass.
o2=$T/o2.git; s3=$T/s3; seed "$s3"; make_bare "$o2"; git -C "$s3" push -q "$o2" main
w2=$T/w2; clone_work "$o2" "$w2"
git -C "$w2" switch -q -c feat
printf 'branch side\n' > "$w2/README.md"; git -C "$w2" commit -qam "branch edit"
s4=$T/s4; rm -rf "$s4"; git clone -q "$o2" "$s4"
git -C "$s4" config user.name test; git -C "$s4" config user.email test@example.com
printf 'main side\n' > "$s4/README.md"; git -C "$s4" commit -qam "main edit"; git -C "$s4" push -q origin main
git -C "$w2" fetch -q origin
out=$(worker_rebase "$w2" origin/main); rc=$?
case $out in CONFLICT*) cf=1 ;; *) cf=0 ;; esac
check "conflicting rebase reports CONFLICT" expect_eq "$cf" "1"
check "conflicting rebase returns 3 (resolvable)" expect_eq "$rc" "3"
check "conflicting rebase is left in progress" worker_rebase_in_progress "$w2"
check "the conflicted file is named" bash -c 'grep -q README <<<"$1"' _ "$out"
git -C "$w2" rebase --abort >/dev/null 2>&1 || true
git -C "$w2" rebase --abort >/dev/null 2>&1 || true

# --- dirty tree: worker_rebase commits uncommitted work first (B2) -----------
# An implement round can leave the tail of its work uncommitted. A rebase must
# not fail on a dirty tree, and the next attempt's reset/clean must not be able
# to wipe it, so the work is committed as a wip commit before rebasing.
o4=$T/o4.git; s6=$T/s6; seed "$s6"; make_bare "$o4"; git -C "$s6" push -q "$o4" main
w4=$T/w4; clone_work "$o4" "$w4"
git -C "$w4" switch -q -c feat
printf 'committed\n' > "$w4/branch.txt"; git -C "$w4" add -A; git -C "$w4" commit -q -m work
printf 'tracked base\n' > "$w4/tracked.txt"; git -C "$w4" add tracked.txt; git -C "$w4" commit -q -m "tracked file"
printf 'uncommitted tail\n' > "$w4/tail.txt"         # untracked, never added
printf 'tracked edit\n' >> "$w4/tracked.txt"          # tracked, unstaged
s7=$T/s6b; rm -rf "$s7"; git clone -q "$o4" "$s7"
git -C "$s7" config user.name test; git -C "$s7" config user.email test@example.com
printf 'moved\n' >> "$s7/README.md"; git -C "$s7" commit -qam "main moved"; git -C "$s7" push -q origin main
git -C "$w4" fetch -q origin
v=$(worker_rebase "$w4" origin/main 2>/dev/null)
check "dirty tree still rebases (REBASED)" expect_eq "$v" "REBASED"
check "the uncommitted tail survived as a commit" \
  git -C "$w4" cat-file -e HEAD:tail.txt
check "the tracked edit survived too" \
  bash -c 'git -C "$1" show HEAD:tracked.txt | grep -q "tracked edit"' _ "$w4"
check "the tree is clean after the rebase" \
  bash -c '[ -z "$(git -C "$1" status --porcelain)" ]' _ "$w4"
if worker_rebase_in_progress "$w4"; then rp2=1; else rp2=0; fi
check "no rebase is left in progress after a dirty rebase" expect_eq "$rp2" "0"

# --- migration guard ---------------------------------------------------------
o3=$T/o3.git; s5=$T/s5; seed "$s5"; make_bare "$o3"; git -C "$s5" push -q "$o3" main
w3=$T/w3; clone_work "$o3" "$w3"
git -C "$w3" switch -q -c feat
printf 'c\n' > "$w3/store/migrations/0003_branch.sql"; git -C "$w3" add -A; git -C "$w3" commit -q -m mig
check "a new migration above the base passes" worker_migration_guard "$w3" origin/main

printf 'd\n' > "$w3/store/migrations/0003_dup.sql"; git -C "$w3" add -A; git -C "$w3" commit -q -m dup
out=$(worker_migration_guard "$w3" origin/main); rc=$?
check "two files sharing a version fail the guard" expect_eq "$rc" "1"
check "the duplicate version is named" bash -c 'grep -q "version 3" <<<"$1"' _ "$out"
git -C "$w3" reset -q --hard HEAD~1

printf 'e\n' > "$w3/store/migrations/0002_below.sql"; git -C "$w3" add -A; git -C "$w3" commit -q -m below
out=$(worker_migration_guard "$w3" origin/main); rc=$?
check "a migration not above the base highest fails" expect_eq "$rc" "1"
check "the offending migration is named" bash -c 'grep -q 0002_below <<<"$1"' _ "$out"
git -C "$w3" reset -q --hard HEAD~1

# A base migration renamed by the branch is a rename, not a new migration:
# the message names the real problem instead of blaming the version number.
git -C "$w3" mv store/migrations/0002_x.sql store/migrations/0002_y.sql
git -C "$w3" commit -q -m rename
out=$(worker_migration_guard "$w3" origin/main); rc=$?
check "renaming a base migration fails the guard" expect_eq "$rc" "1"
check "the rename is reported as a rename" bash -c 'grep -q "was renamed" <<<"$1"' _ "$out"
git -C "$w3" reset -q --hard HEAD~1

# A base migration deleted by the branch is reported as a deletion.
git -C "$w3" rm -q store/migrations/0002_x.sql
git -C "$w3" commit -q -m delete
out=$(worker_migration_guard "$w3" origin/main); rc=$?
check "deleting a base migration fails the guard" expect_eq "$rc" "1"
check "the deletion is reported as a deletion" bash -c 'grep -q "was deleted" <<<"$1"' _ "$out"
git -C "$w3" reset -q --hard HEAD~1

check "a branch with no migrations passes" bash -c 'git -C "$1" reset -q --hard origin/main' _ "$w3"
check "a branch with no migrations passes" worker_migration_guard "$w3" origin/main
check "versions are listed sorted and zero-padded-free" expect_eq \
  "$(worker_migrations "$w3" HEAD | cut -d' ' -f1 | tr '\n' ',')" "1,2,"

echo
if [ "$fails" -eq 0 ]; then
  echo "all worker-git checks passed"
  exit 0
fi
echo "$fails check(s) failed" >&2
exit 1
