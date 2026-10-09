# redcoast

redcoast is a self-hosted gateway for coding-agent subscriptions. It keeps the subscription tokens of your accounts, one or several, on a single host. Client machines start the agent's own CLI through a small launcher, and the CLI never sees a real token. Today it supports [Claude Code](https://code.claude.com).

## Why use it

- **The real token never reaches a client.** Each client gets a session credential that is bound to its machine and lasts only as long as its `claude` process. If a prompt injection or a malicious hook dumps the environment or sends it somewhere, what leaks is that session credential; the subscription token stays on the gateway. (Anything running on the client can still use the session while it lasts.)
- **Set up once, use everywhere.** If you work on several machines (a laptop, a workstation, CI runners), you don't log in or set an OAuth token on each of them. You set up the token once on the gateway, on a server or on your own machine, and every client you register with it can use it.

## When it fits, and when it doesn't

It fits when:

- you use Claude Code with a subscription, and have a setup token (`claude setup-token`) for each account;
- you can run the gateway on a Linux host that your clients can reach, or on the same machine as the client;
- your machines run linux/amd64, the only platform releases are built for.

It doesn't fit when:

- you want a general LLM proxy or an API for other apps: it forwards only the requests Claude Code itself makes;
- you need a coding agent other than Claude Code;
- you rely on Remote Control: Claude Code turns it off when its traffic goes through a gateway;
- you need high availability: it is one process with one database file on one host;
- you want the gateway to log accounts in or refresh tokens: you rotate them yourself.

## Terms of use

redcoast does not check which program sends a request. Staying within your subscription's terms is up to you:

- Send requests through the gateway only from the vendor's own CLI (today, `claude`, started by the launcher). Don't point other apps at it.
- A subscription may allow use only through the vendor's own clients. If yours does, going through the gateway doesn't change that, so make sure only that client uses it.
- If your subscription's terms don't allow this kind of use, don't use redcoast.

## Documentation

- [`docs/design.md`](docs/design.md): what it is and is not, how the parts connect, the invariants and the tests that guard them.
- [`cmd/redcoast/README.md`](cmd/redcoast/README.md): installing, configuring and running the gateway.
- [`cmd/redcoast-client/README.md`](cmd/redcoast-client/README.md): the launcher on client machines.
- [`e2e/README.md`](e2e/README.md): the end-to-end rehearsal in Docker.
- [`AGENTS.md`](AGENTS.md): how to work on this repo (layout, checks, releases).

## License

MIT, see [LICENSE](LICENSE).
