"""Rehearse the Claude gateway end to end in Docker, one Hi at a time.

A gateway container runs the gateway binary with hi_gateway.py around it
(fake API and fake exit in synthetic runs); one or more client containers in
its network namespace run the Claude CLI through hi_client.py. The host
captures the namespace's packets, resolves credentials for authenticated
runs (only this process reads the service account), judges hygiene and the
case's outcome, and writes result.json into a new directory under
--out-root. Synthetic runs need no credential and reach no account; the
inventory only supplies the account's proxy port and names. Run with
``python3 hi.py --gateway-binary <static redcoast> --cli-binary <claude>
--inventory <dir> --account <id> --out-root <dir>``; the cases and their
flags are listed by ``--help``.
"""

import argparse
import asyncio
from dataclasses import dataclass, field
import datetime
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import select
import signal
import struct
import subprocess
import sys
import time
from typing import Any, IO
import uuid

import yaml

# Pinned by digest in a form docker can pull; an image ID alone only runs
# where the image is already present.
IMAGE = (
  "python:3.13-slim"
  "@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285"
)
HERE = Path(__file__).resolve().parent
GATEWAY_RUNNER = HERE / "hi_gateway.py"
CLIENT_RUNNER = HERE / "hi_client.py"
# Messages POSTs each case is expected to make; the gateway itself no longer
# limits them. restart is one Hi, a Hi from a new client, then a Hi after the
# gateway restarts; egress-forward reads the exit IP through the forward proxy.
INFERENCES = {
  "hi": 1, "turns": 3, "stream": 1, "tool": 2, "cancel": 1, "tui": 1,
  "egress": 0, "egress-forward": 0, "long": 1, "restart": 3, "quota": 5,
  "limits": 2, "cache": 2,
}  # fmt: skip
# limits arms: what the fake upstream does on the session's second turn, and
# what the CLI should then see.
LIMITS_ARMS = [
  "local-429",
  "upstream-429",
  "local-503",
  "upstream-503",
  "switch",
]
# The long case streams for minutes and restart runs three clients; every
# time limit grows by this many seconds for them.
EXTRA_SECONDS = {
  "long": 240,
  "restart": 120,
  "quota": 60,
  "limits": 90,
  "cache": 60,
}
ENV = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
ACCOUNT_ID = r"[a-z0-9][a-z0-9_-]{0,63}"
PLACEHOLDER_TOKEN = "sk-ant-PLACEHOLDER-0123456789ABCDEFG"
NAMES_FILTER = (
  "udp port 53 or tcp port 53"
  " or (tcp[((tcp[12]&0xf0)>>2)]=0x16 and tcp[((tcp[12]&0xf0)>>2)+5]=0x01)"
  " or (ip6 and ip6[6]=6 and ip6[40+((ip6[52]&0xf0)>>2)]=0x16"
  " and ip6[40+((ip6[52]&0xf0)>>2)+5]=0x01)"
)
MITM_HOSTS = [
  "api.anthropic.com", "*.anthropic.com", "anthropic.com", "*.claude.com",
  "claude.com", "*.claude.ai", "claude.ai",
]  # fmt: skip
QUOTA_HEADER_KEYS = ["retry-after", "x-should-retry"]
USAGE_KEYS = [
  "input_tokens", "output_tokens", "cache_creation_input_tokens",
  "cache_read_input_tokens",
]  # fmt: skip


def build_parser() -> argparse.ArgumentParser:
  """Build the command line.

  Returns:
    The parser.
  """
  parser = argparse.ArgumentParser(
    description="Rehearse the gateway in Docker."
  )
  add = parser.add_argument
  add("--gateway-binary", type=Path, required=True)
  add("--cli-binary", type=Path, required=True, help="The Claude CLI binary")
  add(
    "--inventory",
    type=Path,
    required=True,
    help="Directory of account files, <id>.yaml, as the gateway reads them",
  )
  add(
    "--account",
    required=True,
    help="Account ID; its token reference, proxy and expected exit IP are"
    " read from <inventory>/<id>.yaml",
  )
  add(
    "--out-root",
    type=Path,
    required=True,
    help="Directory in which each run creates its own directory",
  )
  add("--case", choices=sorted(INFERENCES), default="hi")
  add("--real-once", action="store_true")
  add(
    "--token-vault",
    help="Authenticated runs: the 1Password vault every oauth_token must name",
  )
  add(
    "--proxy-vault",
    help="Authenticated runs: the 1Password vault the proxy references must"
    " name",
  )
  add(
    "--no-token-swap",
    action="store_true",
    help="Control arm: hand the gateway a placeholder account token; the real"
    " token is not resolved",
  )
  add(
    "--proxy-down",
    action="store_true",
    help="Control arm: synthetic credentials, proxy name pinned to loopback,"
    " external network attached",
  )
  add(
    "--default-traffic",
    action="store_true",
    help="Leave telemetry, error reporting and nonessential traffic at CLI"
    " defaults",
  )
  add(
    "--only-switch",
    choices=[
      "DISABLE_TELEMETRY",
      "DISABLE_ERROR_REPORTING",
      "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
    ],
    help="Synthetic only: set just this one traffic switch, to see which side"
    " requests it removes",
  )
  add(
    "--marketplace-autoinstall-off",
    action="store_true",
    help="Also set CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL in the"
    " client",
  )
  add(
    "--pins",
    type=Path,
    help="sha256sum-format file with absolute paths; the three experiment"
    " files and both binaries must match it",
  )
  add(
    "--launcher-binary",
    type=Path,
    help="redcoast-client; the tui and quota cases then start the CLI through"
    " it, and the launcher issues and revokes the session",
  )
  add(
    "--login-dir",
    type=Path,
    help="quota case: the account's own Claude config directory, mounted"
    " read-only for atb; synthetic runs get a generated one",
  )
  add(
    "--quota-fail-first",
    action="store_true",
    help="Synthetic quota only: the fake upstream answers the first inference"
    " with 401, to show the run stops there",
  )
  add(
    "--quota-fail-from",
    type=int,
    default=0,
    help="Synthetic quota only: the fake upstream answers every inference from"
    " this one on with 500, to count what one CLI sends",
  )
  add(
    "--quota-allow-retries",
    action="store_true",
    help="Synthetic quota only: leave the CLI's own retries on (control arm"
    " for CLAUDE_CODE_MAX_RETRIES=0)",
  )
  add(
    "--quota-stop-on-atb",
    action="store_true",
    help="Synthetic quota only: apply the real-mode rule that a failed"
    " starting atb read stops the run",
  )
  add(
    "--atb-binary",
    type=Path,
    help="quota case: agent-toolbox (atb), which reads the quota through the"
    " forward proxy",
  )
  add(
    "--mitm",
    action="store_true",
    help="Synthetic only: terminate proxied TLS with a throwaway CA to log"
    " request lines; nothing is forwarded",
  )
  add(
    "--second-account",
    help="cache case: account ID of account B (its token reference, proxy"
    " port and expected exit IP are read from <inventory>/<id>.yaml)",
  )
  add(
    "--cache-arm",
    choices=["rebind", "stay"],
    default="rebind",
    help="cache case: rebind injects a reading at or over hard for account A"
    " after turn 1 so turn 2 lands on B; stay injects nothing (control)",
  )
  add(
    "--limits-arm",
    choices=LIMITS_ARMS,
    help="limits case: local-429 (readings put the only account over hard"
    " after turn 1), upstream-429 (the fake upstream answers turn 2 with"
    " 429), local-503 (turn 2 gets 401, the account is paused), upstream-503"
    " (the fake upstream answers turn 2 with 503), switch (two accounts; A"
    " answers turn 2 with 429, the retry lands on B)",
  )
  add(
    "--limits-retry-after",
    type=int,
    default=3600,
    help="limits case: the Retry-After the fake upstream puts on its 429s (its"
    " 5h reset stays an hour ahead, so the gateway pauses the account for the"
    " hour either way)",
  )
  add(
    "--limits-reset-seconds",
    type=int,
    default=3600,
    help="limits case: how far ahead the fake upstream dates the 7d reset of"
    " its readings; the local-429 arm's Retry-After follows it",
  )
  add(
    "--start-at",
    type=float,
    help="Unix time at which the workload starts, at most 180 seconds ahead,"
    " so that two runs overlap",
  )
  return parser


def validate_modes(
  parser: argparse.ArgumentParser, args: argparse.Namespace
) -> None:
  """Refuse flag combinations that mix the modes.

  Args:
    parser: The parser, for its error exit.
    args: The parsed arguments; only_switch implies default_traffic.
  """
  if args.no_token_swap and not args.real_once:
    parser.error("--no-token-swap requires --real-once")
  if args.proxy_down and args.real_once:
    parser.error("--proxy-down never reads credentials")
  if args.mitm and (args.real_once or args.proxy_down):
    parser.error("--mitm is synthetic only")
  if args.only_switch and (args.real_once or args.proxy_down):
    parser.error("--only-switch is synthetic only")
  if args.only_switch:
    args.default_traffic = True
  if (args.real_once or args.proxy_down) and args.pins is None:
    parser.error("runs with external network require --pins")
  if args.case in ["egress", "egress-forward"] and (
    not args.real_once or args.no_token_swap
  ):
    parser.error("the egress cases require --real-once and use no token")
  if args.start_at is not None and not (
    time.time() < args.start_at < time.time() + 180
  ):
    parser.error("--start-at must be within the next 180 seconds")
  if not re.fullmatch(ACCOUNT_ID, args.account):
    parser.error("invalid account ID")


def validate_limits_and_quota(
  parser: argparse.ArgumentParser, args: argparse.Namespace
) -> None:
  """Refuse flags that the limits and quota cases do not allow.

  Args:
    parser: The parser, for its error exit.
    args: The parsed arguments.
  """
  if args.launcher_binary is not None and args.case not in [
    "tui", "quota", "limits", "cache",
  ]:  # fmt: skip
    parser.error(
      "--launcher-binary applies to the tui, quota, limits and cache cases only"
    )
  if args.case == "limits" and (
    args.launcher_binary is None
    or args.limits_arm is None
    or args.real_once
    or args.proxy_down
  ):
    parser.error(
      "the limits case is synthetic, runs the CLI through --launcher-binary"
      " and needs --limits-arm"
    )
  if args.limits_arm is not None and args.case != "limits":
    parser.error("--limits-arm applies to the limits case only")
  if args.case == "quota" and args.launcher_binary is None:
    parser.error("the quota case runs the CLI through --launcher-binary")
  if args.case == "quota" and args.atb_binary is None:
    parser.error("the quota case needs --atb-binary")
  if args.case == "quota" and args.real_once and args.login_dir is None:
    parser.error("the real quota case needs --login-dir")
  if args.login_dir is not None and args.case != "quota":
    parser.error("--login-dir applies to the quota case only")
  controls = (
    args.quota_fail_first
    or args.quota_stop_on_atb
    or args.quota_fail_from
    or args.quota_allow_retries
  )
  if controls and (args.case != "quota" or args.real_once):
    parser.error("the quota control switches are synthetic only")


def validate_cache(
  parser: argparse.ArgumentParser, args: argparse.Namespace
) -> None:
  """Refuse flags that the cache case does not allow.

  Args:
    parser: The parser, for its error exit.
    args: The parsed arguments.
  """
  if args.case == "cache" and (
    args.launcher_binary is None or args.second_account is None
  ):
    parser.error("the cache case needs --launcher-binary and --second-account")
  if args.second_account is None:
    return
  if args.case != "cache":
    parser.error("--second-account applies to the cache case only")
  if not re.fullmatch(ACCOUNT_ID, args.second_account):
    parser.error("invalid second account ID")
  if args.second_account == args.account:
    parser.error("the two accounts must differ")


def fingerprint(path: Path) -> str:
  """Return a file's SHA-256.

  Args:
    path: The file.

  Returns:
    Lowercase hex.
  """
  digest = hashlib.sha256()
  with path.open("rb") as stream:
    for chunk in iter(lambda: stream.read(1 << 20), b""):
      digest.update(chunk)
  return digest.hexdigest()


def now() -> str:
  """Return the current time in UTC as ISO 8601.

  Returns:
    The timestamp.
  """
  return datetime.datetime.now(datetime.UTC).isoformat()


@dataclass(frozen=True)
class Proxy:
  """An account's exit proxy, as its inventory file gives it.

  Attributes:
    host: The proxy's host name.
    port: The proxy's port.
    expected_egress_ip: The exit IP the account must leave from.
    username_ref: Reference to the proxy username.
    password_ref: Reference to the proxy password.
  """

  host: str
  port: int
  expected_egress_ip: str
  username_ref: str
  password_ref: str


@dataclass(frozen=True)
class Account:
  """The part of an account file this rehearsal reads.

  Attributes:
    oauth_token: Reference to the account's token, or None.
    proxy: The account's exit proxy.
  """

  oauth_token: str | None
  proxy: Proxy


REFERENCE = re.compile(r"op://([^/\s]+)/\S+")


def checked_reference(value: object, field: str, vault: str | None) -> str:
  """Return value if it is a 1Password reference, in vault when one is given.

  Args:
    value: The field's value from the file.
    field: The field's name, for the error.
    vault: The vault the reference must name, or None for any.

  Returns:
    The reference.

  Raises:
    ValueError: The value is not such a reference; the message names only
      the field.
  """
  match = REFERENCE.fullmatch(value) if isinstance(value, str) else None
  if match is None or (vault is not None and match.group(1) != vault):
    where = f" in vault {vault}" if vault else ""
    raise ValueError(f"{field} must be an op:// reference{where}")
  return match.group(0)


def load_account(
  inventory: Path,
  account: str,
  token_vault: str | None = None,
  proxy_vault: str | None = None,
) -> Account:
  """Read and check an account's inventory file.

  Only references, the proxy origin and the expected exit IP are read here;
  values are resolved later. Values are not coerced: a quoted port is an
  error. Authenticated runs pass the vaults the references must name, so a
  reference to any other item is refused before anything is resolved.

  Args:
    inventory: The inventory directory.
    account: The account ID, the file's name.
    token_vault: The vault oauth_token must name, or None for any.
    proxy_vault: The vault the proxy references must name, or None for any.

  Returns:
    The account.

  Raises:
    ValueError: A field is missing or malformed; the message names the field
      and never its value.
  """
  data = yaml.safe_load((inventory / (account + ".yaml")).read_text())
  proxy = data.get("proxy") if isinstance(data, dict) else None
  if not isinstance(proxy, dict):
    raise ValueError("proxy must be a mapping")
  token = data.get("oauth_token")
  host, port, ip = (
    proxy.get(k) for k in ("host", "port", "expected_egress_ip")
  )
  if not isinstance(host, str) or not host:
    raise ValueError("proxy.host must be a non-empty string")
  if type(port) is not int or not 1 <= port <= 65535:
    raise ValueError("proxy.port must be an integer from 1 to 65535")
  try:
    ipaddress.ip_address(ip if isinstance(ip, str) else "")
  except ValueError:
    raise ValueError("proxy.expected_egress_ip must be an IP address") from None
  return Account(
    oauth_token=None
    if token is None
    else checked_reference(token, "oauth_token", token_vault),
    proxy=Proxy(
      host=host,
      port=port,
      expected_egress_ip=str(ip),
      username_ref=checked_reference(
        proxy.get("username_ref"), "proxy.username_ref", proxy_vault
      ),
      password_ref=checked_reference(
        proxy.get("password_ref"), "proxy.password_ref", proxy_vault
      ),
    ),
  )


@dataclass
class Setup:
  """Everything a run derives from its arguments before anything starts.

  Attributes:
    args: The parsed arguments.
    account: The account's inventory entry.
    account_b: The cache case's second account, else None.
    gateway: The gateway binary.
    cli: The Claude CLI binary.
    launcher: redcoast-client, when the case runs through it.
    atb: agent-toolbox, in the quota case.
    login: The quota case's Claude config directory for atb.
    case: The case.
    extra: Seconds every time limit grows by.
    real: Whether the run is authenticated.
    proxy_down: Whether this is the proxy-down control arm.
    networked: Whether the containers get external network.
    swap: Whether the real account token goes to the gateway.
    expected_posts: Messages POSTs the case is expected to make.
    expected_outcome: Which outcome rule judges the run.
    synthetic_token: The synthetic account's token, handed to the gateway's
      stdin test entry and expected by the fake API.
    root: The run directory.
    name: The run's name, which labels its Docker objects.
    names: The container names: the gateway's, then the clients'.
    network: The Docker network's name.
  """

  args: argparse.Namespace
  account: Account
  account_b: Account | None
  gateway: Path
  cli: Path
  launcher: Path | None
  atb: Path | None
  login: Path | None
  case: str
  extra: int
  real: bool
  proxy_down: bool
  networked: bool
  swap: bool
  expected_posts: int
  expected_outcome: str
  synthetic_token: str
  root: Path
  name: str
  names: list[str]
  network: str

  @property
  def docker(self) -> list[str]:
    """Return the docker command with the run's own config directory.

    Returns:
      The command prefix.
    """
    return ["docker", "--config", str(self.root / "docker-config")]

  @property
  def label(self) -> str:
    """Return the label on every Docker object of the run.

    Returns:
      The label.
    """
    return "redcoast.hi.owner=" + self.name


def expected_outcome_of(
  case: str, proxy_down: bool, swap: bool, real: bool
) -> str:
  """Name the outcome rule a run is judged by.

  Args:
    case: The case.
    proxy_down: Whether this is the proxy-down control arm.
    swap: Whether the real token goes to the gateway.
    real: Whether the run is authenticated.

  Returns:
    The rule's name.
  """
  if case == "limits":
    return "limits_observed"
  if case in ["egress", "egress-forward"]:
    return "egress_matches_inventory"
  if proxy_down:
    return "fails_without_direct"
  if swap or not real:
    return "case_passes"
  return "upstream_rejects_dummy"


def build_setup(
  parser: argparse.ArgumentParser, args: argparse.Namespace
) -> Setup:
  """Derive the run's setup from validated arguments and create its directory.

  Args:
    parser: The parser, for its error exit.
    args: The validated arguments.

  Returns:
    The setup.
  """
  vaults = (args.token_vault, args.proxy_vault)
  if args.real_once and not all(vaults):
    parser.error("--real-once needs --token-vault and --proxy-vault")
  try:
    account = load_account(args.inventory, args.account, *vaults)
    account_b = (
      load_account(args.inventory, args.second_account, *vaults)
      if args.second_account
      else None
    )
  except ValueError as error:
    parser.error(f"inventory: {error}")
  proxy = account.proxy
  case = args.case
  real = args.real_once
  swap = (
    real and not args.no_token_swap and case not in ["egress", "egress-forward"]
  )
  if swap and account.oauth_token is None:
    parser.error("the account has no oauth_token reference")
  if swap and account_b is not None and account_b.oauth_token is None:
    parser.error("the second account has no oauth_token reference")
  if account_b is not None and (
    account_b.proxy.username_ref,
    account_b.proxy.password_ref,
    account_b.proxy.host,
  ) != (proxy.username_ref, proxy.password_ref, proxy.host):
    parser.error(
      "the two accounts must share the proxy host and credential references"
    )
  wait = int(args.start_at - time.time()) + 1 if args.start_at else 0
  os.umask(0o077)
  root = args.out_root.resolve(strict=True) / (
    "gateway-"
    + case
    + "-"
    + datetime.date.today().isoformat()
    + "-"
    + uuid.uuid4().hex[:12]
  )
  root.mkdir(mode=0o700)
  (root / "capture").mkdir(mode=0o700)
  if case == "cache":
    (root / "signal").mkdir(mode=0o700)
  (root / "docker-config").mkdir()
  login = args.login_dir.resolve(strict=True) if args.login_dir else None
  if case == "quota" and login is None:
    login = synthetic_login(root, args.account)
  name = "redcoast-hi-" + root.name.rsplit("-", 1)[1]
  # restart starts a new client container for each of its three requests.
  names = [name + "-gateway", name + "-client"]
  if case == "restart":
    names += [name + "-client-2", name + "-client-3"]
  # The interactive UI adds a one-token probe request when nonessential
  # traffic is left on.
  probe = 1 if case == "tui" and args.default_traffic else 0
  return Setup(
    args=args,
    account=account,
    account_b=account_b,
    gateway=args.gateway_binary.resolve(strict=True),
    cli=args.cli_binary.resolve(strict=True),
    launcher=args.launcher_binary.resolve(strict=True)
    if args.launcher_binary
    else None,
    atb=args.atb_binary.resolve(strict=True) if case == "quota" else None,
    login=login,
    case=case,
    extra=EXTRA_SECONDS.get(case, 0) + wait,
    real=real,
    proxy_down=args.proxy_down,
    networked=real or args.proxy_down,
    swap=swap,
    expected_posts=INFERENCES[case] + probe,
    expected_outcome=expected_outcome_of(case, args.proxy_down, swap, real),
    synthetic_token="sk-ant-oat01-synthetic-" + args.account,
    root=root,
    name=name,
    names=names,
    network=name + "-net",
  )


def synthetic_login(root: Path, account: str) -> Path:
  """Write a synthetic login directory for atb.

  atb finds a credential of the right shape and its request then fails at
  the fake exit.

  Args:
    root: The run directory.
    account: The inventory ID, part of the synthetic token.

  Returns:
    The directory.
  """
  login = root / "login"
  login.mkdir(mode=0o700)
  (login / ".credentials.json").write_text(
    json.dumps(
      {
        "claudeAiOauth": {
          "accessToken": "sk-ant-oat01-synthetic-login-" + account,
          "refreshToken": "sk-ant-ort01-synthetic",
          "expiresAt": int(time.time() * 1000) + 86400000,
          "scopes": ["user:inference"],
          "subscriptionType": "max",
        }
      }
    )
  )
  return login


def initial_report(setup: Setup) -> dict[str, Any]:
  """Start the report with the run's parameters.

  Args:
    setup: The run's setup.

  Returns:
    The report.
  """
  args = setup.args
  proxy = setup.account.proxy
  return {
    "second_account": args.second_account,
    "cache_arm": args.cache_arm if setup.case == "cache" else None,
    "limits_arm": args.limits_arm,
    "limits_retry_after": args.limits_retry_after,
    "limits_reset_seconds": args.limits_reset_seconds,
    "account": args.account,
    "proxy_endpoint": proxy.host + ":" + str(proxy.port),
    "expected_egress_ip": proxy.expected_egress_ip,
    "case": setup.case,
    "real_mode": setup.real,
    "token_swap": setup.swap,
    "proxy_down": setup.proxy_down,
    "default_traffic": args.default_traffic,
    "only_switch": args.only_switch,
    "marketplace_autoinstall_off": args.marketplace_autoinstall_off,
    "mitm": args.mitm,
    "launcher": str(setup.launcher) if setup.launcher else None,
    "expected_outcome": setup.expected_outcome,
    "expected_posts": setup.expected_posts,
    "start_at": args.start_at,
    "directory": str(setup.root),
    "containers": setup.names,
    "network": setup.network if setup.networked else None,
    "consumer_ttl_seconds": 80 + setup.extra,
    "capture_ttl_seconds": 100 + setup.extra,
    "cleanup": [],
    "cleanup_failures": [],
  }


def interrupted(signum: int, frame: object) -> None:
  """Turn a signal into an exception so that the cleanup runs.

  Args:
    signum: The signal number.
    frame: The interrupted frame.

  Raises:
    RuntimeError: Always.
  """
  raise RuntimeError("experiment interrupted")


def session_processes(
  session: int | None, alive_only: bool = False
) -> list[int] | None:
  """List the processes of a session.

  Args:
    session: The session ID, or None for no processes.
    alive_only: Whether to leave out zombies and stopped processes.

  Returns:
    The PIDs, or None when /proc could not be read.
  """
  found: list[int] = []
  if session is None:
    return found
  for path in Path("/proc").iterdir():
    if not path.name.isdecimal():
      continue
    try:
      fields = (path / "stat").read_text().rsplit(")", 1)[1].split()
      if int(fields[3]) == session and (
        not alive_only or fields[0] in ["R", "S", "D", "I"]
      ):
        found.append(int(path.name))
    except FileNotFoundError:
      continue
    except OSError, ValueError, IndexError:
      return None
  return found


async def resolve(references: list[str]) -> list[str]:
  """Resolve 1Password references with the locked official SDK.

  Only this consumer reads its service account.

  Args:
    references: The references, in the order the values are wanted.

  Returns:
    The values.

  Raises:
    RuntimeError: Without a service account, when a reference does not
      resolve, or when a value is not a clean single line.
  """
  from onepassword.client import Client

  service_account = os.environ.pop("OP_SERVICE_ACCOUNT_TOKEN", None)
  if not service_account:
    raise RuntimeError("service account unavailable")
  async with asyncio.timeout(15):
    sdk = await Client.authenticate(
      auth=service_account,
      integration_name="accounts-gateway-hi",
      integration_version="0.1.0",
    )
    response = await sdk.secrets.resolve_all(references)
  values = []
  for reference in references:
    item = response.individual_responses.get(reference)
    if item is None or item.error is not None or item.content is None:
      raise RuntimeError("credential resolution failed")
    value = item.content.secret
    if (
      not isinstance(value, str)
      or not value
      or any(c in value for c in "\r\n\0")
    ):
      raise RuntimeError("invalid credential")
    values.append(value)
  return values


def phase_of(stamp: float, started: float | None, ended: float | None) -> str:
  """Place a packet's time relative to the workload.

  Args:
    stamp: The packet's Unix time.
    started: The workload's start, if known.
    ended: The workload's end, if known.

  Returns:
    'before_workload', 'workload' or 'after_workload'.
  """
  if started is None or stamp < started:
    return "before_workload"
  if ended is None or stamp <= ended:
    return "workload"
  return "after_workload"


def dns_name(message: bytes, offset: int) -> tuple[str, int]:
  """Decode a DNS name, following compression pointers.

  Args:
    message: The DNS message.
    offset: Where the name starts.

  Returns:
    The name and the offset after it.

  Raises:
    ValueError: When the pointers loop.
  """
  labels = []
  jumps = 0
  end = None
  while True:
    length = message[offset]
    if length >= 192:
      if end is None:
        end = offset + 2
      offset = ((length & 63) << 8) | message[offset + 1]
      jumps += 1
      if jumps > 16:
        raise ValueError("pointer loop")
    elif length == 0:
      return ".".join(labels), (end if end is not None else offset + 1)
    else:
      labels.append(
        message[offset + 1 : offset + 1 + length].decode(
          "ascii", errors="replace"
        )
      )
      offset += 1 + length


def client_hello_sni(body: bytes) -> str | None:
  """Extract the server name from a TLS ClientHello.

  Args:
    body: The TCP payload.

  Returns:
    The name, '' for a ClientHello without one, None when the payload is
    no ClientHello.
  """
  if len(body) < 44 or body[0] != 0x16 or body[5] != 1:
    return None
  offset = 43
  offset += 1 + body[offset]
  offset += 2 + int.from_bytes(body[offset : offset + 2])
  offset += 1 + body[offset]
  end = min(len(body), offset + 2 + int.from_bytes(body[offset : offset + 2]))
  offset += 2
  while offset + 4 <= end:
    kind = int.from_bytes(body[offset : offset + 2])
    size = int.from_bytes(body[offset + 2 : offset + 4])
    offset += 4
    if kind == 0:
      length = int.from_bytes(body[offset + 3 : offset + 5])
      return body[offset + 5 : offset + 5 + length].decode(
        "ascii", errors="replace"
      )
    offset += size
  return ""


def note_dns(
  body: bytes,
  transport: int,
  phase: str,
  queries: dict[tuple[str, str], int],
  addresses: dict[str, set[str]],
) -> None:
  """Count a DNS question or record the addresses of an answer.

  Args:
    body: The transport payload.
    transport: 6 for TCP (length-prefixed), 17 for UDP.
    phase: The packet's phase.
    queries: Question counts by phase and name, updated in place.
    addresses: Names by answered address, updated in place.
  """
  if transport == 6:
    body = body[2:]
  if len(body) < 12:
    return
  flags = int.from_bytes(body[2:4])
  question, cursor = dns_name(body, 12)
  cursor += 4
  if not flags & 0x8000:
    queries[(phase, question)] = queries.get((phase, question), 0) + 1
    return
  for _ in range(int.from_bytes(body[6:8])):
    _, cursor = dns_name(body, cursor)
    kind = int.from_bytes(body[cursor : cursor + 2])
    length = int.from_bytes(body[cursor + 8 : cursor + 10])
    value = body[cursor + 10 : cursor + 10 + length]
    cursor += 10 + length
    if kind in [1, 28]:
      addresses.setdefault(str(ipaddress.ip_address(value)), set()).add(
        question
      )


def pcap_records(data: bytes) -> tuple[int, float, list[tuple[float, bytes]]]:
  """Split a pcap file into its frames.

  Args:
    data: The file, at least its 24-byte header.

  Returns:
    The link type, the number of frames and the frames with their time.
  """
  order = "<" if data[:4] in [b"\xd4\xc3\xb2\xa1", b"\x4d\x3c\xb2\xa1"] else ">"
  divisor = (
    1e9 if data[:4] in [b"\x4d\x3c\xb2\xa1", b"\xa1\xb2\x3c\x4d"] else 1e6
  )
  link = struct.unpack(order + "I", data[20:24])[0]
  offset = 24
  frames = []
  while offset + 16 <= len(data):
    seconds, fraction, size, _ = struct.unpack(
      order + "IIII", data[offset : offset + 16]
    )
    frames.append(
      (seconds + fraction / divisor, data[offset + 16 : offset + 16 + size])
    )
    offset += 16 + size
  return link, len(frames), frames


def project_record(record: dict[str, Any]) -> dict[str, Any]:
  """Project one committed capture file to classified fields.

  No body text leaves the capture directory.

  Args:
    record: The decoded capture file.

  Returns:
    The row.
  """
  points = record["points"]
  arrived = points["client_in"]
  answer = points["upstream_in"]
  sent = points["client_out"]
  # Request IDs are a per-process random prefix and a sequence number, so
  # the prefix names the gateway process.
  row: dict[str, Any] = {
    "started_utc": record["started_utc"],
    "gateway_instance": record["request_id"].rsplit("-", 1)[0],
    "account_alias": record.get("account_alias"),
    "method": arrived.get("method"),
    "path": arrived.get("path"),
    "upstream_reached": points["upstream_out"]["reached"],
    "upstream_status": answer.get("status"),
    "client_status": sent.get("status"),
    "client_context_canceled": record["client_context_canceled"],
    "upstream_body_complete": answer["body"]["http_body_complete"],
    "client_body_complete": sent["body"]["http_body_complete"],
    "duration_ms": round(record["duration_ns"] / 1e6),
    "client_first_byte_ms": round(sent["body"]["first_byte_ns"] / 1e6)
    if "first_byte_ns" in sent["body"]
    else None,
  }
  if "first_byte_ns" in sent["body"] and "first_byte_ns" in answer.get(
    "body", {}
  ):
    # How long the gateway held the first upstream byte, and how long the
    # body kept flowing after it.
    row["first_byte_forward_us"] = round(
      (sent["body"]["first_byte_ns"] - answer["body"]["first_byte_ns"]) / 1e3
    )
    row["body_after_first_byte_ms"] = round(
      (record["duration_ns"] - sent["body"]["first_byte_ns"]) / 1e6
    )
  request = arrived["body"]["safe_analysis"]
  if request.get("format") == "classified-json" and isinstance(
    request["data"].get("messages"), list
  ):
    row["request_messages"] = len(request["data"]["messages"])
  row["quota_headers"] = {
    key: values
    for key, values in (answer.get("headers") or {}).items()
    if key.lower().startswith("anthropic-ratelimit-")
    or key.lower() in QUOTA_HEADER_KEYS
  }
  # What the client was told to wait: the upstream's Retry-After when the
  # response was forwarded, the gateway's own on a local refusal.
  row["client_retry_after"] = next(
    (
      values
      for key, values in (sent.get("headers") or {}).items()
      if key.lower() == "retry-after"
    ),
    None,
  )
  for item in reply_objects(answer["body"]["safe_analysis"]):
    note_reply(row, item)
  return row


def reply_objects(reply: dict[str, Any]) -> list[Any]:
  """List the JSON objects of a classified reply body.

  Compressed bodies carry the same classified forms one level down.

  Args:
    reply: The reply's safe analysis.

  Returns:
    The objects.
  """
  if reply.get("format") == "gzip-analysis":
    reply = reply["analysis"]
  if reply.get("format") == "classified-json":
    return [reply["data"]]
  objects = []
  if reply.get("format") == "redacted-lines":
    for line in reply["data"]:
      try:
        objects.append(json.loads(line))
      except ValueError:
        continue
  return objects


def note_reply(row: dict[str, Any], item: Any) -> None:
  """Record a reply object's stop reason, tool names, error type and usage.

  Token counts only: message_start carries the input and cache counts,
  message_delta the output count.

  Args:
    row: The record's row, updated in place.
    item: One object of the reply.
  """
  if not isinstance(item, dict):
    return
  delta = item.get("delta")
  block = item.get("content_block")
  error = item.get("error")
  if isinstance(delta, dict) and isinstance(delta.get("stop_reason"), str):
    row["stop_reason"] = delta["stop_reason"]
  if (
    isinstance(block, dict)
    and block.get("type") == "tool_use"
    and isinstance(block.get("name"), str)
  ):
    row.setdefault("tool_names", []).append(block["name"])
  if isinstance(error, dict) and isinstance(error.get("type"), str):
    row["error_type"] = error["type"]
  message = item.get("message")
  usages = [
    item.get("usage"),
    message.get("usage") if isinstance(message, dict) else None,
  ]
  for usage in usages:
    if isinstance(usage, dict):
      for key in USAGE_KEYS:
        if isinstance(usage.get(key), int):
          row.setdefault("usage", {})[key] = usage[key]


@dataclass
class Run:
  """One run: its processes, captures and report.

  Attributes:
    setup: The run's setup.
    report: The report, written as result.json at the end.
    gateway: The gateway container process.
    client: The current client container process.
    watch: The packet capture (sudo timeout nsenter tcpdump).
    names_watch: The names capture (DNS and ClientHello only).
    capture_pid: The packet capture's tcpdump PID.
    names_pid: The names capture's tcpdump PID.
    packet: The packet capture's text output file.
    statistics: The packet capture's stderr file.
    names_file: The names capture's pcap file.
    own_addresses: Addresses assigned inside the namespace.
  """

  setup: Setup
  report: dict[str, Any]
  gateway: subprocess.Popen[bytes] | None = None
  client: subprocess.Popen[bytes] | None = None
  watch: subprocess.Popen[bytes] | None = None
  names_watch: subprocess.Popen[bytes] | None = None
  capture_pid: int | None = None
  names_pid: int | None = None
  packet: IO[bytes] | None = None
  statistics: IO[bytes] | None = None
  names_file: IO[bytes] | None = None
  own_addresses: set[str] = field(default_factory=set)

  @property
  def root(self) -> Path:
    """Return the run directory.

    Returns:
      The directory.
    """
    return self.setup.root

  def run(
    self, command: list[str], timeout: float = 10
  ) -> subprocess.CompletedProcess[bytes]:
    """Run a host command with the run's environment.

    Args:
      command: The command line.
      timeout: Seconds before it is killed.

    Returns:
      The completed process.
    """
    return subprocess.run(
      command, env=ENV, capture_output=True, timeout=timeout
    )

  def docker(
    self, *args: str, timeout: float = 10
  ) -> subprocess.CompletedProcess[bytes]:
    """Run a docker command with the run's config directory.

    Args:
      *args: The docker subcommand and its arguments.
      timeout: Seconds before it is killed.

    Returns:
      The completed process.
    """
    return self.run(self.setup.docker + list(args), timeout=timeout)

  @property
  def gateway_runner(self) -> Path:
    """Return the run's private copy of hi_gateway.py, the one it mounts.

    Returns:
      The copy.
    """
    return self.root / "hi_gateway.py"

  @property
  def client_runner(self) -> Path:
    """Return the run's private copy of hi_client.py, the one it mounts.

    Returns:
      The copy.
    """
    return self.root / "hi_client.py"

  def pin_inputs(self) -> None:
    """Fingerprint the inputs and refuse to start unless they match the pins.

    The three experiment files and both binaries (plus the launcher and atb
    when used) must be the pinned bytes. The two runner programs are copied
    into the private run directory first and fingerprinted there; the
    containers mount the copies, so what runs is what was verified, whatever
    happens to the source files afterwards.

    Raises:
      SystemExit: When a pinned input differs.
    """
    setup = self.setup
    report = self.report
    self.gateway_runner.write_bytes(GATEWAY_RUNNER.read_bytes())
    self.client_runner.write_bytes(CLIENT_RUNNER.read_bytes())
    report["gateway_binary_sha256"] = fingerprint(setup.gateway)
    report["claude_binary_sha256"] = fingerprint(setup.cli)
    report["script_sha256"] = fingerprint(HERE / "hi.py")
    report["gateway_runner_sha256"] = fingerprint(self.gateway_runner)
    report["client_runner_sha256"] = fingerprint(self.client_runner)
    if setup.launcher:
      report["launcher_binary_sha256"] = fingerprint(setup.launcher)
    if setup.atb:
      report["atb_binary_sha256"] = fingerprint(setup.atb)
    if setup.args.pins is None:
      return
    pins = {
      path: digest
      for digest, path in (
        line.split(maxsplit=1)
        for line in setup.args.pins.read_text().splitlines()
        if line.strip()
      )
    }
    pinned = [
      (HERE / "hi.py", report["script_sha256"]),
      (GATEWAY_RUNNER, report["gateway_runner_sha256"]),
      (CLIENT_RUNNER, report["client_runner_sha256"]),
      (setup.gateway, report["gateway_binary_sha256"]),
      (setup.cli, report["claude_binary_sha256"]),
    ]
    if setup.launcher:
      pinned.append((setup.launcher, report["launcher_binary_sha256"]))
    if setup.atb:
      pinned.append((setup.atb, report["atb_binary_sha256"]))
    for path, digest in pinned:
      if pins.get(str(path)) != digest:
        raise SystemExit("pinned input mismatch: " + str(path))
    report["pins_sha256"] = fingerprint(setup.args.pins)

  def write_settings(self) -> None:
    """Write the two runner programs' settings into the run directory."""
    setup = self.setup
    args = setup.args
    proxy = setup.account.proxy
    cache_arm = args.cache_arm if setup.case == "cache" else None
    (self.root / "gateway-settings.json").write_text(
      json.dumps(
        {
          "CASE": setup.case,
          "LIMITS_ARM": args.limits_arm,
          "LIMITS_RETRY_AFTER": args.limits_retry_after,
          "LIMITS_RESET_SECONDS": args.limits_reset_seconds,
          "PROXY_HOST": proxy.host,
          "PROXY_PORT": proxy.port,
          "AUTH": setup.networked,
          "PROXY_DOWN": setup.proxy_down,
          "ACCOUNT_ALIAS": args.account,
          "ACCOUNT_TOKEN": setup.synthetic_token,
          "MITM": args.mitm,
          "QUOTA_FAIL_FIRST": args.quota_fail_first,
          "QUOTA_FAIL_FROM": args.quota_fail_from,
          "ACCOUNT_ALIAS_B": args.second_account,
          "PROXY_PORT_B": setup.account_b.proxy.port
          if setup.account_b
          else None,
          "CACHE_ARM": cache_arm,
          "EXPECTED_EGRESS_IP": proxy.expected_egress_ip,
          "EXPECTED_EGRESS_IP_B": setup.account_b.proxy.expected_egress_ip
          if setup.account_b
          else None,
        }
      )
    )
    (self.root / "client-settings.json").write_text(
      json.dumps(
        {
          "CASE": setup.case,
          "LIMITS_ARM": args.limits_arm,
          "REAL": setup.real,
          "DEFAULT_TRAFFIC": args.default_traffic,
          "ONLY_SWITCH": args.only_switch,
          "EXTRA": setup.extra,
          "MARKETPLACE_OFF": args.marketplace_autoinstall_off,
          "MITM": args.mitm,
          "LAUNCHER": setup.launcher is not None,
          "INFERENCES_QUOTA": INFERENCES["quota"],
          "QUOTA_STOP_ON_ATB": args.quota_stop_on_atb,
          "QUOTA_ALLOW_RETRIES": args.quota_allow_retries,
          "CACHE_ARM": cache_arm,
        }
      )
    )

  def require_capture_alive(self, boundary: str) -> None:
    """Record that both captures are alive at a boundary of the workload.

    Args:
      boundary: 'before_workload', 'after_workload' or 'before_stop'.

    Raises:
      RuntimeError: When a capture has stopped or its tcpdump is gone.
    """
    observed = now()
    self.report["capture_alive_" + boundary] = False
    for process, pid in [
      (self.watch, self.capture_pid),
      (self.names_watch, self.names_pid),
    ]:
      if process is None or pid is None or process.poll() is not None:
        raise RuntimeError("capture no longer running")
      members = session_processes(process.pid, alive_only=True)
      if members is None or pid not in members or process.poll() is not None:
        raise RuntimeError("capture process not alive")
    self.report["capture_alive_" + boundary] = True
    # This lower bound precedes the successful liveness observations.
    self.report["capture_last_alive_utc"] = observed

  def container_command(
    self, container_name: str, namespace: str, mounts: list[str]
  ) -> list[str]:
    """Build the docker run command of a locked-down container.

    Args:
      container_name: The container's name.
      namespace: Its --network argument.
      mounts: Bind mounts, as host:container:mode.

    Returns:
      The command line up to the image.
    """
    uid = str(os.getuid())
    command = self.setup.docker + [
      "run", "--rm", "--init", "--name", container_name,
      "--label", self.setup.label, "--network", namespace,
      "--log-driver", "none", "--user", uid + ":" + str(os.getgid()),
      "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
      "--cpus", "1", "--memory", "256m", "--pids-limit", "128",
      "--tmpfs", "/client:rw,nosuid,nodev,size=64m,uid=" + uid,
      "--tmpfs", "/tmp:rw,nosuid,nodev,size=8m",
    ]  # fmt: skip
    for mount in mounts:
      command += ["-v", mount]
    return command

  def credentials(self) -> dict[str, str] | None:
    """Resolve or make up the credentials the gateway container receives.

    Without the swap the gateway sends a placeholder account token upstream,
    which the upstream refuses.

    Returns:
      The credentials, or None in synthetic runs.
    """
    setup = self.setup
    proxy = setup.account.proxy
    if setup.real:
      references = []
      if setup.swap:
        assert setup.account.oauth_token is not None
        references.append(setup.account.oauth_token)
        if setup.account_b:
          assert setup.account_b.oauth_token is not None
          references.append(setup.account_b.oauth_token)
      references += [proxy.username_ref, proxy.password_ref]
      values = asyncio.run(resolve(references))
      credentials = {
        "token": values[0] if setup.swap else PLACEHOLDER_TOKEN,
        "proxy_username": values[-2],
        "proxy_password": values[-1],
      }
      if setup.swap and setup.account_b:
        credentials["token_b"] = values[1]
      return credentials
    if setup.proxy_down:
      return {
        "token": "synthetic-proxy-down",
        "proxy_username": "synthetic",
        "proxy_password": "synthetic",
      }
    return None

  def mitm_mounts(self) -> list[list[str]]:
    """Generate a throwaway CA and leaf for the fake exit's TLS.

    Returns:
      The gateway container's mounts and the client container's mounts.

    Raises:
      RuntimeError: When openssl fails.
    """
    root = self.root
    self.report["stage"] = "mitm_certificates"
    (root / "leaf.cnf").write_text(
      "subjectAltName=" + ",".join("DNS:" + host for host in MITM_HOSTS) + "\n"
    )
    commands = [
      ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key",
       "-out", "ca.pem", "-subj", "/CN=redcoast-throwaway-ca", "-days", "1",
       "-addext", "basicConstraints=critical,CA:TRUE"],
      ["req", "-newkey", "rsa:2048", "-nodes", "-keyout", "leaf.key",
       "-out", "leaf.csr", "-subj", "/CN=redcoast-throwaway-leaf"],
      ["x509", "-req", "-in", "leaf.csr", "-CA", "ca.pem", "-CAkey", "ca.key",
       "-CAcreateserial", "-out", "leaf.pem", "-days", "1",
       "-extfile", "leaf.cnf"],
    ]  # fmt: skip
    for command in commands:
      result = subprocess.run(
        ["openssl", *command],
        env=ENV,
        cwd=root,
        capture_output=True,
        timeout=20,
      )
      if result.returncode:
        raise RuntimeError("certificate generation failed")
    (root / "mitm.pem").write_bytes(
      (root / "leaf.pem").read_bytes() + (root / "leaf.key").read_bytes()
    )
    return [
      [str(root / "mitm.pem") + ":/mitm.pem:ro"],
      [str(root / "ca.pem") + ":/ca.pem:ro"],
    ]

  def start_gateway_container(self, mounts: list[str]) -> None:
    """Start the gateway container with its runner on stdin and stdout.

    Args:
      mounts: Extra bind mounts (the mitm certificate).

    Raises:
      RuntimeError: When the gateway binary changed since it was pinned, or
        the image is missing and cannot be pulled.
    """
    setup = self.setup
    root = self.root
    self.report["stage"] = "gateway_container"
    if fingerprint(setup.gateway) != self.report["gateway_binary_sha256"]:
      raise RuntimeError("gateway binary changed")
    # Pulled before the container starts, so that the capture's wait for the
    # container does not include the download.
    if (
      self.docker("image", "inspect", IMAGE).returncode
      and self.docker("pull", "-q", IMAGE, timeout=300).returncode
    ):
      raise RuntimeError("image pull failed")
    volumes = [
      str(root / "capture") + ":/capture:rw",
      str(self.gateway_runner) + ":/runner.py:ro",
      str(root / "gateway-settings.json") + ":/runner-settings.json:ro",
      str(setup.gateway) + ":/gateway:ro",
    ]
    if setup.case == "cache":
      volumes.append(str(root / "signal") + ":/signal:rw")
    command = self.container_command(
      setup.names[0],
      setup.network if setup.networked else "none",
      volumes + mounts,
    )
    if setup.proxy_down:
      command += ["--add-host", setup.account.proxy.host + ":127.0.0.1"]
    command += [
      "-i", IMAGE, "timeout", "--signal=TERM", "--kill-after=10",
      str(80 + setup.extra), "python", "/runner.py",
    ]  # fmt: skip
    self.gateway = subprocess.Popen(
      command,
      env=ENV,
      stdin=subprocess.PIPE,
      stdout=subprocess.PIPE,
      stderr=subprocess.DEVNULL,
      start_new_session=True,
    )

  def namespace_pid(self) -> int:
    """Wait for the gateway container's PID, which names its namespace.

    Returns:
      The PID.

    Raises:
      RuntimeError: When the container stops first or has no PID in time.
    """
    gateway = self.gateway
    assert gateway is not None
    deadline = time.monotonic() + 6
    while time.monotonic() < deadline:
      result = self.docker(
        "inspect", "--format", "{{.State.Pid}}", self.setup.names[0], timeout=2
      )
      if (
        result.returncode == 0
        and result.stdout.strip().isdigit()
        and int(result.stdout) > 0
      ):
        return int(result.stdout)
      if gateway.poll() is not None:
        raise RuntimeError("gateway container stopped")
      time.sleep(0.1)
    raise RuntimeError("namespace unavailable")

  def start_captures(self) -> None:
    """Start the two tcpdumps in the gateway's namespace and wait for them.

    The packet capture keeps header records as text; the names capture
    keeps whole packets, but only DNS and TLS ClientHello: names without
    payload.

    Raises:
      RuntimeError: When the namespace's addresses cannot be read, a capture
        stops, or neither is listening in time.
    """
    root = self.root
    self.report["stage"] = "capture_start"
    pid = self.namespace_pid()
    # Addresses assigned inside this namespace; every other address is an
    # outside peer, private or not.
    listed = self.run(
      [
        "sudo",
        "-n",
        "nsenter",
        "--target",
        str(pid),
        "--net",
        "ip",
        "-o",
        "addr",
        "show",
      ]
    )
    self.own_addresses = {
      match.group(1)
      for match in re.finditer(r"inet6? ([0-9a-f.:]+)/", listed.stdout.decode())
    }
    if listed.returncode or not self.own_addresses:
      raise RuntimeError("namespace addresses unavailable")
    self.report["namespace_addresses"] = sorted(self.own_addresses)
    ttl = str(100 + self.setup.extra)
    prefix = ["sudo", "-n", "timeout", "--signal=INT", "--kill-after=3", ttl,
              "nsenter", "--target", str(pid), "--net",
              "tcpdump", "-i", "any", "-nn"]  # fmt: skip
    self.packet = (root / "packets.txt").open("wb")
    self.statistics = (root / "tcpdump.txt").open("wb")
    self.watch = subprocess.Popen(
      [*prefix, "-tt", "-q", "-v", "-l", "-s", "96"],
      env=ENV,
      stdout=self.packet,
      stderr=self.statistics,
      start_new_session=True,
    )
    self.names_file = (root / "names.pcap").open("wb")
    self.names_watch = subprocess.Popen(
      [*prefix, "-U", "-s", "2000", "-w", "-", NAMES_FILTER],
      env=ENV,
      stdout=self.names_file,
      stderr=(root / "names-tcpdump.txt").open("wb"),
      start_new_session=True,
    )
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
      if (
        b"listening on" in (root / "tcpdump.txt").read_bytes()
        and b"listening on" in (root / "names-tcpdump.txt").read_bytes()
      ):
        break
      if self.watch.poll() is not None or self.names_watch.poll() is not None:
        raise RuntimeError("capture stopped")
      time.sleep(0.1)
    else:
      raise RuntimeError("capture not ready")
    self.capture_pid, self.names_pid = self.capture_pids()
    self.report["capture_ready_utc"] = now()

  def capture_pids(self) -> tuple[int, int]:
    """Find the tcpdump process of each capture.

    Returns:
      The packet capture's and the names capture's tcpdump PIDs.

    Raises:
      RuntimeError: When there are not exactly two.
    """
    found = []
    for process in [self.watch, self.names_watch]:
      assert process is not None
      for pid in session_processes(process.pid, alive_only=True) or []:
        try:
          if Path("/proc", str(pid), "comm").read_text().strip() == "tcpdump":
            found.append(pid)
        except FileNotFoundError:
          continue
    if len(found) != 2:
      raise RuntimeError("capture process unavailable")
    return found[0], found[1]

  def wait_for_start(self) -> None:
    """Sleep until --start-at, when given."""
    if self.setup.args.start_at:
      time.sleep(max(0, self.setup.args.start_at - time.time()))

  def feed(self, credentials: dict[str, str] | None) -> None:
    """Hand the credentials to the gateway container and close its stdin.

    Args:
      credentials: What the gateway runner receives.

    Raises:
      RuntimeError: When the encoded input exceeds 4 KiB.
    """
    gateway = self.gateway
    assert gateway is not None and gateway.stdin is not None
    encoded = json.dumps(credentials).encode()
    if len(encoded) > 4096:
      raise RuntimeError("credential input too large")
    gateway.stdin.write(encoded + b"\n")
    gateway.stdin.flush()
    gateway.stdin.close()
    gateway.stdin = None

  def ready_line(self, seconds: float) -> bool:
    """Read the gateway runner's ready line.

    Args:
      seconds: How long to wait for it.

    Returns:
      Whether a line arrived and said ready.

    Raises:
      RuntimeError: When the gateway container exits first.
    """
    gateway = self.gateway
    assert gateway is not None and gateway.stdout is not None
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
      ready, _, _ = select.select([gateway.stdout], [], [], 0.2)
      if ready:
        return json.loads(gateway.stdout.readline()).get("ready") is True
      if gateway.poll() is not None:
        raise RuntimeError("gateway exited")
    return False

  def client_mounts(self, mounts: list[str]) -> list[str]:
    """List a client container's bind mounts.

    Args:
      mounts: Extra bind mounts (the mitm CA).

    Returns:
      The mounts.
    """
    setup = self.setup
    root = self.root
    volumes = [
      str(self.client_runner) + ":/runner.py:ro",
      str(root / "client-settings.json") + ":/runner-settings.json:ro",
      str(setup.cli) + ":/claude:ro",
      str(root / "capture" / "session.sock") + ":/session.sock",
    ]
    if setup.launcher:
      volumes.append(str(setup.launcher) + ":/redcoast-client:ro")
    if setup.case == "quota":
      volumes += [
        str(setup.atb) + ":/atb:ro",
        str(setup.cli) + ":/usr/local/bin/claude:ro",
        str(setup.login) + ":/login:ro",
      ]
    if setup.case == "cache":
      volumes.append(str(root / "signal") + ":/signal:rw")
    return volumes + mounts

  def run_consumers(self, mounts: list[str]) -> None:
    """Run one client container per consumer, restarting the gateway once.

    One client in every case but restart, whose third client follows a
    gateway restart; the workload spans from the first to the last.

    Args:
      mounts: Extra bind mounts for the clients.

    Raises:
      RuntimeError: When the client binary changed since it was pinned, or
        the gateway does not come back after the restart.
    """
    setup = self.setup
    report = self.report
    report["stage"] = "dummy_client"
    self.wait_for_start()
    self.require_capture_alive("before_workload")
    report["consumers"] = []
    for index, client_name in enumerate(setup.names[1:]):
      if index == 2:
        # Restart the gateway with the same input; the runner answers with a
        # new ready line.
        report["stage"] = "gateway_restart"
        (self.root / "capture" / "restart-request").touch()
        if not self.ready_line(20):
          raise RuntimeError("gateway restart failed")
        report["stage"] = "dummy_client"
      if fingerprint(setup.cli) != report["claude_binary_sha256"]:
        raise RuntimeError("client binary changed")
      command = self.container_command(
        client_name, "container:" + setup.names[0], self.client_mounts(mounts)
      )
      command += [
        IMAGE, "timeout", "--signal=TERM", "--kill-after=10",
        str(70 + setup.extra), "python", "/runner.py",
      ]  # fmt: skip
      self.client = subprocess.Popen(
        command,
        env=ENV,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
      )
      stdout, _ = self.client.communicate(timeout=75 + setup.extra)
      report.setdefault("client_container_exit_codes", []).append(
        self.client.returncode
      )
      report["consumers"].append(json.loads(stdout))
    consumers = report["consumers"]
    report["consumer"] = dict(
      consumers[-1],
      workload_started_utc=consumers[0].get("workload_started_utc"),
      case_ok=all(c.get("case_ok") is True for c in consumers),
    )
    report["client_container_exit_code"] = (
      0
      if all(code == 0 for code in report["client_container_exit_codes"])
      else 1
    )
    self.require_capture_alive("after_workload")

  def execute(self) -> None:
    """Run the experiment: credentials, containers, captures, workload.

    Raises:
      RuntimeError: When the network cannot be created or the gateway never
        reports ready.
    """
    setup = self.setup
    report = self.report
    report["stage"] = (
      "credential_resolution" if setup.real else "synthetic_setup"
    )
    credentials = self.credentials()
    if setup.networked:
      report["stage"] = "network_create"
      created = self.docker(
        "network", "create", "--label", setup.label, setup.network
      )
      if created.returncode:
        raise RuntimeError("network unavailable")
    mounts = self.mitm_mounts() if setup.args.mitm else [[], []]
    self.start_gateway_container(mounts[0])
    self.start_captures()
    if setup.case == "egress":
      # The gateway container sends the two proxy requests itself once it
      # has the input.
      self.wait_for_start()
      self.require_capture_alive("before_workload")
      report["workload_started_utc"] = now()
    self.feed(credentials)
    credentials = None
    if setup.case == "egress":
      report["stage"] = "egress_requests"
      assert self.gateway is not None
      self.gateway.wait(timeout=45)
      report["workload_ended_utc"] = now()
      self.require_capture_alive("after_workload")
    else:
      report["stage"] = "gateway_ready"
      if not self.ready_line(15):
        raise RuntimeError("gateway not ready")
      self.run_consumers(mounts[1])
    report["stage"] = "completed"
    time.sleep(1)

  def stop_processes(self) -> None:
    """Stop the client and gateway containers and read the gateway's output."""
    report = self.report
    for process in [self.client, self.gateway]:
      if process is None:
        continue
      try:
        if process is self.gateway:
          if process.poll() is None:
            result = self.docker(
              "stop", "--time", "8", self.setup.names[0], timeout=12
            )
            if result.returncode:
              report["cleanup_failures"].append(
                {"operation": "process_stop", "failure_kind": "RuntimeError"}
              )
              continue
          stdout, _ = process.communicate(timeout=10)
          report["gateway_container_exit_code"] = process.returncode
          report["gateway"] = json.loads(stdout.splitlines()[-1])
        elif process.poll() is None:
          os.killpg(process.pid, signal.SIGTERM)
          try:
            process.wait(timeout=3)
          except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=3)
      except Exception as error:  # noqa: BLE001 - recorded, not handled
        report["cleanup_failures"].append(
          {"operation": "process_stop", "failure_kind": type(error).__name__}
        )

  def remove_objects(self) -> None:
    """Remove the run's containers and network.

    Each known object has its own cleanup budget; failure cannot skip later
    objects.
    """
    setup = self.setup
    operations = [["rm", "-f", client_name] for client_name in setup.names[1:]]
    operations.append(["rm", "-f", setup.names[0]])
    if setup.networked:
      operations.append(["network", "rm", setup.network])
    for operation in operations:
      try:
        result = self.docker(*operation)
        if result.returncode:
          self.report["cleanup_failures"].append(
            {"operation": operation[0], "exit_code": result.returncode}
          )
      except Exception as error:  # noqa: BLE001 - recorded, not handled
        self.report["cleanup_failures"].append(
          {"operation": operation[0], "failure_kind": type(error).__name__}
        )

  def stop_capture(self) -> None:
    """Stop the packet capture with an interrupt, or kill its session."""
    watch = self.watch
    if watch is None:
      return
    report = self.report
    report.pop("capture_ended_utc", None)
    report["capture_stop_requested"] = False
    failure = None
    try:
      self.require_capture_alive("before_stop")
      stops = [
        self.run(["sudo", "-n", "kill", "-INT", str(pid)]).returncode
        for pid in [self.capture_pid, self.names_pid]
      ]
      if any(stops):
        failure = "RuntimeError"
      else:
        report["capture_stop_requested"] = True
        report["capture_ended_utc"] = report["capture_last_alive_utc"]
        watch.wait(timeout=5)
    except Exception as error:  # noqa: BLE001 - recorded, not handled
      failure = type(error).__name__
    if failure:
      report["cleanup_failures"].append(
        {"operation": "capture_stop", "failure_kind": failure}
      )
      try:
        if watch.poll() is None:
          self.run(["sudo", "-n", "kill", "-KILL", "--", "-" + str(watch.pid)])
        watch.wait(timeout=3)
      except Exception as error:  # noqa: BLE001 - recorded, not handled
        report["cleanup_failures"].append(
          {"operation": "capture_kill", "failure_kind": type(error).__name__}
        )
    report["capture_exit_code"] = watch.returncode
    report["capture_exit_observed_utc"] = now()

  def stop_names_capture(self) -> None:
    """Wait for the names capture, interrupted with the other, or kill it."""
    names_watch = self.names_watch
    if names_watch is None:
      return
    report = self.report
    try:
      names_watch.wait(timeout=5)
    except Exception as error:  # noqa: BLE001 - recorded, not handled
      report["cleanup_failures"].append(
        {
          "operation": "names_capture_stop",
          "failure_kind": type(error).__name__,
        }
      )
      try:
        self.run(
          ["sudo", "-n", "kill", "-KILL", "--", "-" + str(names_watch.pid)]
        )
        names_watch.wait(timeout=3)
      except Exception as error:  # noqa: BLE001 - recorded, not handled
        report["cleanup_failures"].append(
          {
            "operation": "names_capture_kill",
            "failure_kind": type(error).__name__,
          }
        )
    report["names_capture_exit_code"] = names_watch.returncode

  def count_residue(self) -> None:
    """Count the run's containers, networks and processes, twice."""
    report = self.report
    for iteration in range(2):
      groups = [
        session_processes(process.pid)
        for process in [self.client, self.gateway, self.watch, self.names_watch]
        if process is not None
      ]
      row: dict[str, Any] = {
        "containers": None,
        "networks": None,
        "processes": None
        if any(group is None for group in groups)
        else sum(len(group) for group in groups if group is not None),
      }
      for label, operation in [
        ("containers", ["ps", "-aq"]),
        ("networks", ["network", "ls", "-q"]),
      ]:
        try:
          result = self.docker(
            *operation, "--filter", "label=" + self.setup.label
          )
          if result.returncode == 0:
            row[label] = len(result.stdout.splitlines())
          else:
            report["cleanup_failures"].append(
              {"operation": label, "exit_code": result.returncode}
            )
        except Exception as error:  # noqa: BLE001 - recorded, not handled
          report["cleanup_failures"].append(
            {"operation": label, "failure_kind": type(error).__name__}
          )
      report["cleanup"].append(row)
      if iteration == 0:
        time.sleep(1)

  def http_records(self) -> list[dict[str, Any]]:
    """Project the committed capture files to classified rows.

    Forward-proxy records sit beside the HTTP ones; connect_records projects
    them.

    Returns:
      The rows in time order.
    """
    rows = [
      project_record(json.loads(path.read_text()))
      for path in sorted((self.root / "capture").glob("*.json"))
      if not path.name.startswith("connect-")
    ]
    return sorted(rows, key=lambda row: row["started_utc"])

  def connect_records(self) -> list[dict[str, Any]]:
    """Project the gateway's forward-proxy records to the proxy log's shape.

    Returns:
      The rows in time order.
    """
    rows = []
    keys = [
      "account_alias", "method", "target", "started_utc", "result",
      "exit_status", "bytes_up", "bytes_down",
    ]  # fmt: skip
    for path in sorted((self.root / "capture").glob("connect-*.json")):
      record = json.loads(path.read_text())
      rows.append(
        {key: record.get(key) for key in keys}
        | {"duration_ms": round(record["duration_ns"] / 1e6)}
      )
    return sorted(rows, key=lambda row: row["started_utc"])

  def outside(self, address: str) -> bool:
    """Report whether an address is a peer outside the namespace.

    Args:
      address: The address.

    Returns:
      True unless it is loopback or one of the namespace's own.
    """
    parsed = ipaddress.ip_address(address)
    return not parsed.is_loopback and str(parsed) not in self.own_addresses

  def note_frame(
    self,
    frame: bytes,
    link: int,
    phase: str,
    queries: dict[tuple[str, str], int],
    addresses: dict[str, set[str]],
    hellos: dict[tuple[str, str, str], int],
  ) -> None:
    """Record what one captured frame says about names.

    Args:
      frame: The frame.
      link: The pcap link type (276 for Linux SLL2).
      phase: The frame's phase.
      queries: DNS question counts, updated in place.
      addresses: Names by answered address, updated in place.
      hellos: ClientHello counts by phase, name and peer, updated in place.
    """
    if link == 276:
      protocol, packet = int.from_bytes(frame[0:2]), frame[20:]
    else:
      protocol, packet = int.from_bytes(frame[14:16]), frame[16:]
    if protocol == 0x0800:
      transport = packet[9]
      target = str(ipaddress.ip_address(packet[16:20]))
      segment = packet[(packet[0] & 15) * 4 :]
    elif protocol == 0x86DD:
      transport = packet[6]
      target = str(ipaddress.ip_address(packet[24:40]))
      segment = packet[40:]
    else:
      return
    ports = [int.from_bytes(segment[0:2]), int.from_bytes(segment[2:4])]
    body = segment[8:] if transport == 17 else segment[(segment[12] >> 4) * 4 :]
    if 53 in ports:
      note_dns(body, transport, phase, queries, addresses)
    elif transport == 6:
      name = client_hello_sni(body)
      if name is None:
        return
      peer = (target if self.outside(target) else "local") + ":" + str(ports[1])
      hellos[(phase, name, peer)] = hellos.get((phase, name, peer), 0) + 1

  def names_summary(
    self, started: float | None, ended: float | None
  ) -> dict[str, Any]:
    """Summarize the names capture.

    DNS questions, answered addresses and ClientHello server names from the
    filtered capture.

    Args:
      started: The workload's start as Unix time, if known.
      ended: The workload's end, if known.

    Returns:
      The summary.
    """
    data = (self.root / "names.pcap").read_bytes()
    if len(data) < 24:
      return {
        "packets": 0,
        "dns_queries": [],
        "addresses": {},
        "client_hellos": [],
        "unparsed": 0,
      }
    queries: dict[tuple[str, str], int] = {}
    addresses: dict[str, set[str]] = {}
    hellos: dict[tuple[str, str, str], int] = {}
    unparsed = 0
    link, packets, frames = pcap_records(data)
    for stamp, frame in frames:
      phase = phase_of(stamp, started, ended)
      try:
        self.note_frame(frame, link, phase, queries, addresses, hellos)
      except IndexError, ValueError, struct.error:
        unparsed += 1
    return {
      "packets": packets,
      "unparsed": unparsed,
      "dns_queries": [
        {"phase": phase, "name": name, "count": count}
        for (phase, name), count in sorted(queries.items())
      ],
      "addresses": {
        address: sorted(found) for address, found in sorted(addresses.items())
      },
      "client_hellos": [
        {"phase": phase, "sni": name, "peer": peer, "count": count}
        for (phase, name, peer), count in sorted(hellos.items())
      ],
    }

  def direct_peers(
    self, summary: dict[str, Any], names: dict[str, Any]
  ) -> dict[str, Any]:
    """List the peers that are direct connections, per phase.

    A peer outside this namespace is proxy traffic only if it is the
    account's proxy port on an address that DNS returned for the account's
    proxy name in this run; everything else, the other account's port
    included, is a direct connection.

    Args:
      summary: The packet summary by phase and peer.
      names: The names summary.

    Returns:
      Per phase, the direct peers with their records and names.
    """
    proxy = self.setup.account.proxy
    ports = [proxy.port]
    if self.setup.account_b:
      ports.append(self.setup.account_b.proxy.port)
    exits = {
      address + ":" + str(port)
      for address, found in names["addresses"].items()
      if proxy.host in found
      for port in ports
    }
    result = {}
    for phase, cells in summary.items():
      result[phase] = {}
      for key, cell in cells.items():
        if key.startswith("local:") or key in exits:
          continue
        address = key.rsplit(":", 1)[0]
        snis = [h["sni"] for h in names["client_hellos"] if h["peer"] == key]
        found = names["addresses"].get(address, []) + snis
        result[phase][key] = {**cell, "names": sorted(set(found))}
    return result

  def packet_summary(
    self, started: float | None, ended: float | None
  ) -> dict[str, Any]:
    """Count header records per peer and phase; the quick text has no payload.

    Args:
      started: The workload's start as Unix time, if known.
      ended: The workload's end, if known.

    Returns:
      Per phase and peer:port, the record and payload record counts.
    """
    summary: dict[str, Any] = {}
    stamp = None
    for line in (
      (self.root / "packets.txt").read_text(errors="replace").splitlines()
    ):
      if match := re.match(r"(\d+\.\d+) ", line):
        stamp = float(match.group(1))
      match = re.search(
        r"(\S+)\.(\d+) > (\S+)\.(\d+): (?:tcp|UDP,? length) (\d+)", line
      )
      if not match or stamp is None:
        continue
      peer = "local"
      for address in [match.group(1), match.group(3)]:
        if self.outside(address):
          peer = address
      phase = phase_of(stamp, started, ended)
      port = min(int(match.group(2)), int(match.group(4)))
      cell = summary.setdefault(phase, {}).setdefault(
        peer + ":" + str(port), {"records": 0, "payload_records": 0}
      )
      cell["records"] += 1
      cell["payload_records"] += int(int(match.group(5)) > 0)
    return summary

  def base_hygiene(
    self, consumer: dict[str, Any], server: dict[str, Any]
  ) -> bool:
    """Judge isolation, identity and exits of the containers.

    In both failing control arms the CLI keeps retrying until its time
    limit; that timeout is expected there.

    Args:
      consumer: The (last) client's output.
      server: The gateway runner's output.

    Returns:
      Whether this part of hygiene holds.
    """
    setup = self.setup
    report = self.report
    hygiene = (
      report.get("gateway_container_exit_code") == 0
      and "failure_kind" not in server
      and "cleanup_failure_kind" not in server
      and "failure_kind" not in report
      and not report["cleanup_failures"]
      and all(
        row == {"containers": 0, "networks": 0, "processes": 0}
        for row in report["cleanup"]
      )
    )
    if setup.case == "egress":
      return hygiene
    retried_until_timeout = (
      setup.expected_outcome
      in ["fails_without_direct", "upstream_rejects_dummy"]
      and consumer.get("failure_kind") == "TimeoutExpired"
      and consumer.get("cli_error_classes") == ["process_timeout"]
    )

    def clean(c: dict[str, Any]) -> bool:
      return (
        c.get("credentials_file_created") is False
        and "cleanup_failure_kind" not in c
        and ("failure_kind" not in c or retried_until_timeout)
        and c.get("identity_check")
        in ["config_missing", "account_missing", "email_missing"]
        and all(
          c.get(key) is False
          for key in [
            "server_binary_present",
            "capture_directory_present",
            "service_account_present",
          ]
        )
      )

    hygiene = (
      hygiene
      and report.get("client_container_exit_code") == 0
      and server.get("ready") is True
      and server.get("gateway_exit_code") == 0
      and server.get("capture_persistence_failure_count") == 0
      and server.get("other_gateway_stderr_present") is False
      and len(report.get("consumers", [])) == len(setup.names) - 1
      and all(clean(c) for c in report["consumers"])
    )
    # The first gateway of a restart run has to stop as cleanly as the last.
    if setup.case == "restart":
      hygiene = (
        hygiene
        and server.get("restarted") is True
        and server.get("first_gateway_exit_code") == 0
        and server.get("first_gateway_stderr_present") is False
      )
    return hygiene

  def capture_hygiene(self, workload: dict[str, Any]) -> bool:
    """Judge the captures: exits, drop counts and coverage of the workload.

    Args:
      workload: Where the workload's bounds are recorded.

    Returns:
      Whether the captures hold up.
    """
    report = self.report
    for label, source in [
      ("capture_counts", "tcpdump.txt"),
      ("names_capture_counts", "names-tcpdump.txt"),
    ]:
      if (self.root / source).exists():
        text = (self.root / source).read_text(errors="replace")
        report[label] = {
          key: int(match.group(1))
          if (match := re.search(r"(\d+) packets " + key, text))
          else None
          for key in ["captured", "received by filter", "dropped by kernel"]
        }
    counts = report.get("capture_counts", {})
    names_counts = report.get("names_capture_counts", {})
    report["capture_covers_workload"] = (
      all(
        report.get("capture_alive_" + boundary) is True
        for boundary in ["before_workload", "after_workload", "before_stop"]
      )
      and report.get("capture_stop_requested") is True
      and all(
        key in report for key in ["capture_ready_utc", "capture_ended_utc"]
      )
      and all(
        key in workload
        for key in ["workload_started_utc", "workload_ended_utc"]
      )
      and report["capture_ready_utc"]
      < workload["workload_started_utc"]
      < workload["workload_ended_utc"]
      < report["capture_ended_utc"]
    )
    return (
      report.get("capture_exit_code") == 0
      and report.get("names_capture_exit_code") == 0
      and all(
        type(found.get(key)) is int
        for found in [counts, names_counts]
        for key in ["captured", "received by filter", "dropped by kernel"]
      )
      and (counts.get("captured") or 0) > 0
      and counts.get("dropped by kernel") == 0
      and names_counts.get("dropped by kernel") == 0
      and report["capture_covers_workload"]
    )

  def summarize_traffic(self, workload: dict[str, Any]) -> None:
    """Summarize both captures and judge whether any direct peer appears.

    Only the proxy-down arm makes a direct connection on purpose, and only
    before the workload. Zero findings count only when the capture
    demonstrably sees the two control packets.

    Args:
      workload: Where the workload's bounds are recorded.
    """
    report = self.report
    bounds = [
      datetime.datetime.fromisoformat(workload[key]).timestamp()
      if key in workload
      else None
      for key in ["workload_started_utc", "workload_ended_utc"]
    ]
    try:
      report["packet_summary"] = self.packet_summary(*bounds)
      report["names"] = self.names_summary(*bounds)
      report["direct_peers"] = self.direct_peers(
        report["packet_summary"], report["names"]
      )
      report["direct_free"] = not any(
        cells
        for phase, cells in report["direct_peers"].items()
        if not (self.setup.proxy_down and phase == "before_workload")
      )
    except Exception as error:  # noqa: BLE001 - recorded, not handled
      report["traffic_summary_failure_kind"] = type(error).__name__
    names = report.get("names", {})
    report["capture_controls_seen"] = any(
      query["name"] == "capture-control.invalid"
      for query in names.get("dns_queries", [])
    ) and any(
      hello["sni"] == "capture-control.invalid"
      for hello in names.get("client_hellos", [])
    )

  def collect_records(self, server: dict[str, Any]) -> None:
    """Read the gateway's capture records and the proxy log into the report.

    The gateway's forward proxy carried the side traffic in networked runs;
    its records are the proxy log. Synthetic runs take the fake exit's.

    Args:
      server: The gateway runner's output.
    """
    report = self.report
    setup = self.setup
    try:
      report["http_records"] = self.http_records()
    except Exception as error:  # noqa: BLE001 - recorded, not handled
      report["http_records_failure_kind"] = type(error).__name__
    if setup.networked and setup.case != "egress":
      try:
        report["proxy_log"] = self.connect_records()
      except Exception as error:  # noqa: BLE001 - recorded, not handled
        report["proxy_log_failure_kind"] = type(error).__name__
    else:
      report["proxy_log"] = server.get("proxy_log", [])
    report["partial_capture_files"] = len(
      list((self.root / "capture").glob("*.partial"))
    )

  def records_hygiene(
    self, server: dict[str, Any], records: list[dict[str, Any]]
  ) -> bool:
    """Judge the records: completeness, account naming and session hygiene.

    Every forwarded request names the one account; a locally refused one
    names none. In synthetic runs the fake API must have seen the synthetic
    account token and never a session credential. The switch arm's second
    account is the one other alias a forwarded request may name. The
    consumers' sessions were issued and revoked: by the consumer itself, or,
    through the launcher, as the gateway's sessions table shows once it has
    stopped.

    Args:
      server: The gateway runner's output.
      records: The HTTP records.

    Returns:
      Whether the records hold up.
    """
    report = self.report
    setup = self.setup
    args = setup.args
    aliases = [args.account]
    if args.limits_arm == "switch":
      aliases.append(args.account + "-b")
    if args.second_account:
      aliases.append(args.second_account)
    proxy_log = report["proxy_log"] if setup.networked else []
    hygiene = (
      "http_records" in report
      and report["partial_capture_files"] == 0
      and "proxy_log_failure_kind" not in report
      and all(
        (row["account_alias"] in aliases)
        if row["upstream_reached"]
        else row["account_alias"] is None
        for row in records
      )
      and all(
        row["account_alias"]
        == (
          args.account
          if row["result"] not in ["auth_failed", "no_account", "refused"]
          else None
        )
        for row in proxy_log
      )
    )
    if not setup.networked:
      hygiene = hygiene and all(
        row["account_authorization"] and not row["session_credential_forwarded"]
        for row in server.get("requests") or []
      )
    return hygiene and all(
      c.get("session_id") and c.get("session_revoked") is True
      for c in report.get("consumers", [])
      if c.get("session_source") != "launcher"
    )

  def session_hygiene(
    self,
    server: dict[str, Any],
    consumer: dict[str, Any],
    inference: list[dict[str, Any]],
  ) -> bool:
    """Judge the gateway's sessions table against the consumers.

    Every inference row names the account and the CLI's own transcript. This
    is a reading for every run (a `-p` run without session persistence has
    no transcript) and a requirement with the launcher. tui: the one
    consumer's session came from the launcher. quota: the consumer's own
    session for atb plus one launcher session per inference request. Every
    session ended by a revoke.

    Args:
      server: The gateway runner's output.
      consumer: The (last) client's output.
      inference: The gateway's inference traffic rows.

    Returns:
      Whether the sessions hold up (always True without the launcher).
    """
    report = self.report
    setup = self.setup
    transcripts = consumer.get("transcript_session_ids") or []
    report["traffic_matches_transcript"] = bool(inference) and all(
      row["alias"] == setup.args.account
      and row["claude_session_id"] in transcripts
      for row in inference
    )
    if not setup.launcher:
      return True
    consumers = report.get("consumers", [])
    launched = [c for c in consumers if c.get("session_source") == "launcher"]
    expected_sessions = (
      1 + INFERENCES["quota"] if setup.case == "quota" else len(launched)
    )
    sessions = server.get("sessions") or []
    return (
      "store_read_failure_kind" not in server
      and (setup.case == "quota" or len(launched) == len(consumers))
      and len(sessions) == expected_sessions
      and all(
        s["ended"]
        and s["end_reason"] == "client"
        and s["client_machine"] == "client-container"
        for s in sessions
      )
    )

  def outcome_cache(
    self,
    outcome: bool,
    consumer: dict[str, Any],
    server: dict[str, Any],
    posts: list[dict[str, Any]],
  ) -> bool:
    """Judge the cache case.

    Turn 1 on A. The rebind arm's injection must have found turn 1's reading
    and turn 2 must have landed on B with a quota_hard rebind; the stay arm
    keeps both turns on A with no rebind. The cache counts are readings; in
    synthetic runs the fake's per-account cache is also asserted, so a
    judgement that could not tell the arms apart would show there.

    Args:
      outcome: The outcome so far.
      consumer: The client's output.
      server: The gateway runner's output.
      posts: The Messages POSTs.

    Returns:
      The outcome.
    """
    report = self.report
    args = self.setup.args
    history = server.get("binding_history") or []
    injection = consumer.get("injection")
    report["cache_sequence"] = [
      {
        "started_utc": row["started_utc"],
        "account": row.get("account_alias"),
        "usage": row.get("usage"),
        "request_messages": row.get("request_messages"),
      }
      for row in posts
    ]
    report["injection"] = server.get("injection")
    report["binding_history"] = history
    outcome = (
      outcome
      and len(posts) == 2
      and posts[0]["account_alias"] == args.account
      and all("usage" in row for row in posts)
    )
    if args.cache_arm == "rebind":
      outcome = (
        outcome
        and bool(injection and injection.get("ok"))
        and posts[1]["account_alias"] == args.second_account
        and any(
          row["alias"] == args.second_account and row["reason"] == "quota_hard"
          for row in history
        )
      )
    else:
      outcome = (
        outcome
        and posts[1]["account_alias"] == args.account
        and not any(row["reason"] == "quota_hard" for row in history)
      )
    if self.setup.real:
      return outcome
    second = posts[1]["usage"] if outcome else {}
    return (
      outcome
      and posts[0]["usage"].get("cache_read_input_tokens") == 0
      and posts[0]["usage"].get("cache_creation_input_tokens", 0) > 0
      and (
        (
          second.get("cache_read_input_tokens") == 0
          and second.get("cache_creation_input_tokens", 0) > 0
        )
        if args.cache_arm == "rebind"
        else second.get("cache_read_input_tokens", 0) > 0
      )
    )

  def outcome_quota(
    self,
    outcome: bool,
    server: dict[str, Any],
    posts: list[dict[str, Any]],
    inference: list[dict[str, Any]],
    tunnels: list[dict[str, Any]],
  ) -> bool:
    """Judge the quota case.

    Five inference rows for the account and, in real mode, atb's two
    tunnels to the API; the quota headers of every response and the
    gateway's quota_latest are readings for the record, not judged.

    Args:
      outcome: The outcome so far.
      server: The gateway runner's output.
      posts: The Messages POSTs.
      inference: The gateway's inference traffic rows.
      tunnels: The gateway's CONNECT traffic rows.

    Returns:
      The outcome.
    """
    report = self.report
    setup = self.setup
    outcome = (
      outcome
      and len(inference) == len(posts) == INFERENCES["quota"]
      and all(row["alias"] == setup.args.account for row in inference)
      and (
        not setup.real
        or (
          len(tunnels) == 2
          and all(
            row["host"] == "api.anthropic.com" and row["result"] == "ok"
            for row in tunnels
          )
        )
      )
    )
    report["quota_sequence"] = [
      {"started_utc": row["started_utc"], "headers": row.get("quota_headers")}
      for row in posts
    ]
    report["quota_latest"] = server.get("quota_latest")
    return outcome

  def outcome_shape(self, outcome: bool, posts: list[dict[str, Any]]) -> bool:
    """Judge the per-case shape of the Messages POSTs.

    History grows with each turn; the exact step depends on messages the
    CLI adds itself. The upstream's own wait before its first byte is not
    the gateway's; only the forwarding delay is. In the restart case two
    clients reach the first gateway process and the third its replacement.

    Args:
      outcome: The outcome so far.
      posts: The Messages POSTs.

    Returns:
      The outcome.
    """
    case = self.setup.case
    counts = [row.get("request_messages") or 0 for row in posts]
    if case in ["turns", "tool"]:
      outcome = outcome and all(
        earlier < later
        for earlier, later in zip(counts, counts[1:], strict=False)
      )
    if case == "tool":
      outcome = (
        outcome
        and posts[0].get("stop_reason") == "tool_use"
        and posts[0].get("tool_names") == ["Read"]
      )
    if case == "long":
      outcome = outcome and posts[0]["duration_ms"] >= 120000
    if case in ["stream", "long"]:
      outcome = (
        outcome
        and 0 <= posts[0].get("first_byte_forward_us", -1) <= 50000
        and posts[0].get("body_after_first_byte_ms", 0) >= 200
      )
    if case == "restart":
      instances = [row["gateway_instance"] for row in posts]
      outcome = (
        outcome
        and len(instances) == 3
        and instances[0] == instances[1] != instances[2]
      )
    return outcome

  def outcome_case_passes(
    self,
    consumer: dict[str, Any],
    server: dict[str, Any],
    posts: list[dict[str, Any]],
    inference: list[dict[str, Any]],
    tunnels: list[dict[str, Any]],
  ) -> bool:
    """Judge a run expected to pass its case.

    A single switch may or may not remove the interactive UI's extra probe
    request; which one is the finding. Through the launcher: the rows match
    the transcript, and with both traffic switches set there is no CONNECT.

    Args:
      consumer: The client's output.
      server: The gateway runner's output.
      posts: The Messages POSTs.
      inference: The gateway's inference traffic rows.
      tunnels: The gateway's CONNECT traffic rows.

    Returns:
      The outcome.
    """
    setup = self.setup
    report = self.report
    expected = (
      [INFERENCES[setup.case], setup.expected_posts]
      if report.get("only_switch")
      else [setup.expected_posts]
    )
    outcome = consumer.get("case_ok") is True and len(posts) in expected
    if setup.case == "cancel":
      outcome = (
        outcome
        and posts[0]["upstream_status"] == 200
        and posts[0]["client_context_canceled"] is True
        and posts[0]["upstream_body_complete"] is False
      )
    else:
      outcome = outcome and all(
        row["upstream_status"] == 200 and row["client_body_complete"]
        for row in posts
      )
    if setup.launcher and setup.case == "tui":
      outcome = (
        outcome
        and report["traffic_matches_transcript"]
        and len(inference) == len(posts)
        and not tunnels
      )
    if setup.case == "cache":
      outcome = self.outcome_cache(outcome, consumer, server, posts)
    if setup.case == "quota":
      outcome = self.outcome_quota(outcome, server, posts, inference, tunnels)
    return self.outcome_shape(outcome, posts)

  def outcome_limits(
    self,
    consumer: dict[str, Any],
    server: dict[str, Any],
    posts: list[dict[str, Any]],
  ) -> bool:
    """Judge the limits case.

    Turn 1 is a normal 200 on A. What turn 2's requests saw is the arm; the
    CLI's behaviour (retries, intervals, screen text) is recorded, not
    judged, except that the switch arm must finish on B.

    Args:
      consumer: The client's output.
      server: The gateway runner's output.
      posts: The Messages POSTs.

    Returns:
      The outcome.
    """
    report = self.report
    args = self.setup.args
    arm = args.limits_arm
    turn2 = posts[1:]
    report["limits_sequence"] = [
      {
        "started_utc": row["started_utc"],
        "account": row.get("account_alias"),
        "upstream_reached": row["upstream_reached"],
        "upstream_status": row["upstream_status"],
        "client_status": row["client_status"],
        "error_type": row.get("error_type"),
        "upstream_retry_after": (row.get("quota_headers") or {}).get(
          "Retry-After"
        )
        or (row.get("quota_headers") or {}).get("retry-after"),
        "client_retry_after": row.get("client_retry_after"),
        "request_messages": row.get("request_messages"),
      }
      for row in posts
    ]
    report["limits_turn2_seconds"] = consumer.get("turn2_seconds")
    report["limits_snapshots"] = consumer.get("turn2_snapshots")
    report["account_state"] = server.get("account_state")
    report["quota_latest"] = server.get("quota_latest")
    outcome = (
      consumer.get("case_ok") is True
      and len(posts) >= 2
      and posts[0]["upstream_status"] == 200
      and posts[0]["account_alias"] == args.account
      and len(turn2) >= 1
    )
    if not outcome:
      return False
    first = turn2[0]
    rest = turn2[1:]
    if arm == "local-429":
      return (
        not first["upstream_reached"]
        and first["client_status"] == 429
        and all(
          (not row["upstream_reached"] and row["client_status"] == 429)
          or (row["upstream_status"] == 200 and args.limits_reset_seconds < 60)
          for row in rest
        )
      )
    if arm == "upstream-429":
      return (
        first["upstream_reached"]
        and first["upstream_status"] == 429
        and all(
          not row["upstream_reached"] and row["client_status"] == 429
          for row in rest
        )
      )
    if arm == "local-503":
      return (
        first["upstream_reached"]
        and first["upstream_status"] == 401
        and all(
          not row["upstream_reached"] and row["client_status"] == 503
          for row in rest
        )
      )
    if arm == "upstream-503":
      return all(
        row["upstream_reached"] and row["upstream_status"] == 503
        for row in turn2
      )
    return self.outcome_switch(consumer, turn2)

  def outcome_switch(
    self, consumer: dict[str, Any], turn2: list[dict[str, Any]]
  ) -> bool:
    """Judge the switch arm's second turn.

    The CLI retries a 429 only when Retry-After is short enough (the cap is
    between 7 s and an hour in these runs): with a short one the retry must
    land on B carrying the history; with an hour the CLI gives up and
    nothing lands anywhere. The CLI's own extra one-message request may land
    too.

    Args:
      consumer: The client's output.
      turn2: The Messages POSTs from the second turn on.

    Returns:
      The outcome.
    """
    args = self.setup.args
    landed = [row for row in turn2 if row["upstream_status"] == 200]
    outcome = turn2[0]["upstream_status"] == 429 and all(
      row["account_alias"] == args.account
      for row in turn2
      if row["upstream_status"] == 429
    )
    if args.limits_retry_after < 60:
      return (
        outcome
        and bool(landed)
        and all(row["account_alias"] == args.account + "-b" for row in landed)
        and any((row.get("request_messages") or 0) >= 3 for row in landed)
        and consumer.get("turn2_reply") == "Hi"
      )
    return outcome and not landed and consumer.get("turn2_reply") is None

  def outcome_proxy_down(
    self,
    consumer: dict[str, Any],
    server: dict[str, Any],
    posts: list[dict[str, Any]],
    records: list[dict[str, Any]],
  ) -> bool:
    """Judge the proxy-down control arm.

    Args:
      consumer: The client's output.
      server: The gateway runner's output.
      posts: The Messages POSTs.
      records: All HTTP records.

    Returns:
      The outcome.
    """
    report = self.report
    phases = report.get("packet_summary", {})

    def direct(phase: str) -> int:
      return sum(
        cell["records"]
        for key, cell in phases.get(phase, {}).items()
        if not key.startswith("local:")
      )

    report["direct_control_records"] = direct("before_workload")
    report["direct_workload_records"] = direct("workload") + direct(
      "after_workload"
    )
    port = str(self.setup.account.proxy.port)
    report["loopback_proxy_workload_records"] = (
      phases.get("workload", {}).get("local:" + port, {}).get("records", 0)
    )
    return (
      consumer.get("case_ok") is False
      and server.get("proxy_name_is_loopback") is True
      and server.get("direct_control_connected") is True
      and len(posts) >= 1
      and all(
        row["upstream_status"] is None and row["client_status"] in [502, 503]
        for row in records
      )
      and report["direct_control_records"] > 0
      and report["direct_workload_records"] == 0
      and report["loopback_proxy_workload_records"] > 0
    )

  def outcome(
    self,
    consumer: dict[str, Any],
    server: dict[str, Any],
    records: list[dict[str, Any]],
    inference: list[dict[str, Any]],
    tunnels: list[dict[str, Any]],
  ) -> bool:
    """Judge the run by its expected outcome.

    upstream_rejects_dummy: same client and proxy path as the passing arm;
    only the token swap is absent. The upstream's 401 pauses the account, so
    the CLI's retries are refused locally with 503 and reach the upstream 0
    times. egress-forward: the client's request has to have gone through the
    gateway's forward proxy, and nowhere else.

    Args:
      consumer: The client's output.
      server: The gateway runner's output.
      records: All HTTP records.
      inference: The gateway's inference traffic rows.
      tunnels: The gateway's CONNECT traffic rows.

    Returns:
      The outcome.
    """
    setup = self.setup
    report = self.report
    proxy = setup.account.proxy
    posts = [
      row
      for row in records
      if row["method"] == "POST" and row["path"] == "/v1/messages"
    ]
    if setup.expected_outcome == "case_passes":
      return self.outcome_case_passes(
        consumer, server, posts, inference, tunnels
      )
    if setup.expected_outcome == "limits_observed":
      return self.outcome_limits(consumer, server, posts)
    if setup.expected_outcome == "upstream_rejects_dummy":
      return (
        consumer.get("case_ok") is False
        and len(posts) >= 1
        and posts[0]["upstream_reached"]
        and posts[0]["upstream_status"] == 401
        and all(
          not row["upstream_reached"] and row["client_status"] == 503
          for row in posts[1:]
        )
      )
    if setup.expected_outcome == "fails_without_direct":
      return self.outcome_proxy_down(consumer, server, posts, records)
    if setup.case == "egress-forward":
      probe = consumer.get("forward_egress", {})
      proxy_log = report.get("proxy_log") or []
      report["egress_matches_inventory"] = (
        probe.get("ip") == proxy.expected_egress_ip
      )
      return (
        probe.get("status") == 200
        and report["egress_matches_inventory"]
        and not posts
        and len(proxy_log) >= 1
        and all(
          row["target"] == "ip.oxylabs.io:443" and row["result"] == "ok"
          for row in proxy_log
        )
      )
    probe = server.get("egress", {}).get("authenticated", {})
    report["egress_matches_inventory"] = (
      probe.get("ip") == proxy.expected_egress_ip
    )
    return probe.get("status") == 200 and report["egress_matches_inventory"]

  def judge(self) -> None:
    """Judge hygiene and outcome and record both.

    Hygiene covers isolation, identity, exits, capture and cleanup; the
    outcome is judged per arm. A truncated side-traffic log is an incomplete
    capture.
    """
    report = self.report
    setup = self.setup
    consumer = report.get("consumer", {})
    server = report.get("gateway", {})
    parts = [self.base_hygiene(consumer, server)]
    workload = report if setup.case == "egress" else consumer
    # Every part runs: the readings they record must land even when an
    # earlier part fails.
    parts.append(self.capture_hygiene(workload))
    parts.append(server.get("proxy_log_overflow") is not True)
    self.summarize_traffic(workload)
    parts.append(
      report["capture_controls_seen"]
      and "traffic_summary_failure_kind" not in report
      and report.get("direct_free") is True
    )
    self.collect_records(server)
    records = report.get("http_records") or []
    parts.append(self.records_hygiene(server, records))
    traffic = server.get("traffic") or []
    inference = [row for row in traffic if row["kind"] == "inference"]
    tunnels = [row for row in traffic if row["kind"] == "connect"]
    parts.append(self.session_hygiene(server, consumer, inference))
    hygiene = all(parts)
    outcome = self.outcome(consumer, server, records, inference, tunnels)
    report["hygiene_ok"] = bool(hygiene)
    report["outcome_ok"] = bool(outcome)
    report["success"] = report["hygiene_ok"] and report["outcome_ok"]

  def finish(self) -> None:
    """Stop everything, count what is left, judge and write the result."""
    self.stop_processes()
    self.remove_objects()
    self.stop_capture()
    self.stop_names_capture()
    for handle in [self.names_file, self.packet, self.statistics]:
      if handle:
        handle.close()
    self.count_residue()
    self.judge()
    report = self.report
    (self.root / "result.json").write_text(json.dumps(report, indent=2) + "\n")
    print(
      json.dumps(
        {
          "result": str(self.root / "result.json"),
          "success": report["success"],
          "hygiene_ok": report["hygiene_ok"],
          "outcome_ok": report["outcome_ok"],
        }
      ),
      flush=True,
    )


def main(argv: list[str] | None = None) -> int:
  """Run the rehearsal.

  Args:
    argv: Command-line arguments; None reads sys.argv.

  Returns:
    0 when hygiene and the outcome both hold, else 1.
  """
  parser = build_parser()
  args = parser.parse_args(argv)
  validate_modes(parser, args)
  validate_limits_and_quota(parser, args)
  validate_cache(parser, args)
  setup = build_setup(parser, args)
  run = Run(setup, initial_report(setup))
  run.pin_inputs()
  signal.signal(signal.SIGTERM, interrupted)
  signal.signal(signal.SIGINT, interrupted)
  (setup.root / "owner.json").write_text(json.dumps(run.report))
  print(
    json.dumps(
      {
        "directory": str(setup.root),
        "case": setup.case,
        "real_mode": setup.real,
        "expected_outcome": setup.expected_outcome,
      }
    ),
    flush=True,
  )
  run.write_settings()
  try:
    run.execute()
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    # Values in SDK/process exceptions remain private; phase and type are
    # sufficient.
    run.report["failure_kind"] = type(error).__name__
  finally:
    run.finish()
  return 0 if run.report["success"] else 1


if __name__ == "__main__":
  sys.exit(main())
