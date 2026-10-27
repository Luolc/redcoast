# redcoast: installing and running the gateway

`redcoast` is the gateway. It runs on one Linux host as a systemd service; how it works is in [`docs/design.md`](../../docs/design.md).

## Install

Download `redcoast-linux-amd64`, `redcoast-dashboard.html` and `SHA256SUMS` from a release, check the files and put them where the service reads them:

```sh
sha256sum -c --ignore-missing SHA256SUMS
sudo install -D -m 0755 redcoast-linux-amd64 /opt/redcoast/releases/<version>/redcoast
sudo install -D -m 0644 redcoast-dashboard.html /opt/redcoast/releases/<version>/dashboard/index.html
sudo ln -sfn /opt/redcoast/releases/<version> /opt/redcoast/current
```

Or build it from source with the Go version in `go.mod`: `CGO_ENABLED=0 go build -trimpath -o redcoast ./cmd/redcoast`. The release build adds `-buildvcs=false` and is reproducible.

The page is the dashboard's frontend; set `dashboard_dir: /opt/redcoast/current/dashboard` to serve it, or the gateway serves only `/dashboard.json`. To build it from source, run `pnpm install --frozen-lockfile && pnpm run build` in `dashboard/` with the Node and pnpm versions it pins; the page is `dashboard/dist/index.html`.

A handoff (below) starts the program at the path the service started, so an upgrade installs the new release, moves the `current` link and hands off.

## Configure

The gateway reads one YAML file, given by `--config`. [`redcoast.example.yaml`](../../redcoast.example.yaml) lists every key with its default; unknown keys are refused. Credentials appear in it, and in the inventory, only as references:

- `op://vault/item/field`: a 1Password item, resolved with a service account whose token is itself an `env://` or `file://` reference (`credentials.onepassword_token`);
- `env://NAME`: an environment variable of the service;
- `file:///absolute/path`: a file, for example a systemd credential under `/run/credentials/`.

Listen addresses are IP literals. Loopback and private addresses (RFC 1918, 100.64.0.0/10, IPv6 ULA) are accepted; a public one needs `listen.allow_public: true` and, in practice, `listen.tls`.

`/health` and the dashboard answer only requests whose `Host` names the gateway: the IP of `listen.health` or `listen.dashboard`, `listen.tls.server_name`, or a name in `listen.host_names`, at any port. A request with any other `Host`, or with an `Origin` other than the page's own, gets 403. This stops a web page from reading them through a name of its own that resolves to the gateway (DNS rebinding). When people or monitors reach the gateway by a host name, such as its name on a private network, add that name to `listen.host_names`; otherwise they get 403 while the IP still works.

The Claude dashboard's "Refresh all quota" button, next to the accounts' quota, has the gateway refresh every account as `redcoast claude refresh --all` does, in the background: it posts to `POST /quota/refresh`, which answers 202 at once, joins a round that is already waiting or running, and refuses with 429 and `Retry-After` for 5 minutes after the last such round ended. The POST needs the page's own `Origin` and the header `X-Redcoast-Action: refresh`, so a page of another origin cannot send it; there is no other authentication, so anyone who can reach the dashboard can start a round, which sends one request with each account's token through its exit. Each account's outcome goes to the log (`dashboard quota refresh alias=…`), and `gateway.quota_refresh` in `/dashboard.json` gives the round's state. The Codex dashboard has no button, and its `POST /quota/refresh` answers 501.

### The inventory

`inventory` names a directory with one file per account, `<alias>.yaml`, where the alias matches `^[a-z0-9][a-z0-9_-]{0,63}$`. The gateway serves the accounts with `status: active` and `access: gateway`; other keys are ignored, so the directory can hold more facts than the gateway reads. An example with made-up values:

```yaml
email: someone@example.test # optional; shown on the dashboard
access: gateway # gateway or direct
status: active # active, paused, suspended, retired or testing
oauth_token: op://example-vault/example-a/token # a `claude setup-token` token
device_id: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef # optional; 64 lower-case hex digits
account_uuid: 01234567-89ab-cdef-0123-456789abcdef # optional; lower-case UUID
proxy: # the account's exit, an HTTP proxy
  host: proxy.example.test
  port: 8080
  expected_egress_ip: 192.0.2.10
  username_ref: op://example-vault/example-a-proxy/username
  password_ref: op://example-vault/example-a-proxy/password
```

At most one account of an inventory may have a direct exit instead, the gateway host's own address:

```yaml
proxy:
  direct: true
  expected_egress_ip: 203.0.113.7 # the gateway host's own public address
```

With `direct: true`, `host`, `port`, `username_ref` and `password_ref` must be left out; a missing `proxy` is an error, not a direct exit. All of the account's traffic leaves from the gateway, in the address family of `expected_egress_ip` only, so on a host with both an IPv4 and an IPv6 address, name the one the upstream should see. Its tunnels on the forward proxy reach only port 443 of public addresses. The egress check reads the IP echo from the gateway and, instead of a CONNECT through an exit, dials the upstream: a failed dial marks health degraded and never sets the account aside.

`device_id` and `account_uuid` are the identity the gateway puts into `metadata.user_id` of the account's requests in place of the client's; a missing or empty one is sent as an empty string. A malformed one is refused, naming the account and the field, never the value. Changing either counts as changing the account on a reload.

All of an active account's references must resolve, or the gateway does not start (and a reload changes nothing). An optional `direct-hosts.yaml` in the same directory lists the exact `host:port` CONNECT targets the gateway dials itself instead of through an account's exit:

```yaml
hosts:
  - downloads.example.test:443
```

Subscription plans live in the database, not the inventory. Every served account needs a plan in effect, set with `redcoast claude plan` (see `redcoast claude plan` without arguments for its usage).

## Run under systemd

An example unit, `/etc/systemd/system/redcoast.service`, for a service user `redcoast`:

```ini
[Unit]
Description=redcoast gateway
Wants=network-online.target
After=network-online.target

[Service]
# READY=1 comes after the accounts are loaded, the store is opened and the
# egress checks ran. After a handoff the new process sends MAINPID itself.
Type=notify
NotifyAccess=all
TimeoutStartSec=240
User=redcoast
UMask=0077
StateDirectory=redcoast
StateDirectoryMode=0700
RuntimeDirectory=redcoast
RuntimeDirectoryMode=0700
LoadCredential=op-sa-token:/etc/redcoast/op-sa-token
ExecStart=/opt/redcoast/current/redcoast claude serve --config /etc/redcoast/claude.yaml
ExecReload=/opt/redcoast/current/redcoast claude reload --admin-socket /run/redcoast/admin.sock
Restart=on-failure
RestartSec=10s
LimitNOFILE=65536
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

With the example configuration the sockets are under `/run/redcoast/` and the database under `/var/lib/redcoast/`. Start it with `sudo systemctl enable --now redcoast`; `curl -s http://127.0.0.1:7804/health` answers `{"status":"ok",...}` once it is ready.

## Operate

Every command below talks to the running gateway over `--admin-socket` (the `admin_socket` of the configuration, mode 0600) and prints its answer; a refusal exits 1 with the gateway's error. A command without its arguments prints its usage.

| Command | What it does |
| --- | --- |
| `redcoast claude status` | accounts, pauses, quota readings, sessions |
| `redcoast claude sessions [--since] [--until] [--cwd-prefix] [--machine] [--state ended\|all]` | the session list |
| `redcoast claude reload` | re-read the inventory; all or nothing |
| `redcoast claude handoff` | start the program at the service's path with the listeners inherited; the old process drains for up to 30 minutes, and keeps serving if the new one fails |
| `redcoast claude pause <alias> [--reason] [--until]`, `resume <alias>` | take an account out of selection, or put it back (also after an exit IP mismatch) |
| `redcoast claude refresh <alias>` | read the account's quota again: one request through its exit (`/v1/messages`, one output token of the small model), its readings saved; a pause after an upstream 429 ends when they are under the hard threshold; an account paused for any other reason is refused (409) and nothing is sent. A non-2xx `upstream_status` with no readings means the upstream refused the request itself, for example a retired model |
| `redcoast claude refresh --all` | refresh every account of the current set, one after another, under the same rules as `refresh <alias>`, once a dashboard round or the five-minute recheck in progress has ended; prints one JSON line per account: the outcome, `skipped` with the pause of an account that was not sent anything, or `error`. A failed account does not stop the others. It exits 1 when any account failed; skipped accounts alone exit 0. The command waits up to about ten minutes, enough for twenty accounts that each take the gateway's 30-second bound |
| `redcoast claude machine add <name> <sha256> [--max-sessions n]` | register a client machine's credential hash |
| `redcoast claude machine revoke <name>`, `machine limit <name> <n>`, `machine list` | end a machine's sessions and refuse it, change its limit, list machines |

`redcoast claude plan --session-db <path> …` writes subscription plans straight into the database and does not need the gateway to run. The gateway reads the quota from upstream responses, and every five minutes refreshes, as `refresh <alias>` does, each account with a reading at or over the soft threshold (or rejected) whose reset has passed, since such an account gets little or no traffic that would read it again. Until the window resets, an account over its hard threshold still keeps its old reading, even after its plan was upgraded upstream. With `--admin-socket <path>`, `plan` therefore has the gateway refresh each account whose quota unit is higher now than before the change (a new or bigger plan, or a bigger unit of its plan), once; a unit that stays or goes down, or a period that starts later, sends nothing. Without the socket it prints which accounts to refresh. Upgrade upstream first: the refresh reads what the upstream reports, not what the database says.

An account's plan, and a plan's quota unit, is a step function of time: each value holds from its start until the next one. `plan schedule <alias> <plan> [<from>] [--note <text>]` puts the account on `<plan>` from `<from>` on, and `plan unit <plan> <quota-unit> <monthly-usd> [<from>]` sets the plan's unit from `<from>` on; `<from>` is an RFC 3339 time, past or future, and without it the change starts at the moment the command writes. Each is one transaction: the period in effect at `<from>` ends there (one that starts exactly then is replaced), the periods that start later are deleted, and the new value holds from `<from>` on, open-ended. Periods that ended before `<from>` stay as they are, since quota readings and costs are read against them. A reader of the database, the running gateway included, sees either the old schedule or the new one, so there is no moment between the two writes. The command is refused, exits 1 and changes nothing when the change would leave any account without a plan with a quota unit at a time it had one, or when `schedule` names a plan that lacks a quota unit at some time from `<from>` on. `schedule` takes no end time. `plan end <alias> [<at>]` takes the account off its plan from `<at>` (or now) on, the period in effect then ending there and the later ones deleted; it is the only way to leave an account without a plan, and is refused when the account has nothing in effect at or after that time. `redcoast codex plan` takes the same arguments; a Codex gateway cannot refresh, so with `--admin-socket` a raised account gets the line `<alias> not refreshed: this gateway cannot refresh` and the command still exits 0.

`redcoast claude thresholds --session-db <path> <soft> <hard>` writes the soft and hard quota thresholds straight into the database, both in one transaction, and does not need the gateway to run. Both are fractions in [0, 1] with soft not above hard (for example `0.9 0.95`); a pair outside these rules is refused, exits 1 and leaves the stored pair as it was. The gateway reads the thresholds on every request, so the next one uses the new pair; `status` shows it.

## Add a client machine

On the client, create the machine credential and print its hash:

```sh
umask 077 && mkdir -p ~/.config/redcoast-client && head -c 32 /dev/urandom | basenc --base64url -w0 >~/.config/redcoast-client/machine-credential && sha256sum ~/.config/redcoast-client/machine-credential | cut -d' ' -f1
```

On the gateway, register the hash with `machine add`, and set `listen.session` so the client can reach the session interface. Then start Claude Code on the client through [`redcoast-client`](../redcoast-client/README.md).

## The Codex gateway

`redcoast codex serve --config <path>` runs the gateway for ChatGPT subscriptions, which Pi and the Codex CLI reach through `redcoast-client`. It is a second process, with its own configuration file, listeners, sockets, database and inventory; the Claude gateway and its sessions are untouched, and a session credential of one gateway is refused by the other. Every command above exists as `redcoast codex …` against the Codex gateway's sockets and database, except `refresh`, which it refuses; register client machines with it separately (the same machine credential may be registered with both).

The configuration takes the keys of [`redcoast.example.yaml`](../../redcoast.example.yaml); [`redcoast-codex.example.yaml`](../../redcoast-codex.example.yaml) shows what differs: the upstream is the Codex API's base URL `https://chatgpt.com/backend-api/codex` (or a loopback URL in tests), the default listen addresses are other ports, `capture_dir` is refused, and a `codex` section is required:

- `codex.accounts_dir`: an absolute path to an existing directory, mode 0700, with one directory per account alias, mode 0700, holding the account's ChatGPT login as `auth.json` in `$CODEX_HOME`'s format, mode 0600, beside a `lock` file. The logins stay outside the database, and so outside its backups: the directory may not be `session_db`'s directory, nor be in its `backup` directory or in `dashboard_dir`.
- `codex.session_key`: the reference of a key of at least 32 bytes that signs session credentials. They are JWTs carrying a made-up `chatgpt_account_id`, which Pi requires; changing the key ends every session issued under the old one.

An account file of the Codex inventory has the keys of a Claude one without `oauth_token`, `email` is required, and `installation_id` is a lower-case UUID v4 that the account's requests carry in place of the client's:

```yaml
email: someone@example.test # the ChatGPT login's email
access: gateway
status: active
installation_id: 0b8c7e2a-3f41-4d2e-9a6b-5c1d2e3f4a5b
proxy:
  host: proxy.example.test
  port: 8081
  expected_egress_ip: 192.0.2.11
  username_ref: op://example-vault/example-b-proxy/username
  password_ref: op://example-vault/example-b-proxy/password
```

The gateway forwards only `POST /backend-api/codex/responses`, answers a WebSocket upgrade with 426 so that the clients stream over HTTP, and reads each account's quota from the `x-codex-primary-*` and `x-codex-secondary-*` headers of its responses.

An account without a login loads, its requests get 502, and within a minute it is paused with the reason `needs_reimport`, which `status` and `/health` show. To give it a login, log in with the Codex CLI on a machine where the account's browser session is (`codex login`), then copy that `auth.json` to the gateway host and import it:

```sh
redcoast codex import sample-a --admin-socket /run/redcoast-codex/admin.sock < auth.json
```

The import is refused unless the `email` of the login's `id_token` is the inventory's `email` (ignoring case), its `tokens.account_id` is the `id_token`'s ChatGPT account ID, and, when the account already has a login, that account ID is the stored one's; to put another ChatGPT account under the alias, delete the account's directory first. The gateway then renews the login at once, so the copy it came from stops working: delete it. The command fails with the reason when the renewal is refused (the account stays paused for a new import) and when it failed otherwise (the login is kept and the renewal retried).

The gateway renews each login through the account's exit, as the Codex CLI does: within five minutes of the access token's expiry, or, when that cannot be read, eight days after the last renewal. An upstream 401 pauses the account (`upstream_401`) until a renewal succeeds; one is queued at once and retried after a minute, then after twice as long each time, up to an hour. When the token endpoint refuses the refresh token, or the renewed login fails the checks above, the account is paused for `needs_reimport` until a new import. A process holds an account's `lock` while it renews, and reads `auth.json` again under it, so the two processes of a handoff never renew one account at once; a process that starts takes up the renewals of the accounts left paused for `upstream_401`. When a renewed login cannot be written, the gateway keeps serving it from memory, `/health` reports `<alias>: renewed login not saved`, and the write is retried every minute; fix the directory before the gateway stops, or import the login again afterwards, since the refresh token on disk is spent.

## Backups

With a `backup` section the gateway uploads a gzipped snapshot of its database every hour to `<endpoint>/<bucket>/<prefix>/hourly/`, and the first one of each UTC day also to `daily/`. It only creates objects; expire them with the bucket's lifecycle rules. To restore, stop the service, move the database and its `-wal` and `-shm` files aside, put the gunzipped snapshot at `session_db` and start it again. Sessions issued after the snapshot are unknown; their clients start again.
