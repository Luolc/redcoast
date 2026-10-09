#!/bin/sh
# Arms of release-tag-guard.sh: matching tag, other tags, a file without the
# constant, no file.
set -u
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf 'package main\n\n// version is this release.\nconst version = "1.2.3"\n\nconst other = "9.9.9"\n' >"$tmp/version.go"
printf 'package main\n\nconst other = "1.2.3"\n' >"$tmp/none.go"
status=0
expect() { # <want exit> <tag> <file>
  "$here/release-tag-guard.sh" "$2" "$3" 2>/dev/null
  got=$?
  [ "$got" -eq "$1" ] || { echo "FAIL: $2 with $3: exit $got, want $1"; status=1; }
}
expect 0 v1.2.3 "$tmp/version.go"
expect 1 v1.2.4 "$tmp/version.go"
expect 1 1.2.3 "$tmp/version.go"
expect 4 v1.2.3 "$tmp/none.go"
expect 4 v1.2.3 "$tmp/missing.go"
[ "$status" -eq 0 ] && echo "release-tag-guard: ok"
exit "$status"
