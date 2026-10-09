---
name: redcoast-pr-review
description: What a review in the redcoast repo checks on top of a general PR review: the tailnet address range, security-sensitive code and the design document. Use when reviewing any PR here, and as the author before asking for review.
---

# redcoast PR review

## Tailnet addresses

Inside 100.64.0.0/10, only the prefix itself and its first address 100.64.0.1 may appear, as synthetic boundary values for listen and destination policy tests; any other address in this range is P0. `git grep -nE '100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.'` lists every such literal; everything it prints must be `100.64.0.0/10` or `100.64.0.1`.

## Security-sensitive code

A change that touches any of the following starts at P0:

- authentication
- session credentials
- binding a session to its source address
- the upstream path allowlist
- destination checks in the forward proxy
- the listen address policy
- credential resolution

## Design document

- A change that breaks an invariant listed in `docs/design.md` while its "Design impact" says "none" is P0.
- Every invariant `docs/design.md` lists links to a test that exists; a renamed or removed test leaves a dead link, which is a finding.
