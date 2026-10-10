#!/bin/sh
# Release guard: the push run of ci.yml on main for the commit must have
# succeeded on its first attempt. A rerun that turned green does not count:
# it may hide a flaky failure that nobody looked at.
# Usage: release-ci-green.sh COMMIT [REPO]   (REPO defaults to $GITHUB_REPOSITORY)
# COMMIT is a full SHA: the API ignores an empty head_sha and would return
# the runs of other commits.
# Waits while the run is not completed, polling every RELEASE_CI_INTERVAL
# seconds (default 30) for at most RELEASE_CI_TIMEOUT seconds (default 1800).
# Exit 0 when the run succeeded with run_attempt 1; 1 when there is no such
# run, it did not succeed, it was rerun, it vanished or it did not finish in
# time; 4 when the runs cannot be read.
set -u
case $# in 1 | 2) ;; *) echo "usage: release-ci-green.sh COMMIT [REPO]" >&2; exit 2 ;; esac
commit=$1
repo=${2:-${GITHUB_REPOSITORY:-}}
case $commit in
*[!0-9a-f]* | '') echo "not a full commit SHA: $commit" >&2; exit 4 ;;
esac
[ ${#commit} -eq 40 ] || { echo "not a full commit SHA: $commit" >&2; exit 4; }
interval=${RELEASE_CI_INTERVAL:-30}
timeout=${RELEASE_CI_TIMEOUT:-1800}
number() { case $1 in '' | *[!0-9]*) return 1 ;; esac; }
waited=0
seen=
while :; do
  runs=$(gh api "repos/$repo/actions/workflows/ci.yml/runs?head_sha=$commit&event=push&branch=main") ||
    { echo "cannot read the CI runs of $commit" >&2; exit 4; }
  # The newest run: "<id> <status> <conclusion> <run_attempt>", or "none".
  run=$(printf '%s\n' "$runs" | jq -r '.workflow_runs | sort_by(.created_at) | last
    | if . == null then "none" else "\(.id) \(.status) \(.conclusion) \(.run_attempt)" end' 2>/dev/null) ||
    { echo "cannot parse the CI runs of $commit" >&2; exit 4; }
  if [ "$run" = none ]; then
    if [ -n "$seen" ]; then
      echo "CI run $seen of $commit is gone" >&2
    else
      echo "no push run of ci.yml on main for $commit" >&2
    fi
    exit 1
  fi
  # shellcheck disable=SC2086 # split the four fields
  set -- $run
  if [ $# -ne 4 ] || ! number "$1" || ! number "$4"; then
    echo "cannot parse the CI runs of $commit: $run" >&2
    exit 4
  fi
  id=$1 status=$2 conclusion=$3 attempt=$4
  seen=$id
  [ "$status" = completed ] && break
  if [ "$waited" -ge "$timeout" ]; then
    echo "CI run $id of $commit is still $status after ${waited}s" >&2
    exit 1
  fi
  echo "CI run $id of $commit is $status, waiting"
  sleep "$interval"
  waited=$((waited + interval))
done
echo "CI run $id of $commit: conclusion $conclusion, run_attempt $attempt"
if [ "$conclusion" != success ]; then
  echo "CI run $id did not succeed" >&2
  exit 1
fi
if [ "$attempt" != 1 ]; then
  echo "CI run $id passed on attempt $attempt; only a first attempt counts" >&2
  exit 1
fi
