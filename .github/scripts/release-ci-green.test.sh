#!/bin/sh
# Arms of release-ci-green.sh, with gh replaced by a stub that replays one
# canned API response per call: a green first attempt, a green run after a
# wait, the newest of two runs, no run, a failed run, a rerun, a run that
# never finishes, a run that vanishes, an API error, two unreadable answers and
# two commits that are not a full SHA.
set -u
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/bin/sh
# Prints $STUB/<n> for the n-th call, or the last one once they run out, and
# exits with the status in $STUB/<n>.exit when there is one.
n=$(($(cat "$STUB/calls" 2>/dev/null || echo 0) + 1))
echo "$n" >"$STUB/calls"
printf '%s\n' "$*" >>"$STUB/args"
while [ "$n" -gt 1 ] && [ ! -e "$STUB/$n" ]; do n=$((n - 1)); done
cat "$STUB/$n"
[ -e "$STUB/$n.exit" ] && exit "$(cat "$STUB/$n.exit")"
exit 0
EOF
chmod +x "$tmp/bin/gh"
run() { # <id> <status> <conclusion> <run_attempt> <created_at>
  printf '{"id":%s,"status":"%s","conclusion":%s,"run_attempt":%s,"created_at":"%s"}' "$@"
}
runs() { printf '{"total_count":%s,"workflow_runs":[%s]}\n' "$1" "$2"; }
sha=0123456789abcdef0123456789abcdef01234567
green=$(run 7 completed '"success"' 1 2026-10-10T01:00:00Z)
running=$(run 7 in_progress null 1 2026-10-10T01:00:00Z)
arm=0
status=0
expect() { # <want exit> <name> <response>... : one response per gh call
  arm=$((arm + 1))
  stub="$tmp/arm$arm"
  mkdir "$stub"
  want=$1 name=$2
  shift 2
  i=0
  for response in "$@"; do
    i=$((i + 1))
    case $response in
    # A failing gh is not trusted, whatever it printed.
    exit:*) runs 1 "$green" >"$stub/$i"; echo "${response#exit:}" >"$stub/$i.exit" ;;
    *) printf '%s\n' "$response" >"$stub/$i" ;;
    esac
  done
  STUB=$stub PATH="$tmp/bin:$PATH" RELEASE_CI_INTERVAL=1 RELEASE_CI_TIMEOUT=${timeout:-3} \
    "$here/release-ci-green.sh" "${commit-$sha}" o/r >/dev/null 2>&1
  got=$?
  [ "$got" -eq "$want" ] || { echo "FAIL: $name: exit $got, want $want"; status=1; }
}
expect 0 "green first attempt" "$(runs 1 "$green")"
grep -qxF "api repos/o/r/actions/workflows/ci.yml/runs?head_sha=$sha&event=push&branch=main" "$tmp/arm$arm/args" ||
  { echo "FAIL: green first attempt: unexpected query $(cat "$tmp/arm$arm/args")"; status=1; }
expect 0 "green after a wait" "$(runs 1 "$running")" "$(runs 1 "$green")"
expect 0 "newest of two runs" "$(runs 2 "$(run 8 completed '"success"' 1 2026-10-10T02:00:00Z),$(run 7 completed '"failure"' 1 2026-10-10T01:00:00Z)")"
expect 1 "no run" "$(runs 0 '')"
expect 1 "failed run" "$(runs 1 "$(run 7 completed '"failure"' 1 2026-10-10T01:00:00Z)")"
expect 1 "rerun" "$(runs 1 "$(run 7 completed '"success"' 2 2026-10-10T01:00:00Z)")"
timeout=1
expect 1 "never finishes" "$(runs 1 "$running")"
timeout=
[ "$(cat "$tmp/arm$arm/calls")" -eq 2 ] || { echo "FAIL: never finishes: $(cat "$tmp/arm$arm/calls") calls, want 2"; status=1; }
expect 1 "vanishes" "$(runs 1 "$running")" "$(runs 0 '')"
expect 4 "API error" exit:1
expect 4 "unreadable answer" '{"message":"unexpected"}'
expect 4 "run without run_attempt" '{"total_count":1,"workflow_runs":[{"id":7,"status":"completed","conclusion":"success","created_at":"2026-10-10T01:00:00Z"}]}'
commit=
expect 4 "empty commit" "$(runs 1 "$green")"
commit=abc
expect 4 "short commit" "$(runs 1 "$green")"
unset commit
[ "$status" -eq 0 ] && echo "release-ci-green: ok"
exit "$status"
