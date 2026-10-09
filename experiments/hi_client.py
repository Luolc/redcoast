"""The client side of the hi rehearsal, run inside a client container.

The host (hi.py) mounts this file at /runner.py, the run's settings at
/runner-settings.json, the Claude CLI at /claude (and redcoast-client at
/redcoast-client when the case runs through the launcher) and the gateway's
session socket at /session.sock. The program plays one consumer: it obtains
a session, drives the CLI through the case and prints one JSON object with
what it observed. No SDK exception, CLI output, or native profile leaves it.
"""

from collections.abc import Iterator
from dataclasses import dataclass, field
import datetime
import fcntl
import http.client
import ipaddress
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import socket
import struct
import subprocess
import termios
import time
from typing import Any
import urllib.request
import uuid

SETTINGS_FILE = Path("/runner-settings.json")
BASE = ["/claude", "--print", "--model", "sonnet", "--strict-mcp-config"]
EXACT = "Follow the user instruction exactly and add nothing else."
HI_ARGUMENTS = [
  "--tools", "", "--no-session-persistence",
  "--system-prompt", "Reply with exactly Hi.", "Hi",
]  # fmt: skip
FIRST_REPLY = (
  "What was your first reply in this conversation?"
  " Reply with exactly that word."
)
ESCAPES = (
  r"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)"
  r"|\x1b[()][0-9A-B]|\x1b[=>]"
)
ERROR_MARKERS = [
  ("controlled_proxy_block", "controlled_proxy_block"),
  ("proxy authentication required", "proxy_auth_text"),
  ("could not connect to proxy", "proxy_connect_text"),
  ("invalid api key", "auth_text"),
  ("authentication_error", "auth_text"),
  ("invalid bearer token", "auth_text"),
  ("rate limit", "rate_limit_text"),
  ("timed out", "timeout_text"),
]
LAUNCHER_ENV = {
  "REDCOAST_CLIENT_SOCKET": "/session.sock",
  "REDCOAST_CLIENT_CLAUDE": "/claude",
  "REDCOAST_CLIENT_MACHINE": "client-container",
}


@dataclass
class Settings:
  """The run's settings, written by the host next to this program.

  Attributes:
    case: The case being run.
    limits_arm: The limits case's arm, else None.
    real: Whether the run is authenticated.
    default_traffic: Whether the CLI's traffic switches stay at their
      defaults.
    only_switch: The one traffic switch to set, else None.
    extra: Seconds every time limit grows by for the long cases.
    marketplace_off: Whether the marketplace autoinstall switch is set.
    mitm: Whether the fake exit terminates TLS with the mounted CA.
    launcher: Whether redcoast-client is mounted and the case runs through it.
    inferences_quota: The quota case's number of inference requests.
    quota_stop_on_atb: Whether a failed starting atb read stops the run.
    quota_allow_retries: Whether the CLI's own retries stay on.
    cache_arm: The cache case's arm, else None.
  """

  case: str
  limits_arm: str | None
  real: bool
  default_traffic: bool
  only_switch: str | None
  extra: int
  marketplace_off: bool
  mitm: bool
  launcher: bool
  inferences_quota: int
  quota_stop_on_atb: bool
  quota_allow_retries: bool
  cache_arm: str | None


@dataclass
class State:
  """What the consumer holds while it runs.

  Attributes:
    clients: The CLI processes started, for the cleanup.
    config: The CLI's configuration directory, once created.
    work: The CLI's working directory.
    tui_pid: The interactive CLI's PID while it runs.
    deadline: The monotonic time by which the workload must end.
    env: The CLI's environment.
    session_token: The session credential, when this consumer issued it.
  """

  clients: list[subprocess.Popen[bytes]] = field(default_factory=list)
  config: Path | None = None
  work: Path = Path("/client/work")
  tui_pid: int | None = None
  deadline: float = 0.0
  env: dict[str, str] = field(default_factory=dict)
  session_token: str | None = None


S: Settings
state = State()
out: dict[str, Any] = {}


def utc() -> str:
  """Return the current time in UTC as ISO 8601.

  Returns:
    The timestamp.
  """
  return datetime.datetime.now(datetime.UTC).isoformat()


def interrupted(signum: int, frame: object) -> None:
  """Turn a signal into an exception so that the cleanup runs.

  Args:
    signum: The signal number.
    frame: The interrupted frame.

  Raises:
    RuntimeError: Always.
  """
  raise RuntimeError("interrupted")


class SocketConnection(http.client.HTTPConnection):
  """HTTP over the gateway's unix socket; the host name is a placeholder."""

  def connect(self) -> None:
    """Connect to the session socket."""
    self.sock = socket.socket(socket.AF_UNIX)
    self.sock.settimeout(self.timeout)
    self.sock.connect("/session.sock")


def session_request(
  method: str,
  path: str,
  body: dict[str, Any] | None = None,
  token: str | None = None,
) -> tuple[int, Any]:
  """Call the gateway's session interface.

  Args:
    method: The HTTP method.
    path: The path.
    body: A JSON body, if any.
    token: A session credential to send as Bearer, if any.

  Returns:
    The status and the decoded body (None when empty).
  """
  connection = SocketConnection("session", timeout=10)
  headers = {"Content-Type": "application/json"}
  if token:
    headers["Authorization"] = "Bearer " + token
  connection.request(
    method,
    path,
    body=json.dumps(body).encode() if body is not None else None,
    headers=headers,
  )
  response = connection.getresponse()
  data = response.read(8192)
  connection.close()
  return response.status, (json.loads(data) if data else None)


def said(result: dict[str, Any], expected: list[str]) -> bool:
  """Report whether a CLI result object carries one of the expected texts.

  Args:
    result: The CLI's result object.
    expected: The accepted replies.

  Returns:
    True when the result is no error and its text, stripped of trailing
    punctuation, is one of them.
  """
  text = result.get("result")
  return (
    result.get("is_error") is False
    and isinstance(text, str)
    and text.strip().rstrip(".!") in expected
  )


def classify_cli(
  result: Any, stderr: bytes, exit_code: int | None, expected: list[str]
) -> str:
  """Name what a CLI run produced.

  Original output remains in memory; categories describe matched text, not
  root cause.

  Args:
    result: The parsed stdout, or None.
    stderr: The CLI's stderr.
    exit_code: The CLI's exit code.
    expected: The accepted replies.

  Returns:
    The category.
  """
  if not isinstance(result, dict):
    return "invalid_json"
  if exit_code == 0 and said(result, expected):
    return "expected"
  text = result.get("result")
  text = (text if isinstance(text, str) else "").lower()
  text += " " + stderr.decode("utf-8", errors="replace").lower()
  for marker, category in ERROR_MARKERS:
    if marker in text:
      return category
  return "unknown_error"


def parse_json(stdout: bytes) -> Any:
  """Parse a CLI's stdout as JSON when it is not too large.

  Args:
    stdout: The CLI's stdout.

  Returns:
    The object, or None.
  """
  try:
    return json.loads(stdout) if len(stdout) <= 1048576 else None
  except ValueError:
    return None


def start(
  arguments: list[str],
  stdin: int = subprocess.DEVNULL,
  stderr: int = subprocess.DEVNULL,
) -> subprocess.Popen[bytes]:
  """Start one CLI process with the base arguments.

  Args:
    arguments: Arguments after the base ones.
    stdin: The process's stdin.
    stderr: The process's stderr.

  Returns:
    The process, also kept for the cleanup.
  """
  process = subprocess.Popen(
    BASE + arguments,
    cwd=state.work,
    env=state.env,
    stdin=stdin,
    stdout=subprocess.PIPE,
    stderr=stderr,
    start_new_session=True,
  )
  state.clients.append(process)
  return process


def oneshot(arguments: list[str], expected: list[str]) -> bool:
  """Run one JSON-output CLI process to completion.

  Args:
    arguments: Arguments after the base ones.
    expected: The accepted replies.

  Returns:
    Whether it produced an expected reply.
  """
  process = start(
    ["--output-format", "json", *arguments], stderr=subprocess.PIPE
  )
  stdout, stderr = process.communicate(
    timeout=max(1, state.deadline - time.monotonic())
  )
  category = classify_cli(
    parse_json(stdout), stderr[:1048576], process.returncode, expected
  )
  out["cli_exit_codes"].append(process.returncode)
  out["cli_error_classes"].append(category)
  return category == "expected"


def events(process: subprocess.Popen[bytes]) -> Iterator[dict[str, Any]]:
  """Yield stream-json objects as they arrive.

  Args:
    process: A CLI process with stream-json output.

  Yields:
    Each object.

  Raises:
    RuntimeError: When the buffered output grows past 4 MiB.
    subprocess.TimeoutExpired: At the shared deadline.
  """
  stdout = process.stdout
  assert stdout is not None
  buffer = b""
  while time.monotonic() < state.deadline:
    ready, _, _ = select.select([stdout], [], [], 0.2)
    if not ready:
      continue
    chunk = os.read(stdout.fileno(), 65536)
    if not chunk:
      return
    buffer += chunk
    if len(buffer) > 4194304:
      raise RuntimeError("stream output limit")
    while b"\n" in buffer:
      line, buffer = buffer.split(b"\n", 1)
      try:
        event = json.loads(line)
      except ValueError:
        continue
      if isinstance(event, dict):
        yield event
  raise subprocess.TimeoutExpired("claude", 55 + S.extra)


def finish(process: subprocess.Popen[bytes]) -> None:
  """Wait for a streaming CLI process and record its exit.

  Args:
    process: The process.
  """
  process.wait(timeout=10)
  out["cli_exit_codes"].append(process.returncode)
  out["cli_error_classes"].append("stream_session")


def client_environment(home: Path, config: Path) -> dict[str, str]:
  """Build the CLI's environment, obtaining a session when this consumer must.

  This consumer plays the launcher: it asks the gateway for a session over
  the unix socket and gives only that process the credential. The
  credential appears in no output. Through redcoast-client the launcher does
  that itself and this consumer sets only its variables.

  Args:
    home: The container's home directory.
    config: The CLI's configuration directory.

  Returns:
    The environment.

  Raises:
    RuntimeError: When the gateway issues no session.
  """
  base = {
    "PATH": "/usr/bin:/bin",
    "LANG": "C.UTF-8",
    "HOME": str(home),
    "CLAUDE_CONFIG_DIR": str(config),
  }
  if S.launcher and S.case in ["tui", "limits", "cache"]:
    out["session_source"] = "launcher"
    return base | LAUNCHER_ENV
  status, grant = session_request(
    "POST", "/sessions", {"client_machine": "client-container"}
  )
  if status != 201 or not isinstance(grant, dict) or not grant.get("token"):
    raise RuntimeError("session not issued")
  token = grant["token"]
  state.session_token = token
  out["session_id"] = grant.get("session_id")
  out["session_entrypoints"] = (
    grant.get("reverse_proxy"),
    grant.get("forward_proxy"),
  )
  env = dict(
    base,
    ANTHROPIC_BASE_URL="http://127.0.0.1:8789",
    CLAUDE_CODE_OAUTH_TOKEN=token,
    DISABLE_AUTOUPDATER="1",
  )
  if not S.default_traffic:
    env.update(
      {
        "DISABLE_TELEMETRY": "1",
        "DISABLE_ERROR_REPORTING": "1",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
      }
    )
  if S.only_switch:
    env[S.only_switch] = "1"
  if S.marketplace_off:
    env["CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL"] = "1"
  # Side traffic goes to the gateway's forward proxy with the session
  # credential as the proxy password; exit credentials stay on the gateway
  # side. Only the reverse proxy bypasses it.
  env.update(
    {
      "HTTPS_PROXY": "http://session:" + token + "@127.0.0.1:8791",
      "NO_PROXY": "127.0.0.1:8789",
    }
  )
  out["proxy_environment"] = {
    "HTTPS_PROXY": "http://session:[REDACTED]@127.0.0.1:8791",
    "NO_PROXY": env["NO_PROXY"],
  }
  return env


def prepare() -> None:
  """Check the container's isolation and set the CLI's directories up.

  Raises:
    RuntimeError: When the container sees what only the gateway side may, or
      a profile already exists.
  """
  out["server_binary_present"] = Path("/gateway").exists()
  out["capture_directory_present"] = Path("/capture").exists()
  out["service_account_present"] = "OP_SERVICE_ACCOUNT_TOKEN" in os.environ
  if (
    out["server_binary_present"]
    or out["capture_directory_present"]
    or out["service_account_present"]
  ):
    raise RuntimeError("client isolation failed")
  home = Path("/client")
  home.mkdir(exist_ok=True)
  config = home / "config"
  config.mkdir()
  state.config = config
  state.work = home / "work"
  state.work.mkdir()
  if (config / ".credentials.json").exists():
    raise RuntimeError("unexpected profile")
  state.env = client_environment(home, config)
  if S.mitm:
    state.env["NODE_EXTRA_CA_CERTS"] = "/ca.pem"
  out["traffic_switches"] = sorted(
    key for key in state.env if "DISABLE_" in key
  )


def exit_address(data: str) -> str | None:
  """Extract the exit IP from the location service's answer.

  Args:
    data: The response body.

  Returns:
    The address, or None when the body holds none.
  """
  try:
    address = json.loads(data).get("ip")
  except (ValueError, AttributeError):
    address = data.strip()
  try:
    return str(ipaddress.ip_address(address))
  except (ValueError, TypeError):
    return None


def run_egress_forward() -> None:
  """Fetch the exit IP through the account's forward proxy.

  The side path's exit: one request through the gateway's forward proxy,
  with no Claude credentials.
  """
  opener = urllib.request.build_opener(
    urllib.request.ProxyHandler({"https": state.env["HTTPS_PROXY"]})
  )
  with opener.open("https://ip.oxylabs.io/location", timeout=20) as response:
    data = response.read(65536).decode("utf-8", errors="replace")
    address = exit_address(data)
    out["forward_egress"] = {"status": response.status, "ip": address}
  out["case_ok"] = address is not None


def atb(label: str, env: dict[str, str]) -> None:
  """Read the account's quota with atb and record the windows.

  Only the quota windows and the exit code leave; the record's other fields
  stay in memory.

  Args:
    label: 'start' or 'end'.
    env: atb's environment.
  """
  began = utc()
  result = subprocess.run(
    ["/atb", "quota", "claude", "--json"],
    env=env,
    capture_output=True,
    timeout=60,
  )
  reading: dict[str, Any] = {
    "started_utc": began,
    "ended_utc": utc(),
    "exit_code": result.returncode,
    "stderr_present": bool(result.stderr),
  }
  try:
    record = json.loads(result.stdout)
    reading["windows"] = record.get("windows")
    reading["extra_usage_enabled"] = (record.get("extra_usage") or {}).get(
      "is_enabled"
    )
  except ValueError:
    reading["stdout_present"] = bool(result.stdout)
  out.setdefault("atb", {})[label] = reading


def launcher_inference(env: dict[str, str]) -> str:
  """Send one inference request through redcoast-client and classify it.

  A refused request makes the CLI retry until its own limit; one request may
  take at most 40 s here.

  Args:
    env: The launcher's environment.

  Returns:
    The category.
  """
  process = subprocess.Popen(
    [
      "/redcoast-client",
      "claude",
      "--print",
      "--model",
      "sonnet",
      "--strict-mcp-config",
      "--output-format",
      "json",
      *HI_ARGUMENTS,
    ],  # fmt: skip
    cwd=state.work,
    env=env,
    stdin=subprocess.DEVNULL,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    start_new_session=True,
  )
  state.clients.append(process)
  try:
    stdout, stderr = process.communicate(
      timeout=max(1, min(40, state.deadline - time.monotonic()))
    )
  except subprocess.TimeoutExpired:
    os.killpg(process.pid, signal.SIGTERM)
    try:
      process.communicate(timeout=5)
    except subprocess.TimeoutExpired:
      os.killpg(process.pid, signal.SIGKILL)
      process.communicate()
    category = "process_timeout"
  else:
    category = classify_cli(
      parse_json(stdout), stderr[:1048576], process.returncode, ["Hi"]
    )
  out["cli_exit_codes"].append(process.returncode)
  out["cli_error_classes"].append(category)
  return category


def run_quota() -> None:
  """Read the quota before and after a fixed number of inferences.

  atb reads the account's real quota from its own login directory through
  the gateway's forward proxy (this consumer's session); the inference
  requests go through redcoast-client, one session each.
  """
  config = state.config
  assert config is not None
  (config / ".claude.json").write_text(
    json.dumps({"hasCompletedOnboarding": True})
  )
  atb_env = {
    "PATH": "/usr/local/bin:/usr/bin:/bin",
    "LANG": "C.UTF-8",
    "HOME": "/client",
    "CLAUDE_CONFIG_DIR": "/login",
    "HTTPS_PROXY": state.env["HTTPS_PROXY"],
    "NO_PROXY": state.env["NO_PROXY"],
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "DISABLE_AUTOUPDATER": "1",
  }
  launcher_env = {
    "PATH": "/usr/bin:/bin",
    "LANG": "C.UTF-8",
    "HOME": "/client",
    "CLAUDE_CONFIG_DIR": str(config),
  } | LAUNCHER_ENV
  # One CLI process sends one inference request: its own retries on 5xx and
  # network errors are off, so the number of upstream requests is bounded by
  # the number of processes started.
  if not S.quota_allow_retries:
    launcher_env["CLAUDE_CODE_MAX_RETRIES"] = "0"
  out["quota_retries_disabled"] = not S.quota_allow_retries
  # Real mode stops at the first failure and keeps what it has: a failed
  # starting read leaves no before / after pair, and one refused inference
  # must not be followed by more real calls.
  atb("start", atb_env)
  requests = []
  if (S.real or S.quota_stop_on_atb) and out["atb"]["start"]["exit_code"] != 0:
    out["quota_stopped"] = "atb_start"
  for _ in range(0 if "quota_stopped" in out else S.inferences_quota):
    began = utc()
    category = launcher_inference(launcher_env)
    requests.append(
      {
        "started_utc": began,
        "ended_utc": utc(),
        "expected": category == "expected",
      }
    )
    if category != "expected":
      out["quota_stopped"] = "cli"
      break
  out["quota_requests"] = requests
  if "quota_stopped" not in out:
    atb("end", atb_env)
  out["case_ok"] = (
    "quota_stopped" not in out
    and len(requests) == S.inferences_quota
    and (
      not S.real
      or all(
        out["atb"][k]["exit_code"] == 0
        and isinstance(out["atb"][k].get("windows"), dict)
        for k in ["start", "end"]
      )
    )
  )


class Terminal:
  """The interactive CLI's pseudo-terminal, read as text without escapes."""

  def __init__(self, fd: int) -> None:
    """Wrap the terminal's file descriptor.

    Args:
      fd: The controlling side of the pseudo-terminal.
    """
    self.fd = fd
    self.screen = ""

  def shown(self, pattern: str, seconds: float) -> bool:
    """Wait until the screen text matches a pattern or the time passes.

    Escape sequences and spacing are dropped; the screen text never leaves
    this process.

    Args:
      pattern: A regular expression over the text read so far.
      seconds: How long to wait at most, within the shared deadline.

    Returns:
      Whether the pattern appeared.
    """
    end = min(state.deadline, time.monotonic() + seconds)
    while True:
      if re.search(pattern, self.screen):
        return True
      if time.monotonic() >= end:
        return False
      ready, _, _ = select.select([self.fd], [], [], 0.2)
      if not ready:
        continue
      try:
        data = os.read(self.fd, 65536)
      except OSError:
        return False
      text = re.sub(ESCAPES, "", data.decode("utf-8", errors="replace"))
      self.screen = (self.screen + re.sub(r"\s+", "", text))[-20000:]

  def press(self, keys: bytes) -> None:
    """Clear the text read so far and send keys.

    Args:
      keys: The bytes to write to the terminal.
    """
    self.screen = ""
    os.write(self.fd, keys)


def observe_turn2(terminal: Terminal) -> None:
  """Send the limits case's second turn and snapshot what the UI shows.

  The switch arm asks for the first reply, so the answer depends on the
  history the CLI sends to the account it lands on; the other arms repeat
  Hi and record the UI. Snapshots of the whole screen buffer are taken
  whenever it changes, for up to 60 s or until the reply (switch arm).
  Synthetic run: the text holds no credential.

  Args:
    terminal: The CLI's terminal.
  """
  prompt = FIRST_REPLY.encode() if S.limits_arm == "switch" else b"Hi"
  terminal.shown("$^", 1)
  terminal.press(prompt)
  terminal.shown(prompt[-12:].decode(), 3)
  out["turn2_started_utc"] = utc()
  terminal.press(b"\r")
  snapshots = []
  last = ""
  began = time.monotonic()
  end = min(state.deadline, began + 60)
  while time.monotonic() < end:
    terminal.shown("(?!x)x", 0.5)
    text = terminal.screen[-1500:]
    if text != last:
      snapshots.append(
        {"t_ms": round((time.monotonic() - began) * 1000), "screen": text}
      )
      last = text
    if S.limits_arm == "switch" and re.search(
      "●(Hi|NOHISTORY)", terminal.screen
    ):
      break
    if len(snapshots) >= 40:
      break
  out["turn2_snapshots"] = snapshots
  out["turn2_seconds"] = round(time.monotonic() - began, 1)
  reply = None
  if S.limits_arm == "switch":
    if re.search("●Hi", terminal.screen):
      reply = "Hi"
    elif "NOHISTORY" in terminal.screen:
      reply = "NOHISTORY"
  out["turn2_reply"] = reply


def leave(terminal: Terminal, stages: list[str]) -> None:
  """Wait for the interactive CLI to exit after /exit.

  The limits case adds two interrupts when a retry loop keeps the prompt
  busy.

  Args:
    terminal: The CLI's terminal.
    stages: The stages reached, extended with 'exited' when it does.
  """
  pid = state.tui_pid
  assert pid is not None
  if S.case == "limits":
    terminal.shown("$^", 2)
    if os.waitpid(pid, os.WNOHANG) == (0, 0):
      terminal.press(b"\x03")
      terminal.shown("$^", 1)
      terminal.press(b"\x03")
  end = min(state.deadline, time.monotonic() + 10)
  while time.monotonic() < end:
    terminal.shown("$^", 0.3)
    finished, status = os.waitpid(pid, os.WNOHANG)
    if finished:
      stages.append("exited")
      out["cli_exit_codes"].append(os.waitstatus_to_exitcode(status))
      state.tui_pid = None
      break


def drive_session(terminal: Terminal, stages: list[str]) -> None:
  """Type Hi at the prompt, read the reply, run turn 2 if asked, and exit.

  Args:
    terminal: The CLI's terminal, past the trust dialog.
    stages: The stages reached so far.
  """
  if not terminal.shown(r"shift\+tabtocycle", 15):
    return
  stages.append("prompt_ready")
  terminal.shown("$^", 1)
  terminal.press(b"Hi")
  terminal.shown("Hi", 3)
  terminal.press(b"\r")
  if not terminal.shown("●Hi", 25):
    return
  stages.append("reply_hi")
  if S.case == "limits":
    observe_turn2(terminal)
    stages.append("turn2_observed")
    # Leave the UI: /exit, and two interrupts when a retry loop keeps the
    # prompt busy.
    terminal.press(b"\x1b")
    terminal.shown("$^", 0.5)
  terminal.press(b"/exit")
  terminal.shown("/exit", 3)
  terminal.press(b"\r")
  leave(terminal, stages)


def run_tui() -> None:
  """Drive an interactive session on a pseudo-terminal.

  Trust the folder, type Hi, read the reply, exit. The limits case adds a
  second turn and watches what the UI shows while the gateway or the fake
  upstream refuses it.
  """
  config = state.config
  assert config is not None
  # An empty profile stops at the login picker despite the token variable,
  # so onboarding is pre-marked.
  (config / ".claude.json").write_text(
    json.dumps({"hasCompletedOnboarding": True})
  )
  pid, fd = pty.fork()
  if pid == 0:
    executable = "/redcoast-client" if S.launcher else "/claude"
    os.chdir(state.work)
    os.execve(
      executable,
      [
        executable,
        *(["claude"] if S.launcher else []),
        "--model",
        "sonnet",
        "--strict-mcp-config",
        "--tools",
        "",
        "--system-prompt",
        "Reply with exactly Hi.",
      ],  # fmt: skip
      dict(state.env, TERM="xterm-256color"),
    )
  state.tui_pid = pid
  fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
  terminal = Terminal(fd)
  stages: list[str] = []
  # A dialog can be drawn before it takes input; let it settle for a second
  # before each key.
  if terminal.shown("Entertoconfirm", 15) and re.search(
    "Itrustthisfolder", terminal.screen
  ):
    stages.append("trust_dialog")
    terminal.shown("$^", 1)
    terminal.press(b"\x1b[B")
    if terminal.shown("❯Yes,Itrustthisfolder", 5):
      terminal.press(b"\r")
      drive_session(terminal, stages)
  elif re.search("Selectloginmethod", terminal.screen):
    stages.append("login_picker")
  out["tui_stages"] = stages
  if S.case == "limits":
    out["case_ok"] = stages[:4] == [
      "trust_dialog", "prompt_ready", "reply_hi", "turn2_observed",
    ]  # fmt: skip
  else:
    out["case_ok"] = stages == [
      "trust_dialog",
      "prompt_ready",
      "reply_hi",
      "exited",
    ] and out["cli_exit_codes"] == [0]
  # The transcript file names are the CLI's own session IDs; the gateway's
  # inference rows should carry the same.
  projects = config / "projects"
  out["transcript_session_ids"] = (
    sorted(
      path.stem
      for path in projects.glob("*/*.jsonl")
      if re.fullmatch(r"[0-9a-f-]{36}", path.stem)
    )
    if projects.is_dir()
    else []
  )


def turn(
  process: subprocess.Popen[bytes],
  stream: Iterator[dict[str, Any]],
  turns: list[dict[str, Any]],
  prompt: str,
  expected: str,
) -> bool:
  """Send one user turn on a stream-json process and await its result.

  Args:
    process: The CLI process.
    stream: Its event iterator.
    turns: The record each turn is appended to.
    prompt: The user message.
    expected: The accepted reply.

  Returns:
    Whether the reply was the expected one.
  """
  stdin = process.stdin
  assert stdin is not None
  began = datetime.datetime.now(datetime.UTC)
  message = {"type": "user", "message": {"role": "user", "content": prompt}}
  stdin.write((json.dumps(message) + "\n").encode())
  stdin.flush()
  ok = next(
    (
      said(event, [expected])
      for event in stream
      if event.get("type") == "result"
    ),
    False,
  )
  turns.append(
    {
      "started_utc": began.isoformat(),
      "ended_utc": utc(),
      "started_ms": int(began.timestamp() * 1000),
      "expected": ok,
    }
  )
  return ok


def run_cache() -> None:
  """Run one session of two turns through redcoast-client with a long prefix.

  The rebind arm has the gateway runner put account A at or over hard
  between the turns; the stay arm sends turn 2 untouched. One process, no
  CLI retries: exactly two upstream requests.
  """
  config = state.config
  assert config is not None
  (config / ".claude.json").write_text(
    json.dumps({"hasCompletedOnboarding": True})
  )
  rule = (
    "Rule %d: when the user asks for a single word, reply with that word"
    " alone, with no punctuation, no preamble and no explanation; this rule"
    " exists to make the prompt long and is repeated on purpose.\n"
  )
  prefix = (
    "You are a terse assistant. Follow the user instruction exactly and add"
    " nothing else.\n" + "".join(rule % n for n in range(1, 161))
  )
  env = dict(state.env, CLAUDE_CODE_MAX_RETRIES="0")
  session = str(uuid.uuid4())
  turns: list[dict[str, Any]] = []
  out["cache_session_id"] = session
  process = subprocess.Popen(
    [
      "/redcoast-client",
      "claude",
      "--print",
      "--model",
      "sonnet",
      "--strict-mcp-config",
      "--input-format",
      "stream-json",
      "--output-format",
      "stream-json",
      "--verbose",
      "--tools",
      "",
      "--session-id",
      session,
      "--system-prompt",
      prefix,
    ],  # fmt: skip
    cwd=state.work,
    env=env,
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.DEVNULL,
    start_new_session=True,
  )
  state.clients.append(process)
  stream = events(process)
  if turn(process, stream, turns, "Reply with exactly: Hello", "Hello"):
    proceed = True
    if S.cache_arm == "rebind":
      request = {"turn1_started_ms": turns[0]["started_ms"]}
      Path("/signal/exhaust-request.tmp").write_text(json.dumps(request))
      os.replace("/signal/exhaust-request.tmp", "/signal/exhaust-request")
      end = time.monotonic() + 15
      while (
        not os.path.exists("/signal/exhaust-done") and time.monotonic() < end
      ):
        time.sleep(0.2)
      try:
        out["injection"] = json.loads(Path("/signal/exhaust-done").read_text())
      except (OSError, ValueError):
        out["injection"] = None
      proceed = bool(out["injection"] and out["injection"].get("ok"))
    if proceed:
      turn(process, stream, turns, "Reply with exactly: Hi", "Hi")
  stdin = process.stdin
  assert stdin is not None
  stdin.close()
  finish(process)
  out["cache_turns"] = turns
  out["case_ok"] = (
    len(turns) == 2
    and all(t["expected"] for t in turns)
    and process.returncode == 0
  )


def run_turns() -> None:
  """Run two turns in one process, then a third that resumes the session."""
  session = str(uuid.uuid4())
  turns: list[bool] = []
  process = start(
    [
      "--input-format",
      "stream-json",
      "--output-format",
      "stream-json",
      "--verbose",
      "--tools",
      "",
      "--session-id",
      session,
      "--system-prompt",
      EXACT,
    ],  # fmt: skip
    stdin=subprocess.PIPE,
  )
  stdin = process.stdin
  assert stdin is not None
  stream = events(process)
  for prompt, expected in [
    ("Reply with exactly: Hello", "Hello"),
    ("Reply with exactly: Hi", "Hi"),
  ]:
    message = {"type": "user", "message": {"role": "user", "content": prompt}}
    stdin.write((json.dumps(message) + "\n").encode())
    stdin.flush()
    turns.append(
      next(
        (
          said(event, [expected])
          for event in stream
          if event.get("type") == "result"
        ),
        False,
      )
    )
  stdin.close()
  finish(process)
  turns.append(
    oneshot(
      [
        "--tools",
        "",
        "--resume",
        session,
        "--system-prompt",
        EXACT,
        FIRST_REPLY,
      ],
      ["Hello"],
    )
  )
  out["turn_results_expected"] = turns
  out["case_ok"] = turns == [True, True, True]


def run_stream() -> None:
  """Run the stream, cancel and long cases on one streaming process.

  stream and cancel share one streaming process; cancel interrupts it after
  a few deltas. long asks for thousands of numbers.
  """
  limit = {"stream": 40, "cancel": 400, "long": 8000}[S.case]
  # If the interrupt fails, the request still ends at this output cap.
  if S.case == "cancel":
    state.env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] = "2048"
  # Upper bound on what one long request can generate.
  if S.case == "long":
    state.env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] = "32000"
  process = start(
    [
      "--output-format",
      "stream-json",
      "--verbose",
      "--include-partial-messages",
      "--tools",
      "",
      "--no-session-persistence",
      "--system-prompt",
      EXACT,
      "Count from 1 to " + str(limit) + ", one number per line.",
    ],  # fmt: skip
  )
  began = time.monotonic()
  arrivals: list[float] = []
  completed = False
  ended = False
  signalled: float | None = None
  for event in events(process):
    inner = event.get("event")
    if (
      event.get("type") == "stream_event"
      and isinstance(inner, dict)
      and inner.get("type") == "content_block_delta"
    ):
      arrivals.append(time.monotonic() - began)
      if S.case == "cancel" and len(arrivals) == 5:
        signalled = time.monotonic()
        os.killpg(process.pid, signal.SIGINT)
    elif event.get("type") == "result":
      ended = event.get("is_error") is False
      completed = (
        ended
        and isinstance(event.get("result"), str)
        and re.findall(r"\d+", event["result"])
        == [str(number) for number in range(1, limit + 1)]
      )
  finish(process)
  out["stream_delta_events"] = len(arrivals)
  span = round((arrivals[-1] - arrivals[0]) * 1000) if arrivals else None
  out["stream_delta_span_ms"] = span
  out["stream_completed"] = completed
  out["stream_first_delta_ms"] = round(arrivals[0] * 1000) if arrivals else None
  gaps = (
    later - earlier
    for earlier, later in zip(arrivals, arrivals[1:], strict=False)
  )
  out["stream_max_gap_ms"] = round(max(gaps, default=0) * 1000)
  if S.case == "stream":
    out["case_ok"] = (
      completed
      and process.returncode == 0
      and len(arrivals) >= 5
      and span is not None
      and span >= 200
    )
  elif S.case == "long":
    # The exact text is not the point here; the stream has to last two
    # minutes and end on its own.
    out["case_ok"] = (
      ended
      and process.returncode == 0
      and len(arrivals) >= 5
      and span is not None
      and span >= 120000
    )
  else:
    out["cancel_signal_sent"] = signalled is not None
    out["cancel_exit_ms"] = (
      round((time.monotonic() - signalled) * 1000) if signalled else None
    )
    out["case_ok"] = signalled is not None and not completed


def run_case() -> None:
  """Run the case the settings name."""
  out["cli_started"] = True
  out["workload_started_utc"] = utc()
  if S.case == "egress-forward":
    run_egress_forward()
  elif S.case == "quota":
    run_quota()
  elif S.case in ["hi", "restart"]:
    # Each of restart's three clients sends one Hi.
    out["case_ok"] = oneshot(HI_ARGUMENTS, ["Hi"])
  elif S.case == "tool":
    state.work.joinpath("note.txt").write_text("Hello\n")
    out["case_ok"] = oneshot(
      [
        "--tools",
        "Read",
        "--allowedTools",
        "Read",
        "--no-session-persistence",
        "--system-prompt",
        "Use the Read tool when asked to read a file. Reply with exactly the"
        " file content and nothing else.",
        "Read the file /client/work/note.txt and reply with exactly its"
        " content.",
      ],  # fmt: skip
      ["Hello"],
    )
  elif S.case in ["tui", "limits"]:
    run_tui()
  elif S.case == "cache":
    run_cache()
  elif S.case == "turns":
    run_turns()
  else:
    run_stream()


def stop_clients() -> None:
  """Stop every CLI process still running, the interactive one included."""
  for client in state.clients:
    try:
      if client.poll() is None:
        os.killpg(client.pid, signal.SIGTERM)
      try:
        client.wait(timeout=6)
      except subprocess.TimeoutExpired:
        os.killpg(client.pid, signal.SIGKILL)
        client.wait(timeout=3)
    except Exception as error:  # noqa: BLE001 - recorded, not handled
      out["cleanup_failure_kind"] = type(error).__name__
  if state.tui_pid:
    try:
      if os.waitpid(state.tui_pid, os.WNOHANG) == (0, 0):
        os.killpg(state.tui_pid, signal.SIGTERM)
        time.sleep(1)
        if os.waitpid(state.tui_pid, os.WNOHANG) == (0, 0):
          os.killpg(state.tui_pid, signal.SIGKILL)
          os.waitpid(state.tui_pid, 0)
    except OSError as error:
      out["cleanup_failure_kind"] = type(error).__name__


def after_workload() -> None:
  """Record the workload's end and revoke this consumer's session.

  Taken after every CLI process is gone, on success and on timeout alike,
  so a late kill cannot make the workload look shorter than it was.
  """
  config = state.config
  assert config is not None
  out["workload_ended_utc"] = utc()
  out["credentials_file_created"] = (config / ".credentials.json").exists()
  if state.session_token:
    try:
      status, _ = session_request(
        "DELETE", "/sessions/current", token=state.session_token
      )
      out["session_revoked"] = status == 204
    except Exception as error:  # noqa: BLE001 - recorded, not handled
      out["session_revoke_failure_kind"] = type(error).__name__
  state.session_token = None


def identity_check() -> str:
  """Classify what the CLI's configuration says about the account identity.

  Returns:
    'config_missing', 'account_missing', 'email_missing',
    'unexpected_identity' or 'invalid_configuration'.
  """
  config = state.config
  assert config is not None
  identity = config / ".claude.json"
  if not identity.exists():
    return "config_missing"
  try:
    # A configuration over 1 MiB is not read either.
    if identity.stat().st_size > 1048576:
      return "invalid_configuration"
    account = json.loads(identity.read_text()).get("oauthAccount")
  except Exception:  # noqa: BLE001 - classified, not handled
    return "invalid_configuration"
  if account is None:
    return "account_missing"
  if isinstance(account, dict) and "emailAddress" not in account:
    return "email_missing"
  return "unexpected_identity"


def main() -> None:
  """Run the consumer and print what it observed."""
  global S
  S = Settings(
    **{
      key.lower(): value
      for key, value in json.loads(SETTINGS_FILE.read_text()).items()
    }
  )
  out.update(
    {
      "real_mode": S.real,
      "case": S.case,
      "cli_started": False,
      "case_ok": False,
      "cli_exit_codes": [],
      "cli_error_classes": [],
    }
  )
  signal.signal(signal.SIGTERM, interrupted)
  signal.signal(signal.SIGINT, interrupted)
  state.deadline = time.monotonic() + 55 + S.extra
  try:
    prepare()
    run_case()
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    out["failure_kind"] = type(error).__name__
    out["cli_error_classes"].append(
      "process_timeout"
      if isinstance(error, subprocess.TimeoutExpired)
      else "consumer_failure"
    )
  finally:
    stop_clients()
    if out["cli_started"]:
      after_workload()
    out["identity_check"] = "not_reached"
    if state.config is not None:
      out["identity_check"] = identity_check()
    print(json.dumps(out), flush=True)


if __name__ == "__main__":
  main()
