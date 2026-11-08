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

`inventory` names a directory with one file per account, `<alias>.yaml`, where the alias matches `^[a-z0-9][a-z0-9_-]{0,63}$`. The gateway serves the accounts with `status: active` and `access: gateway`; start from the directory ignores other keys, but the first import into the `accounts` table (below) refuses a key it has no column for. An example with made-up values:

```yaml
email: someone@example.test # optional; shown on the dashboard
receiver_email: inbox@example.test # optional; the address that receives the account's mail
access: gateway # gateway or direct
status: active # active, paused, suspended, retired or testing
oauth_token: op://example-vault/example-a/token # a `claude setup-token` token
device_id: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef # optional; 64 lower-case hex digits
account_uuid: 01234567-89ab-cdef-0123-456789abcdef # optional; lower-case UUID
proxy: # the account's exit, an HTTP proxy
  provider: example-provider # optional; kept and shown, not used to serve
  type: residential # optional; kept and shown, not used to serve
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

#### The Claude accounts are kept in the database

The Claude gateway reads the directory once. At its first start with an `accounts` table that was never imported, it reads every `<alias>.yaml` (the accounts it does not serve too), refuses the directory as above when it does not load, and otherwise writes all the accounts and a marker (the `accounts_imported` row of `settings`) in one transaction. From then on the table is the source of the account set, at start and on every reload, and the account files in `inventory` are ignored, so editing one has no effect; `direct-hosts.yaml` is still read from that directory. The import refuses, naming the file and the key and never a value, a key that is not one of those below, at the top or under `proxy`, and imports nothing then: the first start fails with that error, and removing the key from the named file (or moving it out of the directory) lets the next start import. The table holds every key of the file as it is (`email`, `receiver_email`, `access`, `status`, `oauth_token`, `device_id`, `account_uuid` and the `proxy` block with `provider`, `type`, `direct`, `host`, `port`, `expected_egress_ip`, `username_ref` and `password_ref`), each credential only as its reference, plus a revision number and the times. The Codex gateway keeps reading its directory.

`redcoast claude inventory export --session-db <path> <dir>` writes the table back as `<alias>.yaml` files of this format into `<dir>` (created when missing; a file of the same name is replaced, other files stay), so the accounts can be archived elsewhere. It reads the database through a read-only connection and can run beside the gateway. Every key the import accepts is stored and exported; YAML comments and key order are not kept.

`/dashboard.json` gives each account's table row as `config` (references as strings, `rev`), and lists the rows of accounts the gateway does not serve under `unserved_accounts`. The store's figures (usage, sessions, destinations and the rest of the snapshot) are computed by a background task, not by a request: it computes one when the gateway starts, then again 60 seconds after each computation ends, and at once after a reload of the served accounts, and a request reads the latest. `at` is when that computation read the store, so the figures are as old as `at`, at most about one computation (several seconds on a large store) plus 60 seconds. A request that arrives before the first computation ends, or after a reload and before the computation it triggers ends, waits for it; a failed computation keeps the last snapshot, and answers 500 only while there is none for the served accounts. The accounts' table rows, setup states and write permissions are read on every request.

#### Editing the accounts from the dashboard

The Claude gateway's dashboard can create, edit, verify and activate accounts. This replaces the pull request review that an inventory change went through; what stands in its place is below. The Codex gateway answers 501 on these routes and `accounts_editable: false` in `/dashboard.json`.

Writes are off until `dashboard_write.allow_from` lists the TCP peer addresses they may come from (IP addresses or prefixes, never `/0`; see `redcoast.example.yaml`). The check reads the address the connection comes from and nothing in a header, so behind a reverse proxy every request shows the proxy's address. `dashboard_write.token_ref_template` is the token reference of a new account, with `{alias}` in place of its alias; without it no account can be created. A request is accepted only when all of this holds, and refused with 403 (415, 413 or 400 from the body checks) before anything is read or written otherwise:

- its peer address is on `allow_from` (an empty list answers `writes disabled`);
- its `Host` is a name the dashboard answers, and it carries exactly one `Origin`, the page's own;
- it carries exactly one `X-Redcoast-Action` with the route's action name: `account-create`, `account-update`, `account-verify` or `account-activate`;
- its `Content-Type` is `application/json` and its body at most 8 KiB: one JSON object with no unknown key and no key repeated at any depth, and nothing after it.

| Route | What it does |
| --- | --- |
| `POST /accounts` | create an account as `testing`: `alias`, `email`, optional `receiver_email` (one bare address, no display name), `access`, `device_id` (64 lower-case hex digits, different from every other account's; an edit of it is checked the same way), `plan` and `proxy` (optional `provider` and `type`; `host`, `port`, `expected_egress_ip`, or `direct: true` with the IP only; `username_ref` and `password_ref` as one pair that accounts use already). The token reference is the template with the alias. It serves nothing yet. The plan is written after the account, so a failure of that step says the account exists |
| `PATCH /accounts/{alias}` | change `email`, `receiver_email`, `status` (anything but `active`), `device_id`, `account_uuid`, `proxy` fields or `plan`, at the `rev` the page read; another revision answers 409. The plan goes through the plan schedule as `redcoast claude plan schedule` does, and a raised quota unit reads the quota of a served account once. A change that can alter the served set reloads, after the references of the changed account were resolved without writing; a change of the exit, the expected IP or a reference voids the checks below |
| `POST /accounts/{alias}/verify` | run three checks on the account in memory, while no other write runs: `token_ok` (its token reference resolves to a usable token), `egress_ok` (its own exit reads the expected IP and reaches the upstream) and `probe_ok` (the quota probe of `refresh` gets a 2xx through that exit, with the account's own token; it waits for a quota round in progress). Each result and the time are stored, never a value |
| `POST /accounts/{alias}/activate` | at the `rev` given: the account has a plan, no other served account has its exit, and the three checks, run again now, pass; only then it is stored as `active` and the set reloaded. Anything else is a 409 naming the check |

An answer is `{account, reload?, verify?}` or `{error, field?, check?}`; an error never repeats what the request held. Every write that lands is one row of `account_audit` (time, source address, action, alias, and the old and new value of each changed field, references and settings only) and one log line with the field names; a refusal of a source on the list is logged with a fixed reason, one of any other source is not logged. `/dashboard.json` says `write: {enabled, reason, can_create}` for the request's source, `accounts_editable` and `proxy_ref_options`, the proxy logins in use, as reference strings. No route has a field for a credential value, and the token of an account is changed in the secret store, followed by `verify`.

#### Setting an account's token

An account's token is a long-lived setup token that the gateway never refreshes. With `setup.enabled: true` the gateway can also take a new one from a login machine, a separate host that logs in on its own side and runs `claude setup-token`, and put it in the secret store. Setup needs `credentials.onepassword_token` and `dashboard_write.token_ref_template`, and the 1Password service account must be allowed to write to the vault the template names (the same credential the references are read with; it is not another one). While `setup.enabled` is false, every route below answers 501 `setup_unavailable`, `/dashboard.json` has no `setup` blocks and `gateway.setup` is null; the Codex gateway refuses the key.

The page asks for a task with `POST /accounts/{alias}/setup`, under the checks of a write above with the action name `account-setup` and the body `{"action": "login_and_token", "rev": <rev>}` (any other action is a 400 on the field `action`). Only an account that is `testing` or `active` and whose exit is not `direct` can be set up. It answers 202 with `{"setup": <state block>}`, or 409 `setup_open` (the account has a `requested` or `running` task), `rev_stale`, `account_status` (the account is neither `testing` nor `active`) or `direct_account` (its exit is direct; checked after the status). No dashboard route takes a token or a report.

The login machine reaches the management socket, over an ssh key restricted to these three commands, once a minute:

| Command | Route | What it does |
| --- | --- | --- |
| `redcoast claude setup next [--request-id <id>]` | `POST /setup/next[?request_id=<id>]` | prints `{"task": null}`, or one task, which becomes `running`. Without `--request-id` it is the next task the dashboard asked for, never one from Slack. With it, it is that one task, which must be the account's latest, from Slack and waiting; `{"task": null}` also when another task is running (try again). The task has `alias`, `action`, `request_id`, `requested_at`, `status` (`testing` or `active`), `source`, `email`, `receiver_email` (always present, an empty string when the account has none), `device_id` and `proxy` (`host`, `port`, `username_ref`, `password_ref`, `expected_egress_ip`, `direct`; references, never values) |
| `redcoast claude setup report <alias> <request-id> --run-id <id> --source-kind <dashboard\|slack> [--wait-until <RFC 3339>] --state <running\|needs_human\|failed> --step <code> [--reason <code>]` | `POST /accounts/{alias}/setup/report` | records one step. The first report sets the `run_id` (`YYYYMMDDTHHMMSSmmmZ`), which every later report and the handover must carry. `running` takes no reason; `needs_human` only `captcha_challenge`, `unknown_page` or `email_rejected`; `failed` any other reason of the fixed set. `done` is the gateway's alone |
| `redcoast claude setup token <alias> <request-id> --run-id <id> --source-kind <dashboard\|slack>` | `POST /accounts/{alias}/setup/token?request_id=…&run_id=…&source_kind=…` | reads the token from stdin (one line, at most 4 KiB, one line break at the end dropped) and sends it as the raw request body; it prints the gateway's answer, which never holds the token |

A refusal exits 1 and prints `<route> refused: <code>`. The codes are: `setup_unavailable` (501); `bad_request` (400, also a body over 4 KiB, an unknown or repeated key, or an ID of the wrong shape); `unknown_account` (404); `state_invalid`, `step_invalid`, `reason_invalid` (400); `no_open_task` (409: the account has no `running` task that no handover has claimed); `ids_mismatch` (409: `request_id` or `run_id` is not the task's, or no report has set the run ID yet). A refusal by these writes and claims nothing.

A handover is taken once. After the checks above and a token of the shape `sk-ant-oat01-` and `[A-Za-z0-9_-]` only (422 `token_shape_invalid`, which claims nothing), the task is claimed. The gateway then: refuses a token reference that is not what the template gives for the alias, so that nothing else is written or created (`write_target_not_allowed`); sends one quota request with the token through the account's own exit (`probe_refused` for 401 and 403, `probe_failed` for anything else but 2xx); writes the token to the secret store, setting the one field the reference names of the existing item and, when the item has one, its expiry field `expires`, to the UTC date of the write plus one calendar year (an item without an expiry field gets the token only, no field is added, and the page reads `no_field`), or, for a missing item, creating it with that one field and an expiry field (`write_target_missing`, `write_target_ambiguous`, `write_failed`; an item with two expiry fields is ambiguous). After a create the gateway reads the item's fields back; one with a field besides the token's and one expiry field ends the task as `failed` with `created_item_has_other_fields` and `written: true`, the item is left as it is, and the log names the fields, never a value. The expiry field is an API Credential item's own; its ID, type and value format are not yet checked against a real vault: the gateway matches it by its title `expires` ignoring case, not by ID, creates a date field with that ID and title and a value `YYYY-MM-DD`, and reads that or RFC 3339; resolves the reference again to check the value (`readback_mismatch`); and reloads (`reload_failed`). A failure ends the task as `failed` with that reason, drops the token and answers `{"error": <reason>, "written": <bool>}` with status 422, 500 or 502; only `readback_mismatch` and `reload_failed` come after the write, and leave the new token in the secret store while the old one is served until the next reload. Success answers `{"setup": …, "probe": "ok", "written": true, "reload": "ok"}`.

At most one task runs at a time; the others wait as `requested`, each in the order requested. A `running` task fails with `timeout` after 30 minutes without a report or 60 minutes after it was handed out, and a claimed one 5 minutes after the claim; a task claimed when the process starts ends as `failed` with `interrupted`; a `requested` task, and a `running` one that no handover has claimed, ends as `failed` with `account_status` if its account stops being `testing` or `active`, or with `direct_account` if its exit becomes direct; a claimed handover is not cut off by such a change. The gateway renews nothing by itself: a task exists only because the page's button asked for it.

`/dashboard.json` gives each account `setup: {state, request_id, run_id, step, reason, updated_at, token_set_at, action, identity, token_expiry}` (`token_set_at` is when the gateway wrote the token, null for a token it did not write; `identity` is null: the gateway cannot yet tell that a token is the account's own). `token_expiry: {state, expires_at, read_at}` is the expiry date of the secret store item at the account's token reference, hand-placed items included, read at start, once an hour, after a reload that a page write causes and after each handover's write; `state` is `read`, `no_field` (the item has no expiry date), `unreadable` (the item, the vault or the date could not be read) or `unavailable` (not read yet). A failed read changes nothing else, and no token is read. `source` is `{kind, thread?, ts?}` (`dashboard`, or `slack` with the thread key and ts of the message) and `wait_until` is when a login program stops waiting for a person at a captcha page, null otherwise. `gateway.setup: {enabled, queued, last_asked_at}`. `step` and `reason` are each one of a fixed set of codes, so they cannot carry a token. The token is in no log line, audit row, answer, table, backup or capture record; the log lines of a setup name the alias, the request ID, the step and the reason.

#### Adding an account from the management socket

With `admin_write.enabled: true` (off by default; it needs `dashboard_write.token_ref_template`, and the Codex gateway refuses the key) an agent can add an account through the management socket with the same checks and code as the page. Every writing command takes the Slack message it came from, `--source-thread <CHANNEL/TS>` and `--source-ts <TS>`, both required; the gateway records them in the audit and on the setup task, and nothing more: they are no proof that anyone agreed. With the key off, every writing command answers 501 `account_write_unavailable`; `account show` only reads and still answers.

| Command | Route | What it does |
| --- | --- | --- |
| `redcoast claude account create <alias> --email … --device-id … [--receiver-email …] --plan … --proxy-host … --proxy-port … --expected-egress-ip … --proxy-username-ref … --proxy-password-ref … [--proxy-provider …] [--proxy-type …]` | `POST /accounts` | the page's create: a `testing` account on the gateway path behind that proxy (no direct account; the proxy login references must be ones an account already uses), the token reference from the template, the plan written. Prints `{"account": …}` with the `rev` the next steps need |
| `redcoast claude account show <alias>` | `GET /accounts/{alias}` | prints `{account, served, setup, created, history, history_truncated}`: the settings (references, never values), whether this process serves it, the setup block (null with setup off), the audit row of the creation and the last 100 audit rows with their sources |
| `redcoast claude setup request <alias> --rev <n>` | `POST /accounts/{alias}/setup/request` | queues a `login_and_token` task from Slack (needs `setup.enabled` as well; `account_write_unavailable` is checked before `setup_unavailable`); prints `{"setup": …}` with the `request_id` |
| `redcoast claude account set <alias> --rev <n> [--receiver-email …] [--device-id …] [--proxy-provider …] [--proxy-type …]` | `PATCH /accounts/{alias}` | the page's edit, limited to these fields (the receiver email, the device ID, the proxy's provider and type), at the `rev` given (another revision is 409 `rev_stale`), through the same checks and code; at least one flag is required, a flag given with an empty value clears the field and a flag left out leaves it, except that `--device-id` takes 64 lower-case hex digits, differing from every other account's, and only for a `testing` account (any other status is 409 `not_ready` with `check: status`). Nothing else of an account is edited through the socket. Prints `{"account": …}` |
| `redcoast claude account verify <alias> [--rev <n>]` | `POST /accounts/{alias}/verify` | the page's verify |
| `redcoast claude account activate <alias> --rev <n>` | `POST /accounts/{alias}/activate` | the page's activate, but only for a `testing` account: any other status is 409 `not_ready` with `check: status` |

An agent takes its own task with `setup next --request-id <id>` and goes on with `setup report` and `setup token`, which carry `--source-kind slack`; a report or a token whose `--source-kind` is not the task's is refused with 409 `source_mismatch` and writes and claims nothing, which keeps the login machine at the desk and an agent off each other's tasks when the wrapper in front of the socket fixes the value. A login program at a captcha page reports `--state running --step awaiting_captcha --wait-until <time>`, at most 20 minutes ahead, once per task (400 `wait_until_invalid` otherwise, and writes nothing); `/dashboard.json` carries the time, and the account's detail page shows it while the task waits at that step, and the program reports `needs_human` itself when it passes.

A refusal exits 1 and prints `<route> refused: <code>`, with ` (field: …)`, ` (check: …)` and, when the answer has `written` (the token handover), ` (written: true|false)` after it, in that order; scripts read the word after `refused: `. The codes, besides those of the setup commands above: `source_missing`, `source_invalid` (400, `field: source.thread` or `source.ts`), `bad_request`, `field_missing`, `field_invalid` (400, with the `field`), `unknown_account` (404), `rev_stale` (409), `account_exists` (409), `no_token_template` (409), `not_ready` (409, `check` one of `status`, `access`, `plan`, `exit`, `token_ok`, `egress_ok`, `probe_ok`), `plan_not_set` (500: the account exists, its plan was not written), `describe_failed` (500), `resolver_unavailable` (502), `reload_failed` (502: the account is stored, the served set unchanged), `store_unavailable` (500) and `account_write_unavailable` (501).

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
| `redcoast claude reload` | re-read the accounts (the table; the Codex gateway's directory); all or nothing |
| `redcoast claude inventory export --session-db <path> <dir>` | write the accounts table as inventory files into `<dir>` |
| `redcoast claude setup next`, `setup report …`, `setup token …` | the commands of a login machine, when `setup.enabled` is on (see "Setting an account's token") |
| `redcoast claude account create\|show\|verify\|activate`, `setup request`, `setup next --request-id` | an agent adds an account from a Slack message (see "Adding an account from the management socket") |
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
