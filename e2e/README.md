# Gateway end-to-end tests in Docker

`xmachine.py` starts a gateway container and client containers on one Docker network and checks the cross-machine session interface, and how sessions end, through the real `redcoast-client` launcher. Everything is synthetic: the account inventory, the account and proxy credentials (handed to the gateway's stdin test entry), the machine credentials, the model upstream and the exit proxy. Nothing reaches 1Password, api.anthropic.com or Tailscale, and no Claude CLI runs: the `claude` the launcher starts is `xmachine_probe.sh`.

The gateway container (`python:3.13-slim`) runs the gateway together with a fake upstream and a fake exit, since `upstream` only takes a loopback address. The client containers (`alpine:3.20`) each have their own address.

## Running

Needs Docker, Go and Python 3.11 or later (standard library only). From the directory that holds `go.mod`:

```sh
out=$(mktemp -d)
CGO_ENABLED=0 go build -o "$out/redcoast" ./cmd/redcoast
CGO_ENABLED=0 go build -o "$out/redcoast-client" ./cmd/redcoast-client
python3 e2e/xmachine.py --gateway-binary "$out/redcoast" --launcher-binary "$out/redcoast-client"
```

Each arm prints one `PASS` or `FAIL` line; the last two lines are `cleanup remaining=0 remaining_after_5s=0` and `report <path> outcome=True`. The exit status is 0 only when every arm passed and the cleanup left nothing. The run's files and `report.json` go to a new temporary directory, or to `--out <dir>`. `--keep` leaves a failed run's containers for a look; the script prints the command that removes them. `--subnet` changes the Docker network (default `172.30.77.0/24`).

CI runs the same command on the runner's own Docker.

## Arms

1. `register_machine_a`: `machine add client-a <sha256> --max-sessions 1`.
2. `a_inference_and_connect`: client A gets a session through `redcoast-client` in network mode (`REDCOAST_CLIENT_ADDRESS` and `REDCOAST_CLIENT_CREDENTIAL_FILE`); the fake `claude` sends one inference (200) and one CONNECT (200), then holds the session.
3. `status_live_sessions_after_a`: `status` reports one live session.
4. `a_second_session_over_limit`: A's second launcher is refused at the limit of 1 before `claude` starts: exit 3, `machine_limit`.
5. `b_wrong_machine_credential`: client B with an unregistered credential: exit 3, `unknown_machine`.
6. `a_session_credential_from_b`: A's session credential from B's address: inference 401, CONNECT 407.
7. `a_session_credential_from_a_control`: the same credential from A's address: 200 and 200.
8. `revoke_machine_a`: `machine revoke` ends one session.
9. `a_session_after_revocation`: A's session credential afterwards: 401 and 407.
10. `a_relaunch_after_revocation`: a new launcher on A: `unknown_machine`.
11. `machine_list_shows_revocation`: `machine list` shows `client-a` with `revoked_at`.
12. `c_claude_exits_session_ends`: machine C (limit 1) launches a fake `claude` that exits 0 after its two requests. The launcher exits 0 and revokes the session: `end_reason` is `client`, and the launch metadata holds the working directory.
13. `c_claude_killed_session_ends`: C's fake `claude` is killed with SIGKILL while it holds its session. The launcher exits 137 and still revokes: `end_reason` is `client`.
14. `c_no_cwd_launcher_refuses`: the launcher starts in a directory that has been removed. It exits 3 with a `getwd` error before asking for a session; C's session count does not change.
15. `c_launcher_killed_session_stays_live`: the launcher itself is killed with SIGKILL (`docker kill`; it is the container's first process). Nothing revokes the session: it stays live with no `end_reason`. The gateway ends it only after the idle expiry, seven days without a request, which the configuration does not change.
16. `c_slot_held_by_killed_launcher`: so C's next launch is refused at its limit: exit 3, `machine_limit`.
17. `d_issue_without_cwd_registers`: machine D asks the session interface for a session directly, with launch metadata that has no `cwd`. The gateway answers 201 and the session is live, with no `cwd` recorded.
18. `no_credential_in_gateway_log`: no machine credential, session credential, account token or proxy password appears in the gateway log. The log only counts as read when it holds the line the gateway writes at startup for the synthetic account.

The session arms read the gateway's database (`sessions` table) read-only with the gateway container's Python.

## Guards of the script itself

- Secret values never go into a Docker command line: machine credentials travel as read-only mounted files, the session credential on stdin. `sh()` refuses to run a command whose arguments hold one. Errors and the report name only the step and its exit status; stderr excerpts are redacted.
- A SIGTERM runs the same cleanup, and the report is still written (`outcome=False`).

## Not covered

A real Tailscale link and ACLs; a real `claude` on the client (the fake one sends one inference and one CONNECT); several machines asking for sessions at once.
