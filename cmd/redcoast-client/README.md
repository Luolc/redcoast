# redcoast-client: starting Claude Code, Pi and Codex CLI through the gateway

`redcoast-client claude [claude's arguments]` asks the gateway for a session, starts `claude` with the session's settings and passes every argument after `claude` through unchanged. `redcoast-client pi …` and `redcoast-client codex …` do the same for Pi and Codex CLI, through the gateway's Codex process ([Pi and Codex CLI](#pi-and-codex-cli)). When the client exits, the launcher ends the session.

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
```

Put the `PATH` line in a startup file the shell reads even when it is not interactive, so that a `claude` started by `zsh -c`, by tmux or over ssh finds the shim too; for zsh that is `~/.zshenv`, not `~/.zshrc`. Put it after every other line that prepends to `PATH`, such as one adding `~/.local/bin`, where the real `claude` often lives:

```sh
export PATH="$HOME/.local/libexec/redcoast-client:$PATH"
```

`zsh -c 'command -v claude'` then prints `$dir/claude`. When a `claude` started without the launcher sends the gateway a credential of its own, such as the person's Claude login, the gateway answers `401 not a redcoast session credential; start claude through redcoast-client`.

From source: `CGO_ENABLED=0 go build -trimpath -o redcoast-client ./cmd/redcoast-client`; the shims are the files of the same names in this directory.

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

`pi` and `codex` read `REDCOAST_CLIENT_CODEX_SOCKET`, `REDCOAST_CLIENT_CODEX_ADDRESS` and `REDCOAST_CLIENT_CODEX_CREDENTIAL_FILE` instead of the first three: the same modes, for the gateway's Codex process, which has its own session interface and its own registered machines. The credential file may be the same one, registered with both processes. Their executables are `REDCOAST_CLIENT_PI` and `REDCOAST_CLIENT_CODEX`, by default `pi` and `codex` on `PATH`.

For example, on a client machine:

```sh
export REDCOAST_CLIENT_ADDRESS=https://gateway.example.test:7801
export REDCOAST_CLIENT_CREDENTIAL_FILE=$HOME/.config/redcoast-client/machine-credential
redcoast-client claude
```

## What it checks and sets

Before asking for a session the launcher refuses (exit 3) when anything on the machine would replace the session credential: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, a Bedrock, Vertex or Foundry switch, an override in any Claude Code settings layer or `--settings`, or onboarding not yet done. A Claude Code settings layer (or `--settings`) may set one of the variables the launcher sets below to turn features off (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, `CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL`, `DISABLE_AUTOUPDATER`) in its `env` map only to the launcher's value, the string `"1"`; any other value, a number included, also exits 3. A gateway refusal (unknown machine, the machine's session limit, no account available) also exits 3 with the gateway's message.

`claude` then runs with `ANTHROPIC_BASE_URL` set to the gateway's reverse proxy, `CLAUDE_CODE_OAUTH_TOKEN` set to the session credential, `HTTPS_PROXY` set to the gateway's forward proxy with the session credential, `NO_PROXY` for the reverse proxy's host, and Claude Code's nonessential traffic, official marketplace auto-install and auto-updater turned off. Every other proxy variable is removed. The launcher forwards SIGINT, SIGTERM and SIGHUP, and on Linux collects the processes `claude` leaves behind.

Exit status: the client's own, 128 plus the signal number when it was killed, or 3 for a failure before the client started.

If the launcher itself is killed with SIGKILL, its session stays live, and keeps one of the machine's session slots, until it has been idle for 7 days or the machine is revoked.

## Pi and Codex CLI

Each client gets a configuration directory of its own, which must hold no login: `~/.config/pi` as Pi's `PI_CODING_AGENT_DIR`, `~/.config/codex` as Codex CLI's `CODEX_HOME`. The launcher creates the directory with mode 0700 and refuses (exit 3) one that others can enter. Your own `~/.pi/agent` and `~/.codex` are not used; copy any settings or skills you want into the new directory.

Both get the session credential in `CODEX_GATEWAY_TOKEN`, `HTTPS_PROXY` set to the gateway's forward proxy with the session credential, and `NO_PROXY` for the gateway's host, `127.0.0.1` and `localhost`. Every other proxy variable is removed.

`redcoast-client pi` refuses to start when `auth.json` in its directory has any login, when the command line has `--api-key`, or when its `--provider` or `--model` names a provider other than `openai-codex`. It also removes from Pi's environment every variable Pi 1.2.0 reads another provider's credential from (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, the AWS and Google Cloud credential variables and the rest; the list is `piProviderVariables` in `pi.go`), so `openai-codex` is the only provider with a credential: Pi uses it whatever a project's `defaultProvider` says or a resumed session last used, instead of sending your own key elsewhere through the gateway's forward proxy. Commands Pi runs do not see those variables either. It writes `models.json` in its directory, replacing what was there, so that Pi's built-in `openai-codex` provider uses the gateway (`<gateway>/backend-api`) with the key `${CODEX_GATEWAY_TOKEN}`: the credential stays out of the file, and several Pi sessions can run from the directory at once. It sets `PI_OFFLINE=1`.

`redcoast-client codex` refuses to start when its directory has an `auth.json`, when a managed configuration exists (`/etc/codex/managed_config.toml`, `/etc/codex/requirements.toml` or `managed_config.toml` in the directory), or when the command line has `--oss`, `--local-provider`, `--remote`, `--remote-auth-token-env` or a `-c` for `model_provider` or `model_providers`. It starts `codex` with these arguments before yours:

```sh
codex --no-daemon \
  -c 'model_provider="gateway"' \
  -c 'model_providers.gateway.name="gateway"' \
  -c 'model_providers.gateway.base_url="<gateway>/backend-api/codex"' \
  -c 'model_providers.gateway.wire_api="responses"' \
  -c 'model_providers.gateway.env_key="CODEX_GATEWAY_TOKEN"' \
  -c 'model_providers.gateway.supports_websockets=false' \
  -c features.plugins=false -c features.shell_snapshot=false -c analytics.enabled=false -c 'otel.metrics_exporter="none"' \
  -c check_for_update_on_startup=false -c feedback.enabled=false \
  -c 'cli_auth_credentials_store="file"' \
  [your arguments]
```

`-c` outranks `config.toml` and a trusted project's `.codex/config.toml`, so neither can turn these back on. Your own `-c` or `--enable` for one of the last seven may set it only to the launcher's value; any other value exits 3. `features.shell_snapshot=false` keeps Codex CLI from writing its environment, the session credential included, into the directory. `--no-daemon` keeps a shared background server from outliving the session with its credential; `codex agents`, which needs that server, does not run through the launcher.

On macOS, configuration that MDM delivers for `com.openai.codex` also outranks `-c`, and the launcher does not check it.
