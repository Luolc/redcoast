#!/bin/sh
# Runs the synthetic cases of hi.py that CI covers, one after the other, on
# the made-up accounts in fixtures/inventory, and prints one line per case
# with its exit status and seconds. No credential is used and no account is
# reached: the gateway talks to a fake API through a fake exit. Needs Docker,
# passwordless sudo for tcpdump, uv, curl, and static builds of redcoast and
# redcoast-client. The Claude CLI is downloaded at the pinned version and
# checked against its pinned SHA-256.
#
#   ci-cases.sh <redcoast> <redcoast-client> <new output directory>
#
# Exits 1 when any case failed; each case's log is <output>/<case>.log.
set -u

CLI_VERSION=2.1.295
CLI_TARBALL_SHA256=d45a2fa14a7ea0b8ab4f56e502de5893c2b816078c71c4d8ab2e4142148f3d95

if [ "$#" -ne 3 ]; then
  echo 'usage: ci-cases.sh <redcoast> <redcoast-client> <new output directory>' >&2
  exit 2
fi
gateway=$(realpath -- "$1") launcher=$(realpath -- "$2") out=$3
mkdir -- "$out" || exit 1
out=$(realpath -- "$out")
cd "$(dirname "$0")" || exit 1

tarball="$out/claude-code-linux-x64-$CLI_VERSION.tgz"
curl -fsSL -o "$tarball" \
  "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-$CLI_VERSION.tgz" || exit 1
echo "$CLI_TARBALL_SHA256  $tarball" | sha256sum -c - || exit 1
mkdir "$out/cli" && tar -xzf "$tarball" -C "$out/cli" package/claude || exit 1
cli="$out/cli/package/claude"

failed=0
run() { # <name> <hi.py arguments...>
  name=$1
  shift
  start=$(date +%s)
  uv run --no-project --python 3.14 --with pyyaml==6.0.3 python hi.py \
    --gateway-binary "$gateway" --cli-binary "$cli" \
    --inventory fixtures/inventory --account example-a --out-root "$out" \
    "$@" >"$out/$name.log" 2>&1
  code=$?
  echo "$name exit=$code seconds=$(($(date +%s) - start))"
  if [ "$code" -ne 0 ]; then
    failed=1
    tail -n 3 "$out/$name.log"
    # The run's stage and its failure and exit-code readings; the run holds
    # only made-up values.
    result=$(sed -n 's/^{"result": "\([^"]*\)".*/\1/p' "$out/$name.log" | tail -n 1)
    [ -n "$result" ] && python3 - "$result" <<'PY'
import json, sys
report = json.load(open(sys.argv[1]))
for key, value in sorted(report.items()):
  if key == "stage" or "fail" in key or key.endswith("exit_code"):
    print(f"  {key}: {json.dumps(value)[:300]}")
PY
  fi
}

for case in hi turns stream tool restart; do
  run "$case" --case "$case"
done
run mitm --case hi --mitm
run tui --case tui --launcher-binary "$launcher"
run cache --case cache --launcher-binary "$launcher" --second-account example-b
for arm in local-429 upstream-429 local-503 upstream-503 switch; do
  run "limits-$arm" --case limits --limits-arm "$arm" --launcher-binary "$launcher"
done
exit "$failed"
