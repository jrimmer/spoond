#!/usr/bin/env bash
# worker-git — the git and migration rules of the worker loop
# (images/worker-start.sh), kept in their own file so they can be tested
# without a model or Agent Mail.
#
# The worker fetches the task's Base, rebases the task branch onto it
# before verifying and again before reporting DONE, and guards the
# migration numbering after every rebase. Nothing here pushes or
# rewrites the base: the worker only ever moves the task branch.
#
# Callers source this file; it defines functions and sets no shell options.
# Do not add `set -e` (or any option) here: the loop already runs under
# `set -uo pipefail` and changing that would silently change its error
# semantics.

# worker_migrations WT REV: the numeric version and file name of every
# store/migrations/*.sql file at REV, as "<version> <file>" lines sorted
# by version. The version is decimal, so 0018 and 018 compare equal.
worker_migrations() {
  local wt=$1 rev=$2 f name v
  git -C "$wt" ls-tree -r --name-only "$rev" -- store/migrations 2>/dev/null \
    | grep -E '^store/migrations/[0-9]+[^/]*\.sql$' | while IFS= read -r f; do
        name=${f#store/migrations/}
        v=$(sed -nE 's/^([0-9]+).*/\1/p' <<<"$name")
        printf '%d %s\n' "$((10#$v))" "$name"
      done | sort -n -k1,1
}

# worker_base_sha WT BASE_REF: the full base commit, or nothing.
worker_base_sha() {
  git -C "$1" rev-parse -q --verify "$2^{commit}" 2>/dev/null
}

# worker_fetch WT BASE_REF: refresh origin so BASE_REF is the latest base.
# Returns 1 when origin is unreachable or BASE_REF does not exist.
worker_fetch() {
  local wt=$1 base_ref=$2
  git -C "$wt" fetch -q origin 2>/dev/null || return 1
  git -C "$wt" rev-parse -q --verify "$base_ref^{commit}" >/dev/null 2>&1
}

# worker_rebase WT BASE_REF: bring the checked-out branch onto BASE_REF.
# Prints one status line:
#   UPTODATE               BASE_REF is already an ancestor of HEAD
#   REBASED                the branch was rebased onto BASE_REF
#   CONFLICT <file>...     the rebase stopped on conflicts; the working
#                          tree holds the markers and the rebase is left
#                          in progress so the caller (an implement round)
#                          can resolve it and run `git rebase --continue`
# Returns 0 for UPTODATE/REBASED, 3 for CONFLICT, 1 for any other error
# (which aborts the rebase).
worker_rebase() {
  local wt=$1 base=$2 conflicts
  if git -C "$wt" merge-base --is-ancestor "$base" HEAD 2>/dev/null; then
    echo "UPTODATE"
    return 0
  fi
  if GIT_EDITOR=true GIT_SEQUENCE_EDITOR=true \
       git -C "$wt" rebase "$base" >/dev/null 2>&1; then
    echo "REBASED"
    return 0
  fi
  conflicts=$(git -C "$wt" diff --name-only --diff-filter=U 2>/dev/null | tr '\n' ' ')
  if [ -n "$conflicts" ]; then
    echo "CONFLICT $conflicts"
    return 3
  fi
  git -C "$wt" rebase --abort >/dev/null 2>&1 || true
  echo "ERROR rebase failed"
  return 1
}

# worker_rebase_in_progress WT: true while a rebase is stopped mid-way.
worker_rebase_in_progress() {
  local gitdir
  gitdir=$(git -C "$1" rev-parse --git-dir 2>/dev/null) || return 1
  case $gitdir in /*) ;; *) gitdir=$1/$gitdir ;; esac
  [ -d "$gitdir/rebase-merge" ] || [ -d "$gitdir/rebase-apply" ]
}

# worker_migration_guard WT BASE_REF: the migration-numbering gate.
# Fails when two migrations on the branch share a version number, or when
# a migration the branch adds is not numbered above the base's highest.
# A base migration the branch moved or removed is reported as such (git's
# rename detection tells a genuine add from a moved base file), so the
# message names the real problem. Prints one line per problem and returns
# 1; prints nothing and returns 0 when the numbering is sound (or
# store/migrations is absent).
worker_migration_guard() {
  local wt=$1 base=$2 bad=0 v name base_high=0 st src dst
  local head_all base_all head_versions

  head_all=$(worker_migrations "$wt" HEAD)
  [ -n "$head_all" ] || return 0
  base_all=$(worker_migrations "$wt" "$base")
  head_versions=$(cut -d' ' -f1 <<<"$head_all")

  # 1. No two migrations may share a version number.
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    bad=1
    printf 'migration version %s is used more than once:\n' "$v"
    while read -r mv name; do
      [ "$mv" = "$v" ] && printf '  %s\n' "$name"
    done <<<"$head_all"
  done < <(printf '%s\n' "$head_versions" | sort -n | uniq -d)

  # 2. Every migration the branch adds must be above the base's highest.
  # git's rename detection separates a genuine add from a base migration
  # that merely moved (same content, new name) or was removed, so each
  # gets the right message instead of a misleading "not numbered" line.
  # With no base migrations the highest is 0, so every add is still
  # checked against it, as before.
  [ -n "$base_all" ] && base_high=$(cut -d' ' -f1 <<<"$base_all" | sort -n | tail -1)
  while IFS=$'\t' read -r st src dst; do
    # Only migration files matter; a non-SQL or non-numeric file under
    # store/migrations is not a migration and must not trip the guard.
    case ${src#store/migrations/} in
      [0-9]*.sql) ;;
      *) continue ;;
    esac
    case $st in
      A*)
        name=${src#store/migrations/}
        v=$(sed -nE 's/^([0-9]+).*/\1/p' <<<"$name")
        if [ -n "$v" ] && [ "$((10#$v))" -le "$base_high" ]; then
          bad=1
          printf 'migration %s is not numbered above the base highest (%s)\n' \
            "$name" "$base_high"
        fi ;;
      R*)
        bad=1
        printf 'base migration %s was renamed to %s; do not move a base migration\n' \
          "${src#store/migrations/}" "${dst#store/migrations/}" ;;
      D*)
        bad=1
        printf 'base migration %s was deleted; do not remove a base migration\n' \
          "${src#store/migrations/}" ;;
    esac
  done < <(git -C "$wt" diff --name-status -M "$base" HEAD -- store/migrations 2>/dev/null)
  return "$bad"
}
