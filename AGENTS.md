# Working on redcoast

Everything in this repo is written in English: code, comments, docs, commit messages, PR titles, descriptions and comments.

## What this is

redcoast is a self-hosted gateway for coding-agent subscriptions, today for Claude Code: it holds the subscription accounts, hands out short-lived session credentials to client machines, and forwards each session's traffic through the account and exit it is bound to. [`docs/design.md`](docs/design.md) says what it is and what it is not, how the parts connect, and which tests guard which invariant.

## Layout and docs

- `docs/design.md` is the one design document. It describes the current state only, is edited in place, and stays short. There are no ADRs or other design notes; the reason for a decision goes in its PR.
  - Its fixed sections: what this is and what it is not; the parts and how they connect (one diagram, one paragraph); invariants, each linked to the test that guards it (or marked "untested"); interfaces (links only); known issues and next steps.
  - Only a PR whose purpose is to update the design edits it. Every other PR leaves it alone and fills in "Design impact" in its description (`.github/pull_request_template.md`): the section or invariant it changes and the guarding test, or "none".
- Manuals live next to their code: [`cmd/redcoast/README.md`](cmd/redcoast/README.md) (installing and running the gateway), [`cmd/redcoast-client/README.md`](cmd/redcoast-client/README.md) (the launcher), [`e2e/README.md`](e2e/README.md) (the end-to-end rehearsal), [`experiments/README.md`](experiments/README.md).
- Go packages: `cmd/redcoast` (the gateway binary and its configuration), `cmd/redcoast-client` (the launcher), `claude` (proxies, accounts, egress, reload, capture), `session` (the SQLite store, selection, machines, the session interface), `credential` (reference schemes), `backup` (S3 backups), `internal/testtls` (test certificates). `dashboard/` is the read-only web page.
- The review skill's source is `.agents/skills/redcoast-pr-review/`; `.claude/skills/redcoast-pr-review` links to it for Claude Code.

## Checks

CI (`.github/workflows/ci.yml`) runs two jobs on every pull request and every push to `main`.

`check` is required. Its steps, run from the repository root:

```sh
gofmt -l .                      # must print nothing
go vet ./...
go tool -modfile=tools/golangci-lint.mod golangci-lint run ./...
go test -race ./...
.github/scripts/release-tag-guard.test.sh
.github/scripts/check-tailnet-literals.sh
uv run --no-project --python 3.14 --with pyyaml==6.0.3 python .github/scripts/check-skill-frontmatter.py .agents/skills/*/SKILL.md
(cd experiments && uv run --no-project --python 3.14 --with pytest==9.1.1 --with pyyaml==6.0.3 python -m pytest -q tests)
```

and, in `dashboard/` with the Node and pnpm versions it pins (`.node-version`, `packageManager`): `pnpm install --frozen-lockfile`, `pnpm peers check`, `pnpm run format:check`, `pnpm run lint`, `pnpm run test`, `pnpm run knip`, `pnpm run build`. CI also scans the full history with gitleaks; locally, `gitleaks git --redact .` does the same (always with `--redact`, so a hit does not print the secret).

`e2e` runs the end-to-end rehearsals in Docker: [`e2e/README.md`](e2e/README.md) and the synthetic cases of the hi rehearsal ([`experiments/README.md`](experiments/README.md)). It is not required, so a red run does not block a merge; it does ask for a look:

- Before merging, the maintainer reads the `e2e` result of the head being merged.
- When it is red, find the cause first. Merge only once it is clear the failure has nothing to do with the PR, and say so in the PR: the failing run and the cause.

No CI step uses a credential or reaches a real account: the rehearsals use made-up accounts, a fake upstream and a fake exit, and the hi rehearsal's authenticated mode never runs in CI. Keep it that way. `govulncheck` runs as a third job that is not required. Tool versions are pinned in `tools/*.mod`, `go.mod` and the workflows; bump them in a PR of their own.

## Releases

A release is pushing a `v*` tag, and only the maintainer does it. The tag must be `v` plus the constant in `cmd/redcoast/version.go` (`.github/scripts/release-tag-guard.sh`), so the release PR bumps that constant first. `.github/workflows/release.yml` then publishes static linux/amd64 builds of `redcoast` and `redcoast-client`, the two client shims, the dashboard page (`redcoast-dashboard.html`) and `SHA256SUMS` over all five.
