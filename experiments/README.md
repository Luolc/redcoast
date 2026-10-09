# Experiments

`hi.py` rehearses the gateway end to end in Docker with the real Claude Code CLI, one case at a time. A gateway container runs `redcoast` with `hi_gateway.py` around it, and client containers run the CLI through `hi_client.py`; the host captures the gateway namespace's packets, judges the run's hygiene and outcome, and writes `result.json` into a new directory under `--out-root`. `python3 hi.py --help` lists the cases and their flags.

## Synthetic runs

A synthetic run uses no credential and reaches no account: the gateway gets made-up account tokens and talks to a fake API through a fake exit, both inside the gateway container. The accounts come from an inventory directory; [`fixtures/inventory`](fixtures/inventory) has two made-up ones. CI's `e2e` job runs the cases in [`ci-cases.sh`](ci-cases.sh), which also downloads the Claude CLI at the version it pins and checks its SHA-256:

```sh
ci-cases.sh <static redcoast> <static redcoast-client> <new output directory>
```

It needs Docker, passwordless `sudo` for `tcpdump`, `uv` and `curl`. One case by hand, with Python 3.14 and PyYAML:

```sh
python3 hi.py --gateway-binary <redcoast> --cli-binary <claude> \
  --inventory fixtures/inventory --account example-a --out-root <dir> --case hi
```

The `cancel` case sometimes ends with the gateway container killed at shutdown and fails its hygiene check, so CI does not run it yet.

## Authenticated runs

`--real-once` resolves the account's references with the 1Password SDK (`onepassword-sdk`) and sends real requests through the account's real exit. It needs `--token-vault` and `--proxy-vault`: an account file whose references name any other vault is refused before anything is resolved. These runs never happen in CI, and need `--pins` (a `sha256sum` file of the three programs and the binaries). The `quota` case also needs agent-toolbox (`--atb-binary`).

## Tests

```sh
cd experiments && uv run --no-project --python 3.14 --with pytest==9.1.1 --with pyyaml==6.0.3 python -m pytest -q tests
```
