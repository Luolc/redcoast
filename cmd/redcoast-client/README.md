# redcoast-client: starting Claude Code through the gateway

`redcoast-client claude [claude's arguments]` asks the gateway for a session, starts `claude` with the session's settings and passes every argument after `claude` through unchanged. When `claude` exits, the launcher ends the session.

## Install

A release has these files:

| File | Installed as |
| --- | --- |
| `redcoast-linux-amd64` | `redcoast` on the gateway host ([`cmd/redcoast/README.md`](../redcoast/README.md)) |
| `redcoast-client-linux-amd64` | `redcoast-client` |
| `redcoast-client-shim` | `claude`, next to `redcoast-client`, on a machine where only some directories go through the gateway |
| `redcoast-client-shim-full` | `claude`, next to `redcoast-client`, on a machine where every start goes through the gateway |
| `SHA256SUMS` | checks the four files above: `sha256sum -c SHA256SUMS` |

`redcoast-client` alone is enough when you start it yourself (`redcoast-client claude …`). To make a plain `claude` go through the gateway, for tools that start `claude` themselves, install one of the shims as `claude` in the same directory as `redcoast-client`, and put that directory before the real `claude` on `PATH`:

```sh
dir=$HOME/.local/libexec/redcoast-client
install -D -m 0755 redcoast-client-linux-amd64 "$dir/redcoast-client"
install -m 0755 redcoast-client-shim "$dir/claude"   # or redcoast-client-shim-full
export PATH="$dir:$PATH"                             # in the shell's startup file
```

`command -v claude` then prints `$dir/claude`. From source: `CGO_ENABLED=0 go build -trimpath -o redcoast-client ./cmd/redcoast-client`; the shims are the files of the same names in this directory.

## The shims

Both read `$HOME/.config/redcoast-client/shim.env` and start `redcoast-client claude` with the arguments unchanged. The file holds `key=value` lines, read as data: a value may start with `$HOME`, nothing else is expanded, and blank lines and `#` comments are skipped.

```sh
# The gateway's listen.session; https://host:port with TLS.
address=gateway.example.test:7801
credential_file=$HOME/.config/redcoast-client/machine-credential
# The real claude, an absolute path.
claude=/usr/local/bin/claude-real
# Read by redcoast-client-shim only.
prefix=$HOME/work
```

A `#` starts a comment only at the beginning of a line.

- `redcoast-client-shim` sends a start through the gateway only when the working directory is inside `prefix` (an empty `prefix=` means every directory). Outside it, or when anything leaves that undecided (no readable `shim.env`, no `prefix` line, a relative prefix, no working directory), it starts the real `claude` directly: the configured one, else the first other `claude` on `PATH`. Inside the prefix, a broken configuration stops the start with exit 3.
- `redcoast-client-shim-full` never starts the real `claude` itself: every start goes through `redcoast-client`, and any problem with `shim.env` stops it with exit 3. It ignores `prefix`.
- Both unset inherited `REDCOAST_CLIENT_*` variables and refuse a `claude=` that points back at the shim.

## Configure

The launcher reads only environment variables. Exactly one of two modes:

| Variable | Meaning |
| --- | --- |
| `REDCOAST_CLIENT_SOCKET` | Local mode, on the gateway's host: the gateway's `session_socket`. |
| `REDCOAST_CLIENT_ADDRESS` | Network mode: the gateway's `listen.session`, as `host:port`, or `https://host:port` when the gateway serves TLS with a certificate the system trusts. |
| `REDCOAST_CLIENT_CREDENTIAL_FILE` | Network mode: the machine credential file, mode 0600 (see "Add a client machine" in [`cmd/redcoast/README.md`](../redcoast/README.md)). |
| `REDCOAST_CLIENT_CLAUDE` | The `claude` executable; default `claude` on `PATH`. |
| `REDCOAST_CLIENT_MACHINE` | The machine name reported over the socket; default the host name. Over the network the gateway knows the machine by its credential. |

For example, on a client machine:

```sh
export REDCOAST_CLIENT_ADDRESS=https://gateway.example.test:7801
export REDCOAST_CLIENT_CREDENTIAL_FILE=$HOME/.config/redcoast-client/machine-credential
redcoast-client claude
```

## What it checks and sets

Before asking for a session the launcher refuses (exit 3) when anything on the machine would replace the session credential: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, a Bedrock, Vertex or Foundry switch, an override in any Claude Code settings layer or `--settings`, or onboarding not yet done. A gateway refusal (unknown machine, the machine's session limit, no account available) also exits 3 with the gateway's message.

`claude` then runs with `ANTHROPIC_BASE_URL` set to the gateway's reverse proxy, `CLAUDE_CODE_OAUTH_TOKEN` set to the session credential, `HTTPS_PROXY` set to the gateway's forward proxy with the session credential, `NO_PROXY` for the reverse proxy's host, and Claude Code's nonessential traffic, official marketplace auto-install and auto-updater turned off. Every other proxy variable is removed. The launcher forwards SIGINT, SIGTERM and SIGHUP, and on Linux collects the processes `claude` leaves behind.

Exit status: `claude`'s own, 128 plus the signal number when it was killed, or 3 for a failure before `claude` started.

If the launcher itself is killed with SIGKILL, its session stays live, and keeps one of the machine's session slots, until it has been idle for 7 days or the machine is revoked.
