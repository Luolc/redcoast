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

### The inventory

`inventory` names a directory with one file per account, `<alias>.yaml`, where the alias matches `^[a-z0-9][a-z0-9_-]{0,63}$`. The gateway serves the accounts with `status: active` and `access: gateway`; other keys are ignored, so the directory can hold more facts than the gateway reads. An example with made-up values:

```yaml
email: someone@example.test # optional; shown on the dashboard
access: gateway # gateway or direct
status: active # active, paused, suspended, retired or testing
oauth_token: op://example-vault/example-a/token # a `claude setup-token` token
proxy: # the account's exit, an HTTP proxy
  host: proxy.example.test
  port: 8080
  expected_egress_ip: 192.0.2.10
  username_ref: op://example-vault/example-a-proxy/username
  password_ref: op://example-vault/example-a-proxy/password
```

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
| `redcoast claude machine add <name> <sha256> [--max-sessions n]` | register a client machine's credential hash |
| `redcoast claude machine revoke <name>`, `machine limit <name> <n>`, `machine list` | end a machine's sessions and refuse it, change its limit, list machines |

`redcoast claude plan --session-db <path> …` writes subscription plans straight into the database and does not need the gateway to run.

## Add a client machine

On the client, create the machine credential and print its hash:

```sh
umask 077 && mkdir -p ~/.config/redcoast-client && head -c 32 /dev/urandom | basenc --base64url -w0 >~/.config/redcoast-client/machine-credential && sha256sum ~/.config/redcoast-client/machine-credential | cut -d' ' -f1
```

On the gateway, register the hash with `machine add`, and set `listen.session` so the client can reach the session interface. Then start Claude Code on the client through [`redcoast-client`](../redcoast-client/README.md).

## Backups

With a `backup` section the gateway uploads a gzipped snapshot of its database every hour to `<endpoint>/<bucket>/<prefix>/hourly/`, and the first one of each UTC day also to `daily/`. It only creates objects; expire them with the bucket's lifecycle rules. To restore, stop the service, move the database and its `-wal` and `-shm` files aside, put the gunzipped snapshot at `session_db` and start it again. Sessions issued after the snapshot are unknown; their clients start again.
