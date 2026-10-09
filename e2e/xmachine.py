"""Rehearse the cross-machine session interface with two simulated clients.

A gateway container and two client containers share one Docker network, each
client with its own network namespace and address. Everything is synthetic:
the account inventory, the machine credentials (generated here as a client
machine would, only their SHA-256 reaches the gateway), the upstream and the
exit proxy. Nothing reaches 1Password, api.anthropic.com or Tailscale. Run
``python3 xmachine.py --gateway-binary <static redcoast>
--launcher-binary <static redcoast-client>``; the arms are listed in README.md
next to this file.
"""

import argparse
import base64
from dataclasses import dataclass, field
import datetime
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import tempfile
import time
from typing import Any
import uuid

GATEWAY_IMAGE = "python:3.13-slim"
CLIENT_IMAGE = "alpine:3.20"
PROBE = Path(__file__).with_name("xmachine_probe.sh")
# The line the gateway writes at startup for the synthetic account; the zero-hit
# arm requires it before a log counts as read.
ALIAS = "example-a"
STARTUP_LINE = "accounts: " + ALIAS
# The references are only keys into the synthetic credentials the gateway reads
# on stdin; no vault of these names exists.
TOKEN_REF = "op://example-vault/example-token/credential"
USERNAME_REF = "op://example-vault/example-proxy/username"
PASSWORD_REF = "op://example-vault/example-proxy/password"

INVENTORY = f"""id: {ALIAS}
email: {ALIAS}@example.test
status: active
access: gateway
oauth_token: {TOKEN_REF}
proxy:
  host: 127.0.0.1
  port: 18799
  expected_egress_ip: 192.0.2.1
  username_ref: {USERNAME_REF}
  password_ref: {PASSWORD_REF}
"""

# Fake upstream (inference answers 200) and fake exit (every CONNECT answers
# 200, then echoes; plain requests are relayed to the upstream, as the account's
# inference goes through its exit) in one process inside the gateway container.
UPSTREAM = r"""
import socket, threading
def serve(port, handler):
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(('127.0.0.1', port)); s.listen(16)
    while True:
        c, _ = s.accept()
        threading.Thread(target=handler, args=(c,), daemon=True).start()
def read_head(c):
    data = b''
    while b'\r\n\r\n' not in data:
        chunk = c.recv(4096)
        if not chunk: break
        data += chunk
    return data
def upstream(c):
    head = read_head(c)
    body = (b'{"id":"msg_synthetic","type":"message",'
            b'"content":[{"type":"text","text":"hi"}]}')
    c.sendall(b'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n'
              b'Content-Length: ' + str(len(body)).encode()
              + b'\r\nConnection: close\r\n\r\n' + body)
    c.close()
def exit_proxy(c):
    head = read_head(c)
    if head.startswith(b'CONNECT '):
        c.sendall(b'HTTP/1.1 200 Connection Established\r\n\r\n')
        try:
            while True:
                d = c.recv(4096)
                if not d: break
                c.sendall(d)
        except OSError: pass
    else:
        up = socket.create_connection(('127.0.0.1', 18790)); up.sendall(head)
        while True:
            d = up.recv(4096)
            if not d: break
            c.sendall(d)
        up.close()
    c.close()
threading.Thread(target=serve, args=(18790, upstream), daemon=True).start()
serve(18799, exit_proxy)
"""


# Reads a machine's sessions in the gateway container (python:3.13-slim); the
# machine's name is the first argument.
SESSIONS_QUERY = """
import json, sqlite3, sys
db = sqlite3.connect("file:/state/gw.sqlite?mode=ro", uri=True)
rows = db.execute(
    "SELECT end_reason, ended_at IS NOT NULL, launch_meta FROM sessions"
    " WHERE client_machine = ? ORDER BY created_at, rowid", (sys.argv[1],))
print(json.dumps([
    {"end_reason": reason, "ended": bool(ended),
     "cwd": json.loads(meta or "{}").get("cwd")}
    for reason, ended, meta in rows]))
"""


def redact(text: str, secret_values: list[str]) -> str:
  """Replace every secret value in text with a marker.

  Args:
    text: The text to clean.
    secret_values: The values that must not appear; empty ones are skipped.

  Returns:
    The text with each value replaced by ``<redacted>``.
  """
  for value in secret_values:
    if value:
      text = text.replace(value, "<redacted>")
  return text


def redact_value(value: Any, secret_values: list[str]) -> Any:
  """Redact every string inside a JSON-like value.

  Args:
    value: A string, number, boolean, None, list or dict of those.
    secret_values: The values that must not appear.

  Returns:
    The same shape with every string passed through redact.
  """
  if isinstance(value, str):
    return redact(value, secret_values)
  if isinstance(value, list):
    return [redact_value(item, secret_values) for item in value]
  if isinstance(value, dict):
    return {
      key: redact_value(item, secret_values) for key, item in value.items()
    }
  return value


def leaking_argument(command: list[str], secret_values: list[str]) -> bool:
  """Report whether any argument of a command holds a secret value.

  Args:
    command: The command line about to run.
    secret_values: The values that must not go into a command line.

  Returns:
    True when an argument contains one of the values.
  """
  return any(
    value and value in argument
    for argument in command
    for value in secret_values
  )


def credential_hash(credential: str) -> str:
  """Return the SHA-256 of a machine credential, as the gateway registers it.

  Args:
    credential: The credential's value.

  Returns:
    Lowercase hex.
  """
  return hashlib.sha256(credential.encode()).hexdigest()


def new_credential() -> str:
  """Generate a machine credential the way a client machine does.

  Returns:
    256 random bits, base64url without padding.
  """
  return base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode()


def launch_outcome(
  result: subprocess.CompletedProcess[str], marker: str
) -> dict[str, Any]:
  """Classify a launcher run that should have been refused before claude ran.

  Args:
    result: The completed launcher process.
    marker: The gateway's reason the stderr must name.

  Returns:
    The exit status and either the marker's code or the stderr's tail.
  """
  messages = {
    "at its limit of": "machine_limit",
    "matches no registered machine": "unknown_machine",
  }
  message = result.stderr.strip()[-200:]
  for fragment, code in messages.items():
    if fragment in result.stderr and code == marker:
      message = code
  return {"exit": result.returncode, "message": message}


def log_reading(
  result: subprocess.CompletedProcess[str], secret_values: list[str]
) -> dict[str, Any]:
  """Evaluate the gateway log for the zero-hit arm.

  Args:
    result: The completed ``docker logs`` process.
    secret_values: The values that must not appear in the log.

  Returns:
    Whether the log counts as read (exit 0 and the startup line present)
    and how many values were found.
  """
  text = result.stdout + result.stderr
  return {
    "log_read": result.returncode == 0 and STARTUP_LINE in text,
    "hits": sum(1 for value in secret_values if value and value in text),
  }


@dataclass
class Run:
  """One rehearsal: its names, addresses, files, secrets and report.

  Attributes:
    gateway: The static redcoast binary.
    launcher: The static redcoast-client binary.
    root: The run directory, private to this run.
    subnet: The Docker network's subnet.
    keep: Whether a failed run's containers are left for inspection.
    run_id: Eight hex digits naming this run.
    report: The report written at the end.
    secret_values: Values that must never reach a command line, an error or
      the report: the machine credentials, the synthetic account credentials
      and, once issued, the session credential. They travel by file or stdin.
    LAUNCH_ENTRY: The shell command a launcher container runs.
    GONE_ENTRY: The same, started in a directory that no longer exists.
  """

  gateway: Path
  launcher: Path
  root: Path
  subnet: str
  keep: bool
  run_id: str = field(default_factory=lambda: uuid.uuid4().hex[:8])
  report: dict[str, Any] = field(default_factory=dict)
  secret_values: list[str] = field(default_factory=list)

  def __post_init__(self) -> None:
    """Derive the names and addresses."""
    self.name = "xmachine-" + self.run_id
    self.label = "gateway.e2e.run=" + self.name
    base = self.subnet.rsplit(".", 1)[0]
    self.gw_ip, self.a_ip, self.b_ip, self.a2_ip, self.c_ip, self.d_ip = (
      base + ".10",
      base + ".11",
      base + ".12",
      base + ".13",
      base + ".14",
      base + ".15",
    )
    self.report.update(
      {
        "run": self.name,
        "directory": str(self.root),
        "started_utc": datetime.datetime.now(datetime.UTC).isoformat(),
        "gateway_binary_sha256": hashlib.sha256(
          self.gateway.read_bytes()
        ).hexdigest(),
        "launcher_binary_sha256": hashlib.sha256(
          self.launcher.read_bytes()
        ).hexdigest(),
        "addresses": {
          "gateway": self.gw_ip,
          "client_a": self.a_ip,
          "client_b": self.b_ip,
        },
        "arms": {},
        "cleanup": {},
      }
    )

  def sh(
    self,
    command: list[str],
    label: str,
    check: bool = True,
    input: str | None = None,
    timeout: int = 60,
  ) -> subprocess.CompletedProcess[str]:
    """Run a command; errors name only the label and the exit.

    Args:
      command: The command line.
      label: What the command does, for errors and the report.
      check: Whether a non-zero exit is an error.
      input: Text for the command's stdin.
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
        command, capture_output=True, text=True, input=input, timeout=timeout
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

    The actual reading and the extra fields may carry text a container
    printed (a launcher's stderr, a probe's stdout), so they are redacted
    before they reach the report or the output; the verdict is taken on the
    reading as it was.

    Args:
      arm: The arm's name.
      want: The expected reading.
      got: The actual reading.
      extra: More fields for the report.
    """
    ok = want == got
    got = redact_value(got, self.secret_values)
    fields: dict[str, Any] = redact_value(extra or {}, self.secret_values)
    self.report["arms"][arm] = {"want": want, "got": got, "ok": ok, **fields}
    print(("PASS " if ok else "FAIL ") + arm + ": " + json.dumps(got))

  def gateway_exec(
    self, *command: str, check: bool = True
  ) -> subprocess.CompletedProcess[str]:
    """Run a management subcommand of the gateway inside its container.

    Args:
      *command: The subcommand and its arguments.
      check: Whether a non-zero exit is an error.

    Returns:
      The completed process.
    """
    return self.sh(
      [
        "docker",
        "exec",
        self.name + "-gw",
        "/redcoast",
        "claude",
        *command,
        "--admin-socket",
        "/state/admin.sock",
      ],
      "gateway " + command[0],
      check=check,
    )

  def write_files(self) -> None:
    """Write the run's files.

    The inventory, the synthetic credentials, the upstream program and the two
    machine credentials go into the run directory.
    """
    inventory = self.root / "inventory"
    inventory.mkdir()
    (inventory / (ALIAS + ".yaml")).write_text(INVENTORY)
    self.synthetic = {
      TOKEN_REF: "sk-ant-oat01-synthetic-" + secrets.token_hex(8),
      USERNAME_REF: "synthetic-user",
      PASSWORD_REF: "Synthetic-" + secrets.token_hex(4),
    }
    creds = self.root / "creds.json"
    creds.write_text(json.dumps(self.synthetic))
    os.chmod(creds, 0o600)
    self.secret_values += [
      self.synthetic[TOKEN_REF],
      self.synthetic[PASSWORD_REF],
    ]
    (self.root / "upstream.py").write_text(UPSTREAM)
    for file in ["cred-a", "cred-wrong", "cred-c", "cred-d"]:
      path = self.root / file
      path.write_text(new_credential() + "\n")
      os.chmod(path, 0o600)
      self.secret_values.append(path.read_text().strip())
    self.hash_a, self.hash_c, self.hash_d = (
      credential_hash((self.root / file).read_text().strip())
      for file in ["cred-a", "cred-c", "cred-d"]
    )

  def common(self) -> list[str]:
    """Return the docker run options every container of the run shares.

    Returns:
      The options.
    """
    uid = f"{os.getuid()}:{os.getgid()}"
    return [
      "--label", self.label, "--user", uid, "--read-only", "--cap-drop", "ALL",
      "--security-opt", "no-new-privileges", "--cpus", "1", "--memory", "256m",
      "--pids-limit", "128", "--tmpfs", "/tmp:rw,nosuid,nodev,size=8m",
    ]  # fmt: skip

  def client_command(
    self,
    container: str,
    ip: str,
    env: dict[str, str],
    mounts: list[str],
    entry: str,
    interactive: bool = False,
    remove: bool = True,
  ) -> list[str]:
    """Build the docker run command of a client container.

    Args:
      container: The container's name.
      ip: Its address on the run's network.
      env: Its environment variables.
      mounts: Extra bind mounts, as ``host:container:ro``.
      entry: The shell command it runs.
      interactive: Whether stdin is passed through (``-i``).
      remove: Whether the container is removed when it exits (``--rm``);
        without it, ``docker wait`` reads its exit status.

    Returns:
      The command line.
    """
    command = [
      "docker", "run", *(["--rm"] if remove else []), "--name", container,
      "--network", self.name,
      "--ip", ip, *(["-i"] if interactive else []), *self.common(),
      "--tmpfs", f"/client:rw,nosuid,nodev,size=16m,uid={os.getuid()}",
      "-v", f"{self.launcher}:/redcoast-client:ro",
      "-v", f"{PROBE}:/fake-claude:ro",
    ]  # fmt: skip
    for mount in mounts:
      command += ["-v", mount]
    for key, value in env.items():
      command += ["-e", f"{key}={value}"]
    return command + [CLIENT_IMAGE, "sh", "-c", entry]

  def launch_env(self) -> dict[str, str]:
    """Return the environment of a launcher run in a client container.

    Returns:
      The variables.
    """
    return {
      "PATH": "/usr/bin:/bin",
      "HOME": "/client/home",
      "REDCOAST_CLIENT_ADDRESS": self.gw_ip + ":7801",
      "REDCOAST_CLIENT_CREDENTIAL_FILE": "/cred",
      "REDCOAST_CLIENT_CLAUDE": "/fake-claude",
      "REDCOAST_CLIENT_MACHINE": "ignored-over-the-network",
    }

  LAUNCH_ENTRY = (
    "mkdir -p /client/home && printf '{\"hasCompletedOnboarding\": true}'"
    " > /client/home/.claude.json && cd /client/home"
    " && exec /redcoast-client claude"
  )
  GONE_ENTRY = (
    "mkdir -p /client/gone && cd /client/gone && rmdir /client/gone"
    " && exec /redcoast-client claude"
  )

  def probe_env(self) -> dict[str, str]:
    """Return the environment of a probe (the two requests with a credential).

    Returns:
      The variables; the credential itself goes in on stdin.
    """
    return {
      "PATH": "/usr/bin:/bin",
      "HOME": "/client/home",
      "PROBE_BASE": f"http://{self.gw_ip}:7802",
      "PROBE_PROXY": f"{self.gw_ip}:7803",
      "HTTPS_PROXY": "http://none",
    }

  def probe_from_a(self, token: str) -> subprocess.CompletedProcess[str]:
    """Do the two requests with token from A's own address.

    Args:
      token: The session credential, passed on stdin.

    Returns:
      The completed probe.
    """
    env = self.probe_env()
    command = ["docker", "exec", "-i"]
    for key in ["PROBE_BASE", "PROBE_PROXY", "HTTPS_PROXY"]:
      command += ["-e", f"{key}={env[key]}"]
    command += [self.name + "-a", "/fake-claude", "probe"]
    return self.sh(command, "probe from A", input=token + "\n")

  def launch(
    self,
    container: str,
    ip: str,
    credential_file: str,
    entry: str = LAUNCH_ENTRY,
    env: dict[str, str] | None = None,
  ) -> subprocess.CompletedProcess[str]:
    """Run a launcher in a fresh client container until it exits.

    Args:
      container: The container's name.
      ip: Its address.
      credential_file: The machine credential file to mount at /cred.
      entry: The shell command the container runs.
      env: Variables added to the launcher's environment.

    Returns:
      The completed container run.
    """
    return self.sh(
      self.client_command(
        container,
        ip,
        {**self.launch_env(), **(env or {})},
        [f"{self.root / credential_file}:/cred:ro"],
        entry,
      ),
      "launch in " + container,
      check=False,
    )

  def hold(self, container: str, ip: str, credential_file: str) -> str:
    """Start a launcher in a detached container whose claude holds its session.

    The container is not removed when it exits, so that ``docker wait`` reads
    the launcher's exit status.

    Args:
      container: The container's name.
      ip: Its address.
      credential_file: The machine credential file to mount at /cred.

    Returns:
      The session credential the fake claude received.
    """
    command = self.client_command(
      container, ip, self.launch_env(),
      [f"{self.root / credential_file}:/cred:ro"], self.LAUNCH_ENTRY,
      remove=False,
    )  # fmt: skip
    self.sh(command[:2] + ["-d"] + command[2:], "start " + container)
    return self.wait_for_session(container)[1]

  def wait_for_session(self, container: str) -> tuple[str, str]:
    """Wait for a holding fake claude's probe result and session credential.

    Args:
      container: The client container.

    Returns:
      The probe's result line and the session credential, which joins the
      secret values.

    Raises:
      RuntimeError: When the container produces no result in time.
    """
    for _ in range(100):
      got = self.sh(
        ["docker", "exec", container, "sh", "-c",
         "cat /client/out/result 2>/dev/null;"
         " cat /client/out/token 2>/dev/null"],
        "read result of " + container,
        check=False,
      )  # fmt: skip
      lines = got.stdout.splitlines()
      if got.returncode == 0 and "post=" in got.stdout and len(lines) >= 2:
        self.secret_values.append(lines[1])
        return lines[0], lines[1]
      time.sleep(0.2)
    raise RuntimeError(container + " produced no result")

  def wait_exit(self, container: str) -> int:
    """Wait for a container started without --rm to exit.

    Args:
      container: The container.

    Returns:
      Its main process's exit status.
    """
    waited = self.sh(["docker", "wait", container], "wait for " + container)
    return int(waited.stdout.strip())

  def sessions_of(self, machine: str) -> list[dict[str, Any]]:
    """Read a machine's sessions from the gateway's database, oldest first.

    Args:
      machine: The machine's name.

    Returns:
      Each session's end reason, whether it ended and the keys and cwd of its
      launch metadata.
    """
    read = self.sh(
      ["docker", "exec", self.name + "-gw", "python3", "-c", SESSIONS_QUERY,
       machine],
      "read sessions of " + machine,
    )  # fmt: skip
    return json.loads(read.stdout)

  def start_gateway(self) -> None:
    """Start the gateway container and wait until its management socket answers.

    Raises:
      RuntimeError: When the gateway does not come up in time.
    """
    self.sh(
      [
        "docker",
        "network",
        "create",
        "--label",
        self.label,
        "--subnet",
        self.subnet,
        self.name,
      ],
      "create network",
    )
    (self.root / "gateway.yaml").write_text(
      "upstream: http://127.0.0.1:18790\n"
      "listen:\n"
      f"  reverse: {self.gw_ip}:7802\n"
      f"  forward: {self.gw_ip}:7803\n"
      f"  session: {self.gw_ip}:7801\n"
      f"  health: {self.gw_ip}:7804\n"
      "session_socket: /state/s.sock\n"
      "admin_socket: /state/admin.sock\n"
      "session_db: /state/gw.sqlite\n"
      "inventory: /inventory\n"
      "credentials:\n"
      "  stdin_test_entry: true\n"
    )
    entry = (
      "python3 /upstream.py"
      " & exec /redcoast claude serve --config /gateway.yaml < /creds.json"
    )
    self.sh(
      [
        "docker",
        "run",
        "-d",
        "--name",
        self.name + "-gw",
        "--network",
        self.name,
        "--ip",
        self.gw_ip,
        *self.common(),
        "--tmpfs",
        f"/state:rw,nosuid,nodev,size=64m,uid={os.getuid()}",
        "-v",
        f"{self.gateway}:/redcoast:ro",
        "-v",
        f"{self.root / 'upstream.py'}:/upstream.py:ro",
        "-v",
        f"{self.root / 'inventory'}:/inventory:ro",
        "-v",
        f"{self.root / 'creds.json'}:/creds.json:ro",
        "-v",
        f"{self.root / 'gateway.yaml'}:/gateway.yaml:ro",
        GATEWAY_IMAGE,
        "sh",
        "-c",
        entry,
      ],  # fmt: skip
      "start gateway",
    )
    for _ in range(100):
      if self.gateway_exec("status", check=False).returncode == 0:
        break
      time.sleep(0.2)
    else:
      logs = self.sh(
        ["docker", "logs", self.name + "-gw"], "gateway log", check=False
      )
      self.report["gateway_log_tail"] = redact(
        logs.stderr[-600:], self.secret_values
      )
      raise RuntimeError("gateway did not come up")
    # A plan makes the account a candidate; written on the gateway's database.
    self.sh(
      [
        "docker",
        "exec",
        self.name + "-gw",
        "/redcoast",
        "claude",
        "plan",
        "--session-db",
        "/state/gw.sqlite",
        "schedule",
        ALIAS,
        "pro",
        "2026-01-01T00:00:00Z",
      ],  # fmt: skip
      "gateway plan",
    )

  def arms(self) -> None:
    """Run the arms of machines A and B against the started gateway."""
    added = self.gateway_exec(
      "machine", "add", "client-a", self.hash_a, "--max-sessions", "1"
    )
    self.record(
      "register_machine_a", {"name": "client-a"}, json.loads(added.stdout)
    )

    # Positive arm: A, with its credential, gets a session over 7801 and uses
    # it: inference 200, CONNECT 200. The container is detached and holds the
    # session until told to stop.
    a_command = self.client_command(
      self.name + "-a", self.a_ip, self.launch_env(),
      [f"{self.root / 'cred-a'}:/cred:ro"], self.LAUNCH_ENTRY,
    )  # fmt: skip
    self.sh(a_command[:2] + ["-d"] + a_command[2:], "start client A")
    result_a, token_a = self.wait_for_session(self.name + "-a")
    self.record("a_inference_and_connect", "post=200 connect=200", result_a)
    status = json.loads(self.gateway_exec("status").stdout)
    self.record("status_live_sessions_after_a", 1, status.get("live_sessions"))

    # Limit arm: A is at its limit of 1 while the first session lives.
    second = self.launch(self.name + "-a2", self.a2_ip, "cred-a")
    self.record(
      "a_second_session_over_limit",
      {"exit": 3, "message": "machine_limit"},
      launch_outcome(second, "machine_limit"),
    )
    # Wrong machine credential from B: refused before claude starts.
    wrong = self.launch(self.name + "-b", self.b_ip, "cred-wrong")
    self.record(
      "b_wrong_machine_credential",
      {"exit": 3, "message": "unknown_machine"},
      launch_outcome(wrong, "unknown_machine"),
    )
    # A's session credential from B's address: 401 and 407. Control: the same
    # credential from A's address passes.
    stolen = self.sh(
      self.client_command(
        self.name + "-b",
        self.b_ip,
        self.probe_env(),
        [],
        "exec /fake-claude probe",
        interactive=True,
      ),  # fmt: skip
      "probe from B",
      check=False,
      input=token_a + "\n",
    )
    self.record(
      "a_session_credential_from_b",
      "post=401 connect=407",
      stolen.stdout.strip(),
    )
    control = self.probe_from_a(token_a)
    self.record(
      "a_session_credential_from_a_control",
      "post=200 connect=200",
      control.stdout.strip(),
    )

    # Revocation: A's live session ends, its credential is refused from then
    # on, a new launch is refused.
    revoked = json.loads(
      self.gateway_exec("machine", "revoke", "client-a").stdout
    )
    self.record(
      "revoke_machine_a", {"name": "client-a", "sessions_ended": 1}, revoked
    )
    after = self.probe_from_a(token_a)
    self.record(
      "a_session_after_revocation", "post=401 connect=407", after.stdout.strip()
    )
    relaunch = self.launch(self.name + "-a2", self.a2_ip, "cred-a")
    self.record(
      "a_relaunch_after_revocation",
      {"exit": 3, "message": "unknown_machine"},
      launch_outcome(relaunch, "unknown_machine"),
    )
    machines = json.loads(self.gateway_exec("machine", "list").stdout)
    self.record(
      "machine_list_shows_revocation",
      True,
      len(machines) == 1
      and machines[0]["name"] == "client-a"
      and "revoked_at" in machines[0],
    )

    # Let A's fake claude exit; the launcher's own revocation then finds the
    # session gone, which it reports and tolerates.
    self.sh(
      ["docker", "exec", self.name + "-a", "touch", "/client/stop"],
      "stop client A",
    )
    for _ in range(50):
      running = self.sh(
        ["docker", "inspect", "-f", "{{.State.Running}}", self.name + "-a"],
        "inspect client A",
        check=False,
      )
      if running.stdout.strip() != "true":
        break
      time.sleep(0.2)

  def session_arms(self) -> None:
    """Run the arms of how a session ends, on machines C and D.

    C may hold one session. A launch whose claude exits and one whose claude
    is killed both end the session through the launcher's revocation (reason
    client). A launcher that cannot read its working directory stops before
    it asks for a session. A killed launcher revokes nothing: its session
    stays live and holds C's slot until the idle expiry (seven days, fixed in
    the session store), so the next launch on C is refused. D asks for a
    session directly, with launch metadata that has no cwd, and gets one.
    """
    for machine, digest in [
      ("client-c", self.hash_c),
      ("client-d", self.hash_d),
    ]:
      self.gateway_exec(
        "machine", "add", machine, digest, "--max-sessions", "1"
      )
    c = self.name + "-c"
    ended = {"end_reason": "client", "ended": True, "cwd": "/client/home"}
    done = self.launch(
      c + "1", self.c_ip, "cred-c", env={"FAKE_CLAUDE_EXIT": "0"}
    )
    self.record(
      "c_claude_exits_session_ends",
      {"exit": 0, "probe": "post=200 connect=200", "sessions": [ended]},
      {"exit": done.returncode, "probe": done.stdout.strip(),
       "sessions": self.sessions_of("client-c")},
    )  # fmt: skip

    self.hold(c + "2", self.c_ip, "cred-c")
    self.sh(
      ["docker", "exec", c + "2", "sh", "-c", "kill -9 $(cat /client/out/pid)"],
      "kill claude on C",
    )
    self.record(
      "c_claude_killed_session_ends",
      {"exit": 137, "sessions": [ended, ended]},
      {"exit": self.wait_exit(c + "2"),
       "sessions": self.sessions_of("client-c")},
    )  # fmt: skip

    gone = self.launch(c + "3", self.c_ip, "cred-c", entry=self.GONE_ENTRY)
    self.record(
      "c_no_cwd_launcher_refuses",
      {"exit": 3, "getwd_error": True, "sessions": 2},
      {"exit": gone.returncode,
       "getwd_error": "redcoast-client: getwd" in gone.stderr,
       "sessions": len(self.sessions_of("client-c"))},
    )  # fmt: skip

    # The launcher is the container's first process; docker kill sends it
    # SIGKILL, which it cannot forward or survive.
    self.hold(c + "4", self.c_ip, "cred-c")
    self.sh(["docker", "kill", c + "4"], "kill launcher on C")
    live = {"end_reason": None, "ended": False, "cwd": "/client/home"}
    self.record(
      "c_launcher_killed_session_stays_live",
      {"exit": 137, "sessions": [ended, ended, live]},
      {"exit": self.wait_exit(c + "4"),
       "sessions": self.sessions_of("client-c")},
    )  # fmt: skip
    refused = self.launch(c + "5", self.c_ip, "cred-c")
    self.record(
      "c_slot_held_by_killed_launcher",
      {"exit": 3, "message": "machine_limit"},
      launch_outcome(refused, "machine_limit"),
    )

    issued = self.sh(
      self.client_command(
        self.name + "-d", self.d_ip,
        {**self.probe_env(), "PROBE_SESSION": f"http://{self.gw_ip}:7801",
         "ISSUE_BODY": '{"launch_meta":{"launcher_pid":1}}'},
        [f"{self.root / 'cred-d'}:/cred:ro"], "exec /fake-claude issue",
      ),
      "issue without cwd from D",
      check=False,
    )  # fmt: skip
    self.record(
      "d_issue_without_cwd_registers",
      {"status": "201", "sessions": [
        {"end_reason": None, "ended": False, "cwd": None}]},
      {"status": issued.stdout.strip(),
       "sessions": self.sessions_of("client-d")},
    )  # fmt: skip

  def log_arm(self) -> None:
    """Check that no secret value reached the gateway's log."""
    # The log must have been read and hold the startup line, or a zero count
    # says nothing.
    logs = self.sh(
      ["docker", "logs", self.name + "-gw"], "gateway log", check=False
    )
    self.record(
      "no_credential_in_gateway_log",
      {"log_read": True, "hits": 0},
      log_reading(logs, self.secret_values),
      {"log_lines": len((logs.stdout + logs.stderr).splitlines())},
    )

  def cleanup(self) -> bool:
    """Remove the run's containers and network and count what is left, twice.

    Returns:
      True when nothing of the run remains at either count.
    """
    for suffix in [
      "-a",
      "-a2",
      "-b",
      "-c1",
      "-c2",
      "-c3",
      "-c4",
      "-c5",
      "-d",
      "-gw",
    ]:
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


def terminated(signum: int, frame: object) -> None:
  """Turn a SIGTERM into a SystemExit so that the cleanup still runs.

  Args:
    signum: The signal number.
    frame: The interrupted frame.

  Raises:
    SystemExit: Always, with 128 plus the signal number.
  """
  raise SystemExit(128 + signum)


def main(argv: list[str] | None = None) -> int:
  """Run the rehearsal and write its report.

  Args:
    argv: Command-line arguments; None reads sys.argv.

  Returns:
    0 when every arm passed and the cleanup left nothing, else 1.
  """
  parser = argparse.ArgumentParser(
    description="Cross-machine session rehearsal in Docker."
  )
  parser.add_argument(
    "--gateway-binary",
    type=Path,
    required=True,
    help="static redcoast (CGO_ENABLED=0)",
  )
  parser.add_argument(
    "--launcher-binary",
    type=Path,
    required=True,
    help="static redcoast-client (CGO_ENABLED=0)",
  )
  parser.add_argument(
    "--out",
    type=Path,
    help="directory for the run's files and report; it must not exist yet"
    " (default: a new temporary directory)",
  )
  parser.add_argument(
    "--subnet",
    default="172.30.77.0/24",
    help="Docker network subnet for the three containers",
  )
  parser.add_argument(
    "--keep",
    action="store_true",
    help="leave the containers of a failed run for a look",
  )
  args = parser.parse_args(argv)
  if args.out is None:
    root = Path(tempfile.mkdtemp(prefix="xmachine-"))
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
    run.write_files()
    run.start_gateway()
    run.arms()
    run.session_arms()
    run.log_arm()
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
