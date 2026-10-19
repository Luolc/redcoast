"""Rehearse the Codex gateway end to end with the real Pi and Codex CLI.

A gateway container runs ``redcoast codex serve`` and, in a second container
that shares its network namespace, ``redcoast claude serve``, with a fake
upstream and fake exits (codex_fake.py) behind both. A client container runs
Pi and the Codex CLI, at the versions pinned below and checked against their
SHA-256, through ``redcoast-client pi`` and ``redcoast-client codex``, and the
stand-in client codex_probe.mjs for the cross-provider arms. Everything is
synthetic: the inventories, the ChatGPT login (a made-up JWT), the Claude
token, the proxy credentials, the session key and the machine credential.
Nothing reaches chatgpt.com, api.anthropic.com, 1Password or Tailscale; only
the two client packages are downloaded from the npm registry. Run
``python3 codex.py --gateway-binary <static redcoast> --launcher-binary
<static redcoast-client>``; the arms are listed in README.md next to this
file.
"""

import argparse
import base64
import datetime
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
from typing import Any
import urllib.request
import uuid

from xmachine import (
  credential_hash,
  leaking_argument,
  new_credential,
  redact,
  redact_value,
  terminated,
)

# Pinned by digest in a form docker can pull.
GATEWAY_IMAGE = (
  "python:3.13-slim"
  "@sha256:9d2e5553305c7c7b0097999bb17187c69b921ccd6bc9d40e4bb5ebe652c00285"
)
CLIENT_IMAGE = (
  "node:24-slim"
  "@sha256:a5f4366e834695d5f363b920c104953a535d3a9864d1bdf74566a0ebe01daee5"
)
# The npm tarballs of the two clients and their SHA-256. Pi's bundle needs only
# node; the Codex package for linux-x64 holds a static (musl) binary.
PI_URL = (
  "https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/"
  "pi-coding-agent-1.2.0.tgz"
)
PI_SHA256 = "1dfed308f46a69aec06bf9dc20700ddc6fe2370b1898f01334b7898397541e66"
CODEX_URL = "https://registry.npmjs.org/@openai/codex/-/codex-0.159.3-linux-x64.tgz"
CODEX_SHA256 = (
  "0719027cfe326e248971a2dff0a391c3532a3d838b4e3cc660bf93b2a606d2ce"
)
CODEX_MEMBER = "package/vendor/x86_64-unknown-linux-musl/bin/codex"
HERE = Path(__file__).resolve().parent
FAKE = HERE / "codex_fake.py"
PROBE = HERE / "codex_probe.mjs"
MODEL = "gpt-5.5"
REPLY = "redcoast-rehearsal-ok"

CODEX_ALIAS = "example-codex"
CLAUDE_ALIAS = "example-claude"
# The account's installation ID, from its inventory file, and the one the
# client's CODEX_HOME holds; both lower-case UUID v4 of the same length.
ACCOUNT_INSTALLATION = "0b8c7e2a-3f41-4d2e-9a6b-5c1d2e3f4a5b"
CLIENT_INSTALLATION = "7d6c5b4a-3e2f-4a1b-8c9d-0e1f2a3b4c5d"
ACCOUNT_ID = "synthetic-chatgpt-account"
# The account ID inside the made-up JWT of the direct control arms.
DIRECT_ACCOUNT_ID = "direct-control"
# The references are only keys into the synthetic credentials each gateway
# reads on stdin; no vault of these names exists.
SESSION_KEY_REF = "op://example-vault/example-session-key/credential"
CLAUDE_TOKEN_REF = "op://example-vault/example-claude-token/credential"
USERNAME_REF = "op://example-vault/example-proxy/username"
PASSWORD_REF = "op://example-vault/example-proxy/password"
# Ports: the Claude gateway's session, reverse, forward and health listeners,
# then the Codex gateway's; the fake exits of the two accounts; the fake
# upstream's direct port.
CLAUDE_PORTS = (7801, 7802, 7803, 7804)
CODEX_PORTS = (7811, 7812, 7813, 7814)
CLAUDE_EXIT, CODEX_EXIT, DIRECT_PORT = 18798, 18799, 18791
# The exit IP echo of the gateway's egress check also goes through the exits;
# its CONNECTs are not traffic of any arm.
EGRESS_ECHO = "ip.oxylabs.io:443"


def proxy_yaml(port: int) -> str:
  """Return the proxy section of an account file.

  Args:
    port: The fake exit's port.

  Returns:
    The YAML lines.
  """
  return (
    "proxy:\n"
    "  host: 127.0.0.1\n"
    f"  port: {port}\n"
    "  expected_egress_ip: 192.0.2.1\n"
    f"  username_ref: {USERNAME_REF}\n"
    f"  password_ref: {PASSWORD_REF}\n"
  )


CODEX_INVENTORY = (
  f"email: {CODEX_ALIAS}@example.test\n"
  "access: gateway\n"
  "status: active\n"
  f"installation_id: {ACCOUNT_INSTALLATION}\n"
) + proxy_yaml(CODEX_EXIT)
CLAUDE_INVENTORY = (
  f"id: {CLAUDE_ALIAS}\n"
  f"email: {CLAUDE_ALIAS}@example.test\n"
  "status: active\n"
  "access: gateway\n"
  f"oauth_token: {CLAUDE_TOKEN_REF}\n"
) + proxy_yaml(CLAUDE_EXIT)


def fake_jwt(claims: dict[str, Any]) -> str:
  """Return an unsigned-looking JWT with the given claims.

  The signature is random bytes: nothing verifies it, the clients only read
  the claims.

  Args:
    claims: The payload.

  Returns:
    The token.
  """

  def part(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

  header = part(json.dumps({"alg": "RS256", "typ": "JWT"}).encode())
  payload = part(json.dumps(claims).encode())
  return f"{header}.{payload}.{part(secrets.token_bytes(32))}"


def login_claims(email: str, account_id: str) -> dict[str, Any]:
  """Return the claims of a made-up ChatGPT token.

  Args:
    email: The login's email.
    account_id: Its ChatGPT account ID.

  Returns:
    The claims, with the email and the account ID where ChatGPT puts them.
  """
  return {
    "email": email,
    "https://api.openai.com/auth": {"chatgpt_account_id": account_id},
    "exp": 4102444800,
  }


def download(url: str, sha256: str, path: Path, cached: Path | None) -> None:
  """Fetch a tarball, or copy a cached one, and check its SHA-256.

  Args:
    url: Where to fetch it.
    sha256: Its pinned SHA-256.
    path: Where to put it.
    cached: A copy fetched earlier, used instead of the network.

  Raises:
    RuntimeError: When the file's SHA-256 is not the pinned one.
  """
  if cached is not None:
    shutil.copyfile(cached, path)
  else:
    with urllib.request.urlopen(url, timeout=120) as response:
      with path.open("wb") as out:
        shutil.copyfileobj(response, out)
  digest = hashlib.sha256(path.read_bytes()).hexdigest()
  if digest != sha256:
    raise RuntimeError(f"{path.name}: SHA-256 {digest}, pinned {sha256}")


def new_records(before: list[dict], after: list[dict]) -> list[dict]:
  """Return the records added since a snapshot, without egress checks.

  Args:
    before: The records at the snapshot.
    after: The records now.

  Returns:
    The records after the snapshot's length, less the CONNECTs of the
    gateways' egress check.
  """
  return [
    entry
    for entry in after[len(before) :]
    if not (entry["kind"] == "connect" and entry["target"] == EGRESS_ECHO)
  ]


def summarize(records: list[dict]) -> dict[str, int]:
  """Count records by where they arrived.

  Args:
    records: Records of the fake upstream and exits.

  Returns:
    The number of upstream requests and of CONNECTs at the exits.
  """
  return {
    "upstream_requests": sum(1 for r in records if r["kind"] == "request"),
    "exit_connects": sum(1 for r in records if r["kind"] == "connect"),
  }


class Run:
  """One rehearsal: its names, addresses, files, secrets and report.

  Attributes:
    gateway: The static redcoast binary.
    launcher: The static redcoast-client binary.
    root: The run directory, private to this run.
    subnet: The Docker network's subnet.
    keep: Whether a failed run's containers are left for inspection.
    secret_values: Values that must never reach a command line, an error or
      the report: the account, proxy and Claude tokens, the session key, the
      machine credential and, once issued, the session credentials.
  """

  def __init__(
    self, gateway: Path, launcher: Path, root: Path, subnet: str, keep: bool
  ) -> None:
    """Derive the names and addresses.

    Args:
      gateway: The static redcoast binary.
      launcher: The static redcoast-client binary.
      root: The run directory.
      subnet: The Docker network's subnet.
      keep: Whether a failed run's containers stay.
    """
    self.gateway, self.launcher, self.root = gateway, launcher, root
    self.subnet, self.keep = subnet, keep
    self.name = "codexrh-" + uuid.uuid4().hex[:8]
    self.label = "gateway.e2e.run=" + self.name
    base = subnet.rsplit(".", 1)[0]
    self.gw_ip, self.client_ip = base + ".10", base + ".11"
    self.secret_values: list[str] = []
    self.report: dict[str, Any] = {
      "run": self.name,
      "directory": str(root),
      "started_utc": datetime.datetime.now(datetime.UTC).isoformat(),
      "gateway_binary_sha256": hashlib.sha256(gateway.read_bytes()).hexdigest(),
      "launcher_binary_sha256": hashlib.sha256(
        launcher.read_bytes()
      ).hexdigest(),
      "arms": {},
      "cleanup": {},
    }

  def sh(
    self,
    command: list[str],
    label: str,
    check: bool = True,
    timeout: int = 60,
  ) -> subprocess.CompletedProcess[str]:
    """Run a command; errors name only the label and the exit.

    Args:
      command: The command line.
      label: What the command does, for errors and the report.
      check: Whether a non-zero exit is an error.
      timeout: Seconds before the command is killed.

    Returns:
      The completed process.

    Raises:
      RuntimeError: When an argument holds a secret value, the command timed
        out, or it failed with check set.
    """
    if leaking_argument(command, self.secret_values):
      raise RuntimeError(
        label + ": a secret value was about to go into a command line"
      )
    try:
      result = subprocess.run(
        command, capture_output=True, text=True, timeout=timeout
      )
    except subprocess.TimeoutExpired:
      raise RuntimeError(f"{label} timed out after {timeout}s") from None
    if check and result.returncode != 0:
      self.report.setdefault("failures", []).append(
        {
          "label": label,
          "exit": result.returncode,
          "stderr_tail": redact(
            result.stderr.strip()[-400:], self.secret_values
          ),
        }
      )
      raise RuntimeError(f"{label} failed with exit {result.returncode}")
    return result

  def record(
    self, arm: str, want: Any, got: Any, extra: dict[str, Any] | None = None
  ) -> None:
    """Record one arm's expected and actual readings and print the verdict.

    Args:
      arm: The arm's name.
      want: The expected reading.
      got: The actual reading.
      extra: More fields for the report, such as a client's output tail.
    """
    ok = want == got
    got = redact_value(got, self.secret_values)
    fields: dict[str, Any] = redact_value(extra or {}, self.secret_values)
    self.report["arms"][arm] = {"want": want, "got": got, "ok": ok, **fields}
    print(("PASS " if ok else "FAIL ") + arm + ": " + json.dumps(got))

  def write_files(self, pi_tarball: Path | None, codex_tarball: Path | None) -> None:
    """Fetch and unpack the clients and write the run's files.

    Args:
      pi_tarball: A copy of Pi's tarball fetched earlier, or None.
      codex_tarball: A copy of the Codex tarball fetched earlier, or None.
    """
    pi_tgz, codex_tgz = self.root / "pi.tgz", self.root / "codex.tgz"
    download(PI_URL, PI_SHA256, pi_tgz, pi_tarball)
    download(CODEX_URL, CODEX_SHA256, codex_tgz, codex_tarball)
    with tarfile.open(pi_tgz) as archive:
      archive.extractall(self.root / "pi", filter="data")
    with tarfile.open(codex_tgz) as archive:
      archive.extract(CODEX_MEMBER, self.root / "codex", filter="data")
    self.pi = self.root / "pi" / "package"
    self.codex = self.root / "codex" / CODEX_MEMBER

    for name, alias, text in [
      ("codex-inventory", CODEX_ALIAS, CODEX_INVENTORY),
      ("claude-inventory", CLAUDE_ALIAS, CLAUDE_INVENTORY),
    ]:
      (self.root / name).mkdir()
      (self.root / name / (alias + ".yaml")).write_text(text)
    self.access_token = fake_jwt(
      login_claims(CODEX_ALIAS + "@example.test", ACCOUNT_ID)
    )
    self.refresh_token = "synthetic-refresh-" + secrets.token_hex(16)
    login = {
      "OPENAI_API_KEY": None,
      "tokens": {
        "id_token": fake_jwt(
          login_claims(CODEX_ALIAS + "@example.test", ACCOUNT_ID)
        ),
        "access_token": self.access_token,
        "refresh_token": self.refresh_token,
        "account_id": ACCOUNT_ID,
      },
      "last_refresh": "2026-10-19T00:00:00Z",
    }
    account_dir = self.root / "accounts" / CODEX_ALIAS
    account_dir.mkdir(parents=True)
    os.chmod(account_dir, 0o700)
    (account_dir / "auth.json").write_text(json.dumps(login))
    os.chmod(account_dir / "auth.json", 0o600)

    self.claude_token = "sk-ant-oat01-synthetic-" + secrets.token_hex(8)
    proxy_password = "Synthetic-" + secrets.token_hex(4)
    session_key = "synthetic-session-key-" + secrets.token_hex(16)
    common = {USERNAME_REF: "synthetic-user", PASSWORD_REF: proxy_password}
    for name, values in [
      ("codex-creds.json", {**common, SESSION_KEY_REF: session_key}),
      ("claude-creds.json", {**common, CLAUDE_TOKEN_REF: self.claude_token}),
    ]:
      (self.root / name).write_text(json.dumps(values))
      os.chmod(self.root / name, 0o600)
    (self.root / "cred").write_text(new_credential() + "\n")
    os.chmod(self.root / "cred", 0o600)
    self.machine_hash = credential_hash((self.root / "cred").read_text().strip())
    self.secret_values += [
      self.access_token,
      self.refresh_token,
      login["tokens"]["id_token"],
      self.claude_token,
      proxy_password,
      session_key,
      (self.root / "cred").read_text().strip(),
    ]
    # The made-up bearer of the direct control arms; no gateway issued it.
    self.direct_token = fake_jwt(
      login_claims("direct@example.test", DIRECT_ACCOUNT_ID)
    )

    for provider, ports, exit_port in [
      ("codex", CODEX_PORTS, CODEX_EXIT),
      ("claude", CLAUDE_PORTS, CLAUDE_EXIT),
    ]:
      session, reverse, forward, health = ports
      upstream = "http://127.0.0.1:18790" + (
        "/backend-api/codex" if provider == "codex" else ""
      )
      text = (
        f"upstream: {upstream}\n"
        "listen:\n"
        f"  reverse: {self.gw_ip}:{reverse}\n"
        f"  forward: {self.gw_ip}:{forward}\n"
        f"  session: {self.gw_ip}:{session}\n"
        f"  health: {self.gw_ip}:{health}\n"
        f"session_socket: /state/{provider}.sock\n"
        f"admin_socket: /state/{provider}-admin.sock\n"
        f"session_db: /state/{provider}.sqlite\n"
        f"inventory: /{provider}-inventory\n"
        "credentials:\n"
        "  stdin_test_entry: true\n"
      )
      if provider == "codex":
        text += (
          "codex:\n"
          "  accounts_dir: /accounts\n"
          f"  session_key: {SESSION_KEY_REF}\n"
        )
      (self.root / f"{provider}.yaml").write_text(text)

  def common(self) -> list[str]:
    """Return the docker run options every container of the run shares.

    Returns:
      The options.
    """
    uid = f"{os.getuid()}:{os.getgid()}"
    return [
      "--label", self.label, "--user", uid, "--read-only", "--cap-drop", "ALL",
      "--security-opt", "no-new-privileges", "--cpus", "1", "--memory", "512m",
      "--pids-limit", "256", "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m",
    ]  # fmt: skip

  def admin(
    self, provider: str, *command: str, check: bool = True
  ) -> subprocess.CompletedProcess[str]:
    """Run a management subcommand of one gateway inside its container.

    Args:
      provider: codex or claude.
      *command: The subcommand and its arguments.
      check: Whether a non-zero exit is an error.

    Returns:
      The completed process.
    """
    return self.sh(
      ["docker", "exec", f"{self.name}-{provider}", "/redcoast", provider,
       *command, "--admin-socket", f"/state/{provider}-admin.sock"],
      f"{provider} {command[0]}",
      check=check,
    )  # fmt: skip

  def start_gateways(self) -> None:
    """Start the network, both gateways and the fakes, and register the client.

    Raises:
      RuntimeError: When a gateway does not come up in time.
    """
    self.sh(
      # Internal: no container of the run can reach anything outside it.
      ["docker", "network", "create", "--internal", "--label", self.label,
       "--subnet", self.subnet, self.name],
      "create network",
    )  # fmt: skip
    uid = os.getuid()
    mounts = {
      "codex": [
        f"{self.root / 'codex-inventory'}:/codex-inventory:ro",
        f"{self.root / 'accounts'}:/accounts:ro",
        f"{FAKE}:/codex_fake.py:ro",
      ],
      "claude": [f"{self.root / 'claude-inventory'}:/claude-inventory:ro"],
    }
    for provider in ["codex", "claude"]:
      if provider == "codex":
        network = ["--network", self.name, "--ip", self.gw_ip]
        entry = (
          f"python3 /codex_fake.py /state/fake.jsonl {self.gw_ip} &"
          " exec /redcoast codex serve --config /codex.yaml < /codex-creds.json"
        )
      else:
        network = ["--network", f"container:{self.name}-codex"]
        entry = (
          "exec /redcoast claude serve --config /claude.yaml"
          " < /claude-creds.json"
        )
      command = [
        "docker", "run", "-d", "--name", f"{self.name}-{provider}", *network,
        *self.common(),
        "--tmpfs", f"/state:rw,nosuid,nodev,size=64m,uid={uid}",
        "-v", f"{self.gateway}:/redcoast:ro",
        "-v", f"{self.root / (provider + '.yaml')}:/{provider}.yaml:ro",
        "-v", f"{self.root / (provider + '-creds.json')}:/{provider}-creds.json:ro",
      ]  # fmt: skip
      for mount in mounts[provider]:
        command += ["-v", mount]
      self.sh(
        command + [GATEWAY_IMAGE, "sh", "-c", entry], "start " + provider
      )
      for _ in range(100):
        if self.admin(provider, "status", check=False).returncode == 0:
          break
        time.sleep(0.2)
      else:
        logs = self.sh(
          ["docker", "logs", f"{self.name}-{provider}"], "log", check=False
        )
        self.report[provider + "_log_tail"] = redact(
          logs.stderr[-600:], self.secret_values
        )
        raise RuntimeError(provider + " gateway did not come up")
      alias = CODEX_ALIAS if provider == "codex" else CLAUDE_ALIAS
      self.sh(
        ["docker", "exec", f"{self.name}-{provider}", "/redcoast", provider,
         "plan", "--session-db", f"/state/{provider}.sqlite", "schedule",
         alias, "pro", "2026-01-01T00:00:00Z"],
        provider + " plan",
      )  # fmt: skip
      self.admin(
        provider, "machine", "add", "client-a", self.machine_hash,
        "--max-sessions", "8",
      )  # fmt: skip

  def start_client(self) -> None:
    """Start the client container, which idles until the arms exec in it."""
    uid = os.getuid()
    # CODEX_HOME's installation ID, which Codex reads instead of making one;
    # a directory others cannot enter, as the launcher requires.
    setup = (
      "mkdir -p -m 0700 /client/home/.config/codex /client/direct-codex"
      " /client/direct-pi /client/work"
      f" && printf %s {CLIENT_INSTALLATION}"
      " | tee /client/home/.config/codex/installation_id"
      " > /client/direct-codex/installation_id"
      " && printf '{\"hasCompletedOnboarding\": true}'"
      " > /client/home/.claude.json"
      " && exec sleep 1800"
    )
    self.sh(
      ["docker", "run", "-d", "--name", f"{self.name}-client",
       "--network", self.name, "--ip", self.client_ip, *self.common(),
       "--tmpfs", f"/client:rw,exec,nosuid,nodev,size=64m,uid={uid}",
       "-v", f"{self.launcher}:/redcoast-client:ro",
       "-v", f"{self.pi}:/opt/pi:ro",
       "-v", f"{self.codex}:/opt/codex/codex:ro",
       "-v", f"{PROBE}:/probe.mjs:ro",
       "-v", f"{self.root / 'cred'}:/cred:ro",
       CLIENT_IMAGE, "sh", "-c", setup],
      "start client",
    )  # fmt: skip
    for _ in range(50):
      if (
        self.client_exec(["test", "-f", "/client/home/.claude.json"], "ready")
      ).returncode == 0:
        return
      time.sleep(0.2)
    raise RuntimeError("client container did not come up")

  def client_env(self) -> dict[str, str]:
    """Return the environment of a launcher run in the client container.

    Returns:
      The variables.
    """
    return {
      "PATH": "/usr/local/bin:/usr/bin:/bin",
      "HOME": "/client/home",
      "REDCOAST_CLIENT_ADDRESS": f"{self.gw_ip}:{CLAUDE_PORTS[0]}",
      "REDCOAST_CLIENT_CREDENTIAL_FILE": "/cred",
      "REDCOAST_CLIENT_CODEX_ADDRESS": f"{self.gw_ip}:{CODEX_PORTS[0]}",
      "REDCOAST_CLIENT_CODEX_CREDENTIAL_FILE": "/cred",
      "REDCOAST_CLIENT_PI": "/opt/pi/dist/bundle/cli.js",
      "REDCOAST_CLIENT_CODEX": "/opt/codex/codex",
      "REDCOAST_CLIENT_CLAUDE": "/probe.mjs",
    }

  def client_exec(
    self,
    command: list[str],
    label: str,
    env: dict[str, str] | None = None,
    detach: bool = False,
    timeout: int = 60,
  ) -> subprocess.CompletedProcess[str]:
    """Run a command in the client container.

    Args:
      command: The command.
      label: What it does.
      env: Its environment variables.
      detach: Whether it runs in the background (``-d``).
      timeout: Seconds before it is killed.

    Returns:
      The completed ``docker exec``.
    """
    options = ["-d"] if detach else []
    for key, value in (env or {}).items():
      options += ["-e", f"{key}={value}"]
    return self.sh(
      ["docker", "exec", *options, "-w", "/client/work",
       f"{self.name}-client", *command],
      label, check=False, timeout=timeout,
    )  # fmt: skip

  def fake_records(self) -> list[dict]:
    """Read the fake upstream's and exits' records.

    Returns:
      The records, oldest first.
    """
    read = self.sh(
      ["docker", "exec", f"{self.name}-codex", "sh", "-c",
       "cat /state/fake.jsonl 2>/dev/null || true"],
      "read fake records",
    )  # fmt: skip
    return [json.loads(line) for line in read.stdout.splitlines()]

  def client_run(
    self, arm: str, command: list[str], env: dict[str, str]
  ) -> tuple[subprocess.CompletedProcess[str], list[dict]]:
    """Run one client to its end and return what reached the fakes meanwhile.

    Args:
      arm: The arm, for the label.
      command: The client's command line.
      env: Its environment.

    Returns:
      The completed client and the new records.
    """
    before = self.fake_records()
    result = self.client_exec(command, arm, env=env, timeout=180)
    return result, new_records(before, self.fake_records())

  def tail(self, result: subprocess.CompletedProcess[str]) -> dict[str, str]:
    """Return the ends of a client's output for the report.

    Args:
      result: The completed client.

    Returns:
      The last characters of its stdout and stderr.
    """
    return {"stdout_tail": result.stdout[-300:], "stderr_tail": result.stderr[-600:]}

  def responses(self, records: list[dict], port: int) -> list[dict]:
    """Return the upstream's inference requests among records.

    Args:
      records: New records.
      port: The upstream port they arrived on: 18790 through the gateway's
        exit, DIRECT_PORT directly.

    Returns:
      The POSTs to .../responses that arrived on port.
    """
    return [
      r
      for r in records
      if r["kind"] == "request"
      and r["method"] == "POST"
      and r["port"] == port
      and r["path"].endswith("/responses")
    ]

  def through_gateway(self, requests: list[dict]) -> dict[str, Any]:
    """Judge what the upstream received through the gateway.

    Args:
      requests: The inference requests.

    Returns:
      Whether every one carried the account's token and account ID.
    """
    token = hashlib.sha256(f"Bearer {self.access_token}".encode()).hexdigest()
    return {
      "requests": len(requests) > 0,
      "account_token": all(r["authorization_sha256"] == token for r in requests),
      "account_id": sorted({r["chatgpt_account_id"] for r in requests}),
    }

  def pi_arms(self) -> None:
    """Pi straight to the fake upstream, then through the gateway."""
    models = json.dumps({
      "providers": {"openai-codex": {
        "baseUrl": f"http://{self.gw_ip}:{DIRECT_PORT}/backend-api",
        "apiKey": "${DIRECT_TOKEN}",
      }},
    })  # fmt: skip
    self.client_exec(
      ["sh", "-c", 'printf %s "$MODELS" > /client/direct-pi/models.json'],
      "write direct models.json", env={"MODELS": models},
    )  # fmt: skip
    pi_args = ["--provider", "openai-codex", "--model", MODEL, "--no-session"]
    result, records = self.client_run(
      "pi_direct_control",
      ["/opt/pi/dist/bundle/cli.js", *pi_args, "-p", "Hi"],
      {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/client/home",
       "PI_CODING_AGENT_DIR": "/client/direct-pi", "PI_OFFLINE": "1",
       "DIRECT_TOKEN": self.direct_token},
    )  # fmt: skip
    direct = self.responses(records, DIRECT_PORT)
    self.record(
      "pi_direct_control",
      {"exit": 0, "reply": True, "requests": 1,
       "account_id": [DIRECT_ACCOUNT_ID]},
      {"exit": result.returncode, "reply": REPLY in result.stdout,
       "requests": len(direct),
       "account_id": sorted({r["chatgpt_account_id"] for r in direct})},
      self.tail(result),
    )  # fmt: skip

    result, records = self.client_run(
      "pi_through_gateway",
      ["/redcoast-client", "pi", *pi_args, "-p", "Hi"],
      self.client_env(),
    )
    self.record(
      "pi_through_gateway",
      {"exit": 0, "reply": True, "requests": True, "account_token": True,
       "account_id": [ACCOUNT_ID], "direct_requests": 0},
      {"exit": result.returncode, "reply": REPLY in result.stdout,
       **self.through_gateway(self.responses(records, 18790)),
       "direct_requests": len(self.responses(records, DIRECT_PORT))},
      self.tail(result),
    )  # fmt: skip

  def codex_arms(self) -> None:
    """Codex CLI straight to the fake upstream, then through the gateway."""
    exec_args = [
      "exec", "--skip-git-repo-check", "--ephemeral", "--color", "never",
      "-m", MODEL, "Hi",
    ]  # fmt: skip
    provider = [
      "-c", 'model_provider="direct"',
      "-c", 'model_providers.direct.name="direct"',
      "-c", 'model_providers.direct.base_url='
      f'"http://{self.gw_ip}:{DIRECT_PORT}/backend-api/codex"',
      "-c", 'model_providers.direct.wire_api="responses"',
      "-c", 'model_providers.direct.env_key="DIRECT_TOKEN"',
      "-c", "model_providers.direct.supports_websockets=false",
    ]  # fmt: skip
    result, records = self.client_run(
      "codex_direct_control",
      ["/opt/codex/codex", "--no-daemon", *provider, *exec_args],
      {"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/client/home",
       "CODEX_HOME": "/client/direct-codex",
       "DIRECT_TOKEN": self.direct_token},
    )  # fmt: skip
    direct = self.responses(records, DIRECT_PORT)
    self.record(
      "codex_direct_control",
      {"exit": 0, "reply": True, "requests": True,
       "installation_ids": [CLIENT_INSTALLATION]},
      {"exit": result.returncode, "reply": REPLY in result.stdout,
       "requests": len(direct) > 0,
       "installation_ids": sorted(
         {v for r in direct for v in r["installation_ids"].values()}
       )},
      self.tail(result),
    )  # fmt: skip

    result, records = self.client_run(
      "codex_through_gateway",
      ["/redcoast-client", "codex", *exec_args],
      self.client_env(),
    )
    sent = self.responses(records, 18790)
    kept = self.client_exec(
      ["cat", "/client/home/.config/codex/installation_id"],
      "read client installation_id",
    )
    self.record(
      "codex_through_gateway",
      {"exit": 0, "reply": True, "requests": True, "account_token": True,
       "account_id": [ACCOUNT_ID], "direct_requests": 0},
      {"exit": result.returncode, "reply": REPLY in result.stdout,
       **self.through_gateway(sent),
       "direct_requests": len(self.responses(records, DIRECT_PORT))},
      self.tail(result),
    )  # fmt: skip
    self.record(
      "codex_installation_id_rewritten",
      {"header": [ACCOUNT_INSTALLATION], "body": [ACCOUNT_INSTALLATION],
       "body_turn_metadata": [ACCOUNT_INSTALLATION],
       "client_still_holds": CLIENT_INSTALLATION},
      {**{place: sorted({r["installation_ids"][place] for r in sent}, key=str)
          for place in ["header", "body", "body_turn_metadata"]},
       "client_still_holds": kept.stdout.strip()},
    )  # fmt: skip
    status = json.loads(self.admin("codex", "status").stdout)
    quota = {
      account["alias"]: account["quota"].get("5h", {}).get("utilization")
      for account in status["accounts"]
    }
    self.record(
      "codex_quota_read_without_content_type", {CODEX_ALIAS: 0.12}, quota
    )

  def cross_provider_arms(self) -> None:
    """Each gateway's session credential at the other gateway, with controls.

    Two stand-in clients hold a Claude and a Codex session from the client's
    address; the probes run there too, since a session credential only
    counts from the address it was issued to.
    """
    for provider in ["claude", "codex"]:
      self.client_exec(
        ["/redcoast-client", provider],
        "hold " + provider,
        env={**self.client_env(), "REDCOAST_CLIENT_CODEX": "/probe.mjs",
             "PROBE_HOLD": provider},
        detach=True,
      )  # fmt: skip
    for provider in ["claude", "codex"]:
      for _ in range(100):
        read = self.client_exec(
          ["cat", f"/client/out/{provider}.token"], "read " + provider
        )
        if read.returncode == 0 and read.stdout.strip():
          self.secret_values.append(read.stdout.strip())
          break
        time.sleep(0.2)
      else:
        raise RuntimeError(provider + " stand-in got no session")
    targets = {
      "codex": (CODEX_PORTS, "/backend-api/codex/responses"),
      "claude": (CLAUDE_PORTS, "/v1/messages"),
    }
    for credential, gateway, want in [
      ("claude", "codex", "post=401 connect=407"),
      ("codex", "claude", "post=401 connect=407"),
      ("codex", "codex", "post=200 connect=200"),
      ("claude", "claude", "post=200 connect=200"),
    ]:
      ports, path = targets[gateway]
      before = self.fake_records()
      probe = self.client_exec(
        ["sh", "-c",
         f"node /probe.mjs probe http://{self.gw_ip}:{ports[1]} {path}"
         f" {self.gw_ip}:{ports[2]} < /client/out/{credential}.token"],
        f"{credential} credential at {gateway}",
      )  # fmt: skip
      reached = summarize(new_records(before, self.fake_records()))
      negative = credential != gateway
      arm = f"{credential}_credential_at_{gateway}" + (
        "" if negative else "_control"
      )
      self.record(
        arm,
        {"probe": want, "upstream_requests": 0 if negative else 1,
         "exit_connects": 0 if negative else 1},
        {"probe": probe.stdout.strip(), **reached},
      )  # fmt: skip

  def hygiene_arms(self) -> None:
    """Check the network, what the upstream saw and what the logs hold."""
    internal = self.sh(
      ["docker", "network", "inspect", "--format", "{{.Internal}}", self.name],
      "inspect network",
    )
    lookup = self.client_exec(
      ["node", "-e", "require('dns').lookup('registry.npmjs.org',"
       " (e) => console.log(e ? 'unresolved' : 'resolved'))"],
      "resolve an outside name",
    )  # fmt: skip
    self.record(
      "network_has_no_way_out",
      {"internal": "true", "outside_name": "unresolved"},
      {"internal": internal.stdout.strip(),
       "outside_name": lookup.stdout.strip()},
    )  # fmt: skip
    allowed = {
      hashlib.sha256(f"Bearer {token}".encode()).hexdigest()
      for token in [self.access_token, self.claude_token, self.direct_token]
    }
    seen = [r for r in self.fake_records() if r["kind"] == "request"]
    self.record(
      "upstream_saw_only_account_credentials",
      {"requests": True, "other_credentials": 0},
      {"requests": len(seen) > 0,
       "other_credentials": sum(
         1 for r in seen
         if r["authorization_sha256"] not in allowed or r["x_api_key_sha256"]
       )},
    )  # fmt: skip
    for provider, alias in [("codex", CODEX_ALIAS), ("claude", CLAUDE_ALIAS)]:
      logs = self.sh(
        ["docker", "logs", f"{self.name}-{provider}"], "log", check=False
      )
      text = logs.stdout + logs.stderr
      self.record(
        f"no_credential_in_{provider}_gateway_log",
        {"log_read": True, "hits": 0},
        {"log_read": logs.returncode == 0 and f"accounts: {alias}" in text,
         "hits": sum(1 for v in self.secret_values if v and v in text)},
        {"log_lines": len(text.splitlines())},
      )  # fmt: skip

  def cleanup(self) -> bool:
    """Remove the run's containers and network and count what is left, twice.

    Returns:
      True when nothing of the run remains at either count.
    """
    for suffix in ["-client", "-claude", "-codex"]:
      self.sh(
        ["docker", "rm", "-f", self.name + suffix],
        "remove " + self.name + suffix,
        check=False,
      )
    self.sh(
      ["docker", "network", "rm", self.name], "remove network", check=False
    )

    def residue() -> int:
      containers = self.sh(
        ["docker", "ps", "-aq", "--filter", "label=" + self.label],
        "count containers",
      ).stdout.split()
      networks = self.sh(
        ["docker", "network", "ls", "-q", "--filter", "label=" + self.label],
        "count networks",
      ).stdout.split()
      return len(containers) + len(networks)

    first = residue()
    time.sleep(5)
    second = residue()
    self.report["cleanup"] = {"remaining": first, "remaining_after_5s": second}
    print(f"cleanup remaining={first} remaining_after_5s={second}")
    return first == 0 and second == 0


def main(argv: list[str] | None = None) -> int:
  """Run the rehearsal and write its report.

  Args:
    argv: Command-line arguments; None reads sys.argv.

  Returns:
    0 when every arm passed and the cleanup left nothing, else 1.
  """
  parser = argparse.ArgumentParser(
    description="Codex gateway rehearsal with the real Pi and Codex CLI."
  )
  add = parser.add_argument
  add("--gateway-binary", type=Path, required=True, help="static redcoast")
  add("--launcher-binary", type=Path, required=True, help="static redcoast-client")
  add(
    "--out",
    type=Path,
    help="directory for the run's files and report; it must not exist yet"
    " (default: a new temporary directory)",
  )
  add(
    "--pi-tarball",
    type=Path,
    help="Pi's npm tarball fetched earlier, used instead of the network; its"
    " SHA-256 is checked all the same",
  )
  add("--codex-tarball", type=Path, help="the same for the Codex CLI")
  add("--subnet", default="172.30.78.0/24", help="Docker network subnet")
  add("--keep", action="store_true", help="leave a failed run's containers")
  args = parser.parse_args(argv)
  if args.out is None:
    root = Path(tempfile.mkdtemp(prefix="codexrh-"))
  else:
    root = args.out.resolve()
    root.mkdir(mode=0o700, parents=True)
  run = Run(
    gateway=args.gateway_binary.resolve(strict=True),
    launcher=args.launcher_binary.resolve(strict=True),
    root=root,
    subnet=args.subnet,
    keep=args.keep,
  )
  signal.signal(signal.SIGTERM, terminated)
  failed = False
  clean = False
  try:
    run.write_files(args.pi_tarball, args.codex_tarball)
    run.start_gateways()
    run.start_client()
    run.pi_arms()
    run.codex_arms()
    run.cross_provider_arms()
    run.hygiene_arms()
  except BaseException as error:  # noqa: BLE001 - a SIGTERM or Ctrl-C ends the run through the cleanup as well
    failed = True
    run.report["error"] = redact(
      f"{type(error).__name__}: {error}", run.secret_values
    )
    print("ERROR " + run.report["error"])
  finally:
    passed = all(arm["ok"] for arm in run.report["arms"].values())
    if run.keep and (failed or not passed):
      print(
        "kept containers for inspection; remove them with: docker rm -f"
        f" $(docker ps -aq --filter label={run.label});"
        f" docker network rm {run.name}"
      )
    else:
      clean = run.cleanup()
  run.report["finished_utc"] = datetime.datetime.now(datetime.UTC).isoformat()
  run.report["outcome"] = (not failed) and passed and clean
  (run.root / "report.json").write_text(json.dumps(run.report, indent=2) + "\n")
  print(f"report {run.root / 'report.json'} outcome={run.report['outcome']}")
  return 0 if run.report["outcome"] else 1


if __name__ == "__main__":
  sys.exit(main())
