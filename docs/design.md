# redcoast design

The current design, edited in place and kept short: it says what is true now, not how it got here. Why a decision was made lives in the PR that made it. Only a PR whose purpose is to update the design edits this file; other PRs describe their effect on it under "Design impact".

## 1. What this is, and what it is not

redcoast is a self-hosted gateway for coding-agent subscriptions; today it supports Claude Code. Client machines never log in to a model account and hold no real token: the gateway holds the subscription tokens of one or more accounts and puts one on each request it forwards.

- One Go module, two binaries: `redcoast` on the gateway host (`redcoast claude serve`; the provider is the first word of every command) and `redcoast-client`, the launcher on client machines.
- Two entrypoints shared by all accounts: a reverse proxy that `claude` reaches as `ANTHROPIC_BASE_URL` (inference), and a CONNECT-only forward proxy that it reaches as `HTTPS_PROXY` (everything else the CLI fetches).
- Sessions: before it starts `claude`, the launcher asks for a session credential. The credential authenticates every request for as long as the process lives; the gateway decides which account serves it.
- Quota-aware selection: a session is bound to an account for a lease (1 hour), chosen from the upstream's rate-limit headers against soft and hard thresholds.
- A fixed egress per account: each account has its own upstream HTTP proxy (exit), connection pool and concurrency limit, and all of a session's traffic leaves through its account's exit.
- One SQLite file holds all state; a local unix socket is the management interface; upgrades hand the listeners to a new process without dropping streams; optional hourly backups go to any S3-compatible bucket.
- Account facts come from a directory of YAML files (the inventory). Credentials appear there only as references: `op://` (1Password), `env://` or `file://`.

Not in scope:

- A general LLM proxy. The reverse proxy forwards three request lines, to `https://api.anthropic.com` only (or a loopback origin in tests).
- Other providers, for now. The layout leaves room for one subcommand, process and database per provider; only `claude` exists.
- Switching accounts in the middle of a request, or replaying one. A switch happens between requests; retries are the CLI's.
- TLS interception. The forward proxy only tunnels.
- Failing over to another exit, or sending account traffic direct. When an exit fails, the request fails.
- High availability: one process, one database file, one host.
- Logging accounts in or refreshing tokens. Tokens are long-lived setup tokens; an operator rotates them in the secret store and reloads.
- Remote Control for sessions that go through the gateway (Claude Code turns it off when `ANTHROPIC_BASE_URL` is not the official host).
- Recording conversations. Capture is off by default, redacted by an allowlist and capped.

## 2. Parts and how they connect

```text
 client machine                                 gateway host
 --------------                                 ------------------------------------------------
 redcoast-client --POST /sessions (machine)---> session (TCP or unix) --+
   | starts claude with BASE_URL, OAUTH_TOKEN=session,                  v
   | HTTPS_PROXY=session@forward                             SQLite store: sessions, bindings,
   v                                                         machines, plans, quota, traffic
 claude --POST /v1/messages (Bearer session)-> reverse --+            ^          |
   +-----CONNECT host:port (Basic session)---> forward --+--bind per request     | VACUUM INTO
                                                |        v                       v
                                   direct hosts |   account transport        backup -> S3
                                                v   (token, pool, exit)
                                          public target  --> account exit --> API, targets
 operator --- admin unix socket: reload, handoff, status, sessions, pause, resume, machines
 monitor  --- /health          browser --- /dashboard.json and the dashboard page (read-only)
 inventory dir (YAML, direct-hosts.yaml) + op:// | env:// | file:// --> account set (on reload)
```

`redcoast claude serve --config <file>` reads one YAML file, resolves the inventory's references (all or nothing), opens and checks the store, checks every account's egress, opens or inherits its listeners and tells systemd it is ready. A client machine holds a machine credential whose SHA-256 an operator registered with `machine add`; `redcoast-client claude …` presents it to the session interface over TCP (or nothing, over the local unix socket), gets a session credential bound to its source address, and starts `claude` with the base URL, the session credential as its OAuth token and the forward proxy, all other proxy settings dropped. On each request the gateway checks the credential outside any transaction, then in one write transaction binds the session to an account (keeping, renewing or replacing the lease) and forwards through that account's transport, writing one traffic row. The session ends when `claude` exits (the launcher deletes it), when its machine is revoked, or after 7 days without a request. An egress check (an IP echo through each exit) runs at start, every 15 minutes and on reload, and pauses an account whose exit IP changed. `reload` re-reads the inventory and swaps the account set after migrating affected sessions in one transaction; `handoff` starts the new binary with the listeners inherited and drains the old process for up to 30 minutes. Packages: [`cmd/redcoast`](../cmd/redcoast/) (configuration, commands, listeners, handoff), [`cmd/redcoast-client`](../cmd/redcoast-client/), [`claude`](../claude/) (proxies, accounts, egress, reload, capture), [`session`](../session/) (the store, selection, machines, the session interface), [`credential`](../credential/), [`backup`](../backup/).

## 3. Invariants

Each one links to the test that guards it. Go tests are named after the file and run in the required `check` job; `e2e/xmachine.py` is the Docker rehearsal (fake upstream, fake exit, fake `claude`), which runs in the `e2e` job and does not block a merge.

Credentials and sessions:

1. Session credentials have one shape, are unique, and only their SHA-256 is stored. [`session/store_test.go`](../session/store_test.go): `TestIssueConcurrentUnique`, `TestTokenNotStored`.
2. An unknown, malformed, ended or revoked session credential is refused before anything goes upstream; the client's own credentials are stripped and the account's token put in. [`claude/accounts_test.go`](../claude/accounts_test.go): `TestAccountTokenInjection`; [`session/store_test.go`](../session/store_test.go): `TestRevoke`.
3. A session works only from the address it was issued to: from anywhere else inference gets 401 and CONNECT 407, nothing goes upstream and the session is untouched. [`claude/source_test.go`](../claude/source_test.go): `TestSourceBinding`; [`session/server_test.go`](../session/server_test.go): `TestRevokeSource`, `TestSourceOf`; `e2e/xmachine.py`: `a_session_credential_from_b`.
4. A refused credential is rejected outside the write transaction, and rows for refused requests are capped per source address. [`session/session_test.go`](../session/session_test.go): `TestRefusedCredentialTakesNoWriteLock`; [`claude/route_test.go`](../claude/route_test.go): `TestRefusedRowsAreLimitedPerSource`; [`claude/forward_test.go`](../claude/forward_test.go): `TestForwardRefusedRowsAreLimitedPerSource`.
5. The session interface over TCP needs a registered machine credential; a machine's live sessions are limited in the same transaction that issues one; revoking a machine ends its sessions and refuses its credential from then on. [`session/server_test.go`](../session/server_test.go): `TestServeTCP`, `TestServeUnixOnly`; [`session/machine_test.go`](../session/machine_test.go): `TestMachines`; [`claude/admin_test.go`](../claude/admin_test.go): `TestAdminMachines`; `e2e/xmachine.py`: `a_second_session_over_limit`, `a_session_after_revocation`.
6. The launcher refuses a machine credential file others can read and anything that would override the session credential (environment, settings layers, `--settings`), and clears inherited proxy variables. [`cmd/redcoast-client/main_test.go`](../cmd/redcoast-client/main_test.go): `TestNetworkMode`, `TestPreflight`, `TestChildEnvironment`.
7. Known credentials never reach a capture record in any encoding; session credentials are always fully redacted, and the redaction of a replaced account set survives a reload. [`claude/accounts_test.go`](../claude/accounts_test.go): `TestKnownCredentialsSavedCapture`, `TestKnownCredentialForms`, `TestPartialTokenSavedCapture`, `TestMalformedBearerIsFullyRedactedInSavedCapture`; [`claude/reload_test.go`](../claude/reload_test.go): `TestReloadKeepsOldRedaction`. No credential appears in the gateway log: `e2e/xmachine.py`: `no_credential_in_gateway_log`.

Where traffic may go:

8. The reverse proxy forwards only `POST /v1/messages`, `POST /v1/messages/count_tokens` and `HEAD /api/hello`; anything else gets 403. [`claude/route_test.go`](../claude/route_test.go): `TestRequestAllowlist`.
9. The upstream is `https://api.anthropic.com` or a loopback origin; real tokens go nowhere else. [`cmd/redcoast/main_test.go`](../cmd/redcoast/main_test.go): `TestConfiguration`.
10. The forward proxy accepts only CONNECT to `host:port`; on the account route it refuses non-public IP literals in any notation before authentication. [`claude/forward_test.go`](../claude/forward_test.go): `TestForwardProxy`; [`claude/direct_test.go`](../claude/direct_test.go): `TestAccountRouteRefusesNonPublicLiterals`.
11. Direct hosts are exact names, dialed from the gateway only at the public addresses they resolve to, with no fallback to the other route. [`claude/direct_test.go`](../claude/direct_test.go): `TestReadDirectHosts`, `TestDirectRouteDialsOnlyPublicAddresses`, `TestForwardProxyDirectRoute`.
12. Claude Code's ID headers are kept and forwarded only in their known shapes, so they cannot carry a credential; the traffic table keeps no body. [`claude/route_test.go`](../claude/route_test.go): `TestClaudeIDHeadersCannotCarryCredentials`, `TestClaudeIDShape`, `TestTrafficTableKeepsNoBody`.

Configuration:

13. Listen addresses are distinct IP literals with a port, never a wildcard, and loopback, RFC 1918, 100.64.0.0/10 or IPv6 ULA unless `listen.allow_public` is set. [`cmd/redcoast/main_test.go`](../cmd/redcoast/main_test.go): `TestConfiguration`; [`cmd/redcoast/health_test.go`](../cmd/redcoast/health_test.go): `TestHealthAndEgressConfiguration`.
14. The configuration is strict: unknown keys, a second YAML document or an oversize file are refused, naming the key; the example file loads. [`cmd/redcoast/main_test.go`](../cmd/redcoast/main_test.go): `TestConfiguration`, `TestLoadConfig`, `TestExampleConfig`.
15. Credential references are only `op://`, `env://NAME` and `file:///absolute/path`; `op://` needs a 1Password token, whose own reference is `env://` or `file://`. [`credential/credential_test.go`](../credential/credential_test.go): `TestCheck`, `TestResolve`; [`cmd/redcoast/credentials_test.go`](../cmd/redcoast/credentials_test.go): `TestNewResolverToken`.
16. The inventory loads all or nothing; errors name the alias and field, never a value. [`claude/accounts_test.go`](../claude/accounts_test.go): `TestLoadAccounts`.
17. With TLS, the certificate must pair with its key and cover `server_name`; only TLS 1.2+ and HTTP/1.1 are offered. [`cmd/redcoast/tls_test.go`](../cmd/redcoast/tls_test.go): `TestTLSConfiguration`, `TestListenTLS`; [`claude/tls_test.go`](../claude/tls_test.go): `TestEntrypointsOverTLS`.
18. The capture directory must exist with mode 0700. Untested.

Selection and accounts:

19. At or over the hard threshold a session rebinds at once; over soft only new bindings and renewals are blocked; when every account is out for quota the answer is 429 with `Retry-After`, for any other reason 503. [`session/select_test.go`](../session/select_test.go): `TestHardThresholdRebindsAtOnce`, `TestSoftThresholdKeepsLeaseOnly`, `TestAllOverHard`, `TestExhaustionByPauseReason`; [`claude/select_test.go`](../claude/select_test.go): `TestLocalRateLimitWhenAllOverHard`.
20. Inference is never replayed, and a stream in flight stays on its account when the binding changes. [`claude/proxy_test.go`](../claude/proxy_test.go): `TestInferenceIsNotReplayed`; [`claude/route_test.go`](../claude/route_test.go): `TestInFlightStreamStaysOnAccount`.
21. An upstream 401 pauses the account for an hour only when the token itself was refused; a pause never shortens one in effect. [`claude/route_test.go`](../claude/route_test.go): `TestRefusedTokenPausesForAnHour`, `TestUpstreamUnauthorizedPausesAccount`; [`session/select_test.go`](../session/select_test.go): `TestPauseNeverShortens`.
22. An exit IP that differs from the expected one pauses the account until `resume`; a check that reads no IP pauses nothing and marks health degraded. [`claude/egress_test.go`](../claude/egress_test.go): `TestEgressCheck`, `TestEgressSweep`, `TestReloadWithEgressCheck`.
23. Per-account concurrency and the tunnel limit refuse at once (503) instead of queueing. [`claude/concurrency_test.go`](../claude/concurrency_test.go): `TestAccountConcurrency`, `TestTunnelLimit`.

Operations:

24. A reload either swaps the whole account set after migrating sessions in one transaction, or changes nothing; no binding straddles the swap. [`claude/reload_test.go`](../claude/reload_test.go): `TestReloadWaitsForBindingSection`, `TestReloadMigrationFailure`, `TestReloadValidationFailure`, `TestReloadSerializesReloads`.
25. A handoff whose new process fails or is late leaves the old one serving; otherwise the old one drains streams and tunnels, then exits. [`cmd/redcoast/handoff_test.go`](../cmd/redcoast/handoff_test.go): `TestHandoff`; [`claude/handoff_test.go`](../claude/handoff_test.go): `TestHandoffDrainsInferenceStreams`, `TestHandoffDrainsTunnels`.
26. A store that fails `quick_check` stops startup; the dashboard reads through a connection that cannot write. [`cmd/redcoast/main_test.go`](../cmd/redcoast/main_test.go): `TestOpenStoreRefusesCorruptFile`; [`session/dashboard_test.go`](../session/dashboard_test.go): `TestReaderCannotWrite`.
27. Backups only create objects (`If-None-Match: *`), signed with SigV4; the local snapshot is deleted only after every upload succeeded, and a snapshot restores. [`backup/backup_test.go`](../backup/backup_test.go): `TestSignatureVector`, `TestOnceUploads`, `TestOnceFailureKeepsSnapshot`, `TestRestoreDrill`.
28. A release tag is `v` plus the constant in `cmd/redcoast/version.go`. [`release-tag-guard.test.sh`](../.github/scripts/release-tag-guard.test.sh).

## 4. Interfaces

- Running the gateway, its commands, configuration and inventory format: [`cmd/redcoast/README.md`](../cmd/redcoast/README.md); every configuration key with its default: [`redcoast.example.yaml`](../redcoast.example.yaml).
- The launcher, its environment and the `claude` shims: [`cmd/redcoast-client/README.md`](../cmd/redcoast-client/README.md).
- The session interface (`POST /sessions`, `DELETE /sessions/current`): [`session/server.go`](../session/server.go); the management API: [`claude/admin.go`](../claude/admin.go); `/health`: [`claude/health.go`](../claude/health.go); `/dashboard.json`: [`claude/dashboard.go`](../claude/dashboard.go) and [`dashboard/`](../dashboard/).
- The store's schema and migrations: [`session/migrate.go`](../session/migrate.go).
- The end-to-end rehearsals: [`e2e/README.md`](../e2e/README.md) (sessions across machines, fake `claude`) and [`experiments/README.md`](../experiments/README.md) (the real Claude CLI against a fake API). Releases: [`.github/workflows/release.yml`](../.github/workflows/release.yml), on a `v*` tag.

## 5. Known issues and next steps

- On the account route, host names are resolved by the exit, so the gateway cannot stop a name that resolves to a private address on the exit's side.
- The gateway trusts the TCP peer address. A proxy or TLS terminator in front of it breaks source binding and the per-source caps.
- A public listener without TLS only logs a warning; it should become fatal. The client trusts only the system CAs.
- `/health` and the dashboard have no authentication and rely on network access control. The dashboard shows each account's email and expected exit IP.
- An upstream 401 whose error body is neither plain nor gzip is not recognized as a refused token, so the account is not paused.
- A launcher killed with SIGKILL leaves its session live, holding a machine slot, until the 7-day idle expiry.
- Backups lose up to an hour; sessions issued after the restored snapshot are unknown and their clients must relaunch. Backups pause for up to 30 minutes after a handoff.
- During a handoff's drain the account and tunnel limits apply per process, so they double; reloads are not coordinated across the two processes.
- Schema migrations are one-way; a rollback across one needs a decision by the operator.
- The soft and hard thresholds have no management command, and plans have no read command.
- The egress check calls one fixed IP echo service and expects a top-level `ip` field; it should be configurable.
- IPv6 CONNECT targets are refused by the syntax check.
- The hi rehearsal's `cancel` case sometimes fails its hygiene check because the gateway container is killed at shutdown; CI leaves it out until the cause is known.
- Other providers are not started.
