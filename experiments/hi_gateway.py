"""The gateway side of the hi rehearsal, run inside the gateway container.

The host (hi.py) mounts this file at /runner.py and the run's settings at
/runner-settings.json, hands the credentials on stdin once its captures
listen, and reads two things from stdout: a ready line once the gateway
accepts connections and, at the end, one JSON object with everything the
container observed. Synthetic runs also host the fake API and the fake exit
on loopback; authenticated runs start the gateway against the real upstream
and the account's real exit.
"""

import contextlib
from dataclasses import dataclass
import datetime
import hashlib
import http.client
import http.server
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import socket
import sqlite3
import ssl
import subprocess
import sys
import threading
import time
from typing import Any
import urllib.error
import urllib.parse
import urllib.request

SETTINGS_FILE = Path("/runner-settings.json")
PERSISTENCE_FAILURE = b"capture persistence failed request_id="
# The three lines the gateway writes at every start: its candidate accounts,
# behind an exit that answers no CONNECT the egress check that read no IP,
# and the retention sweep's counts.
KNOWN_LINE = re.compile(
  r"^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (?:accounts: (?P<accounts>.*)"
  r"|egress: check failed alias=(?P<alias>[^,]+), not paused:"
  r" exit IP not read: .*"
  r"|retention: deleted traffic=\d+ binding_history=\d+ sessions=\d+"
  r" vacuum=(?:true|false))$"
)
OK_EMPTY = (
  b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
  b"Content-Length: 2\r\nConnection: close\r\n\r\n{}"
)
BAD_GATEWAY = (
  b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
)
UPSERT_READING = (
  "INSERT INTO quota_latest (alias, window, utilization, status, reset_at,"
  " observed_at, source) VALUES (?, '5h', 0.95, 'allowed', ?, ?, 'experiment')"
  " ON CONFLICT (alias, window) DO UPDATE SET utilization=excluded.utilization,"
  " status=excluded.status, reset_at=excluded.reset_at,"
  " observed_at=excluded.observed_at, source=excluded.source"
)


@dataclass
class Settings:
  """The run's settings, written by the host next to this program.

  Attributes:
    case: The case being run.
    limits_arm: The limits case's arm, else None.
    limits_retry_after: The Retry-After the fake upstream puts on its 429s.
    limits_reset_seconds: How far ahead the fake upstream dates its 7d reset.
    proxy_host: The account's exit host.
    proxy_port: The account's exit port.
    auth: Whether the run has external network (real or proxy-down).
    proxy_down: Whether this is the proxy-down control arm.
    account_alias: The account's inventory ID.
    account_token: The synthetic account token the fake API expects.
    mitm: Whether the fake exit terminates proxied TLS.
    quota_fail_first: Whether the fake upstream answers the first inference
      with 401.
    quota_fail_from: The inference from which the fake upstream answers 500,
      0 for never.
    account_alias_b: The cache case's second account, else None.
    proxy_port_b: The second account's exit port, else None.
    cache_arm: The cache case's arm, else None.
    expected_egress_ip: The account's expected exit IP, from the inventory.
    expected_egress_ip_b: The second account's, else None.
  """

  case: str
  limits_arm: str | None
  limits_retry_after: int
  limits_reset_seconds: int
  proxy_host: str
  proxy_port: int
  auth: bool
  proxy_down: bool
  account_alias: str
  account_token: str
  mitm: bool
  quota_fail_first: bool
  quota_fail_from: int
  account_alias_b: str | None
  proxy_port_b: int | None
  cache_arm: str | None
  expected_egress_ip: str
  expected_egress_ip_b: str | None


@dataclass
class Held:
  """The gateway process the cleanup stops.

  Attributes:
    process: The running gateway, once started.
  """

  process: subprocess.Popen[bytes] | None = None


S: Settings
held = Held()
out: dict[str, Any] = {"ready": False}
stop = threading.Event()
servers: list[http.server.ThreadingHTTPServer] = []


def utc() -> str:
  """Return the current time in UTC as ISO 8601.

  Returns:
    The timestamp.
  """
  return datetime.datetime.now(datetime.UTC).isoformat()


def text_of(message: dict[str, Any]) -> str:
  """Return a message's text: plain content, text blocks and tool results.

  Args:
    message: One entry of the request's messages.

  Returns:
    The joined text; a tool result contributes its JSON-encoded content.
  """
  content: Any = message.get("content")
  if isinstance(content, str):
    return content
  parts = []
  for block in content:
    if block.get("type") == "text":
      parts.append(block.get("text", ""))
    elif block.get("type") == "tool_result":
      parts.append(json.dumps(block.get("content", "")))
    else:
      parts.append("")
  return " ".join(parts)


def plan(body: dict[str, Any]) -> list[str] | None:
  """Decide the fake API's reply from what the client actually sent.

  The reply depends on the request, so lost history or a missing tool
  result shows.

  Args:
    body: The request body.

  Returns:
    The text parts of the reply, or None for a tool_use reply.
  """
  messages = body.get("messages", [])
  users = [m for m in messages if m.get("role") == "user"]
  last = text_of(users[-1]) if users else ""
  tool_result = any(
    isinstance(m.get("content"), list)
    and any(b.get("type") == "tool_result" for b in m["content"])
    for m in messages
  )
  if tool_result:
    return ["Hello" if "Hello" in last else "NOFILE"]
  if body.get("tools") and "Read the file" in last:
    return None
  match = re.search(r"Count from 1 to (\d+)", last)
  if match:
    return [str(i) + "\n" for i in range(1, min(int(match.group(1)), 100) + 1)]
  match = re.search(r"Reply with exactly: (\w+)", last)
  if match:
    return [match.group(1)]
  if "first reply" in last:
    replies = [text_of(m) for m in messages if m.get("role") == "assistant"]
    return [replies[0].strip() if replies else "NOHISTORY"]
  return ["Hi"]


def quota_headers() -> list[tuple[str, str]]:
  """Return the synthetic rate-limit headers of the quota and cache cases.

  quota: the 5h utilization, a fraction, grows by 0.01 per inference
  response, so a recorded sequence that does not move is the gateway's
  doing, not the fake's. cache: benign, constant readings, so that turn 1's
  response writes the row the injection expects.

  Returns:
    Header pairs; empty in the other cases.
  """
  if S.case not in ["quota", "cache"]:
    return []
  count = (
    0
    if S.case == "cache"
    else sum(1 for r in out.get("requests", []) if r["path"] == "/v1/messages")
  )
  now = int(time.time())
  return [
    ("anthropic-ratelimit-unified-5h-utilization", str(count / 100)),
    ("anthropic-ratelimit-unified-5h-status", "allowed"),
    ("anthropic-ratelimit-unified-5h-reset", str(now + 3600)),
    ("anthropic-ratelimit-unified-7d-utilization", "0.2"),
    ("anthropic-ratelimit-unified-7d-status", "allowed"),
    ("anthropic-ratelimit-unified-7d-reset", str(now + 86400)),
  ]


def limits_headers(exhausted: bool) -> list[tuple[str, str]]:
  """Return the limits case's readings on a normal response.

  The 7d reading puts the account over hard (local-429) or is benign; the
  upstream-429 and switch arms add a rejected 5h status on their 429s
  instead (see rejection_headers).

  Args:
    exhausted: Whether the 7d reading is the exhausted one.

  Returns:
    Header pairs; empty outside the limits case.
  """
  if S.case != "limits":
    return []
  now = int(time.time())
  return [
    (
      "anthropic-ratelimit-unified-7d-utilization",
      "0.95" if exhausted else "0.2",
    ),
    (
      "anthropic-ratelimit-unified-7d-status",
      "allowed_warning" if exhausted else "allowed",
    ),
    ("anthropic-ratelimit-unified-7d-reset", str(now + S.limits_reset_seconds)),
    ("anthropic-ratelimit-unified-5h-utilization", "0.1"),
    ("anthropic-ratelimit-unified-5h-status", "allowed"),
    ("anthropic-ratelimit-unified-5h-reset", str(now + 1800)),
  ]


def rejection_headers() -> list[tuple[str, str]]:
  """Return the headers of a synthetic 429.

  Returns:
    Header pairs: a rejected 5h status and the arm's Retry-After.
  """
  return [
    ("anthropic-ratelimit-unified-5h-utilization", "1.0"),
    ("anthropic-ratelimit-unified-5h-status", "rejected"),
    ("anthropic-ratelimit-unified-5h-reset", str(int(time.time()) + 3600)),
    ("retry-after", str(S.limits_retry_after)),
  ]


def usage_for(body: dict[str, Any], account: str | None) -> dict[str, int]:
  """Return the usage block of a reply.

  The cache case keeps a cache per account: the first request of an account
  writes its prefix (system plus tools), a later one with the same prefix
  reads it. Lengths are character counts over four, enough to tell the arms
  apart.

  Args:
    body: The request body.
    account: The account the request carried ('A', 'B' or None).

  Returns:
    The usage block.
  """
  if S.case != "cache":
    return {"input_tokens": 1, "output_tokens": 0}
  prefix = json.dumps([body.get("system"), body.get("tools")], sort_keys=True)
  size = max(1, len(prefix) // 4)
  digest = hashlib.sha256(prefix.encode()).hexdigest()
  seen = out.setdefault("cache_prefixes", {})
  if seen.get(account) == digest:
    return {
      "input_tokens": 5,
      "cache_read_input_tokens": size,
      "cache_creation_input_tokens": 7,
      "output_tokens": 0,
    }
  seen[account] = digest
  return {
    "input_tokens": 5,
    "cache_read_input_tokens": 0,
    "cache_creation_input_tokens": size,
    "output_tokens": 0,
  }


def message_shell(usage: dict[str, int]) -> dict[str, Any]:
  """Return the reply's message object without content.

  Args:
    usage: The usage block.

  Returns:
    The message.
  """
  return {
    "id": "synthetic-message",
    "type": "message",
    "role": "assistant",
    "model": "claude-sonnet-4-6",
    "content": [],
    "stop_reason": None,
    "stop_sequence": None,
    "usage": usage,
  }


def reply_events(
  parts: list[str] | None, usage: dict[str, int]
) -> list[tuple[str, dict[str, Any]]]:
  """Build the server-sent events of a streamed reply.

  Args:
    parts: The text parts, or None for a tool_use reply.
    usage: The usage block of message_start.

  Returns:
    Pairs of event name and event.
  """
  events: list[tuple[str, dict[str, Any]]] = [
    (
      "message_start",
      {"type": "message_start", "message": message_shell(usage)},
    )
  ]
  if parts is None:
    block = {
      "type": "tool_use",
      "id": "toolu_synthetic",
      "name": "Read",
      "input": {},
    }
    events += [
      (
        "content_block_start",
        {"type": "content_block_start", "index": 0, "content_block": block},
      ),
      (
        "content_block_delta",
        {
          "type": "content_block_delta",
          "index": 0,
          "delta": {
            "type": "input_json_delta",
            "partial_json": json.dumps({"file_path": "/client/work/note.txt"}),
          },
        },
      ),
    ]
  else:
    events += [
      (
        "content_block_start",
        {
          "type": "content_block_start",
          "index": 0,
          "content_block": {"type": "text", "text": ""},
        },
      )
    ]
    events += [
      (
        "content_block_delta",
        {
          "type": "content_block_delta",
          "index": 0,
          "delta": {"type": "text_delta", "text": part},
        },
      )
      for part in parts
    ]
  events += [
    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
    (
      "message_delta",
      {
        "type": "message_delta",
        "delta": {
          "stop_reason": "tool_use" if parts is None else "end_turn",
          "stop_sequence": None,
        },
        "usage": {"output_tokens": 1},
      },
    ),
    ("message_stop", {"type": "message_stop"}),
  ]
  return events


def whole_message(
  parts: list[str] | None, usage: dict[str, int]
) -> dict[str, Any]:
  """Build the message object of an unstreamed reply.

  Args:
    parts: The text parts, or None.
    usage: The usage block.

  Returns:
    The message with its text and end_turn.
  """
  message = message_shell(usage)
  message["content"] = [{"type": "text", "text": "".join(parts or [])}]
  message["stop_reason"] = "end_turn"
  return message


class API(http.server.BaseHTTPRequestHandler):
  """The fake Anthropic API on loopback: records requests, answers by plan."""

  def log_message(self, format: str, *args: Any) -> None:
    """Drop the server's own log; the requests are recorded in the output.

    Args:
      format: The log line's format.
      *args: Its arguments.
    """

  def send_json(
    self,
    status: int,
    data: bytes,
    headers: list[tuple[str, str]] | None = None,
  ) -> None:
    """Send a JSON body with its content headers.

    Args:
      status: The status code.
      data: The encoded body.
      headers: Headers sent before the content headers.
    """
    self.send_response(status)
    for key, value in headers or []:
      self.send_header(key, value)
    self.send_header("Content-Type", "application/json")
    self.send_header("Content-Length", str(len(data)))
    self.end_headers()
    self.wfile.write(data)

  def limits_error(self, status: int, kind: str, label: str) -> None:
    """Send a synthetic upstream error; a 429 carries rejection headers.

    Args:
      status: The status code.
      kind: The error type in the body.
      label: What the message says the upstream did.
    """
    data = json.dumps(
      {
        "type": "error",
        "error": {"type": kind, "message": "synthetic upstream " + label},
      }
    ).encode()
    self.send_json(status, data, rejection_headers() if status == 429 else None)

  def record(self, body: dict[str, Any], path: str) -> str | None:
    """Append the request's shape to the output and name its account.

    Args:
      body: The request body.
      path: The request path without its query.

    Returns:
      'A' or 'B' for the two synthetic tokens, None for anything else.
    """
    authorization = self.headers.get("Authorization") or ""
    account = (
      "A"
      if authorization == "Bearer " + S.account_token
      else "B"
      if authorization == "Bearer " + S.account_token + "-b"
      else None
    )
    messages = body.get("messages", [])
    out.setdefault("requests", []).append(
      {
        "at_utc": utc(),
        "account": account,
        "path": path,
        "method": self.command,
        "account_authorization": account is not None,
        "session_credential_forwarded": authorization.startswith(
          "Bearer sk-ant-gws-"
        ),
        "oauth_capability": "oauth-2025-04-20"
        in self.headers.get("anthropic-beta", "").split(","),
        "header_names": sorted(self.headers.keys()),
        "body_keys": sorted(body.keys()),
        "stream": body.get("stream") is True,
        "messages": len(messages),
        "roles": [m.get("role") for m in messages][:40],
      }
    )
    return account

  def refused(self, path: str, account: str | None) -> bool:
    """Answer the requests that never reach the plan.

    Args:
      path: The request path.
      account: The account the request carried.

    Returns:
      Whether a reply was sent.
    """
    posts = sum(1 for r in out["requests"] if r["path"] == "/v1/messages")
    if (
      path == "/v1/messages"
      and S.quota_fail_from
      and posts >= S.quota_fail_from
    ):
      self.send_json(
        500,
        b'{"type":"error","error":{"type":"api_error","message":"synthetic"}}',
      )
      return True
    if S.quota_fail_first and path == "/v1/messages" and posts == 1:
      self.send_json(
        401,
        b'{"type":"error","error":{"type":"authentication_error",'
        b'"message":"synthetic"}}',
      )
      return True
    if path == "/v1/messages/count_tokens":
      self.send_json(200, b'{"input_tokens":1}')
      return True
    if path != "/v1/messages":
      self.send_error(404)
      return True
    return self.limits_refusal(account)

  def limits_refusal(self, account: str | None) -> bool:
    """Apply the limits arm from an account's second request on.

    Turn 1 of each account is a normal reply; what happens from the second
    request on is the arm.

    Args:
      account: The account the request carried.

    Returns:
      Whether an error was sent.
    """
    if S.case != "limits":
      return False
    nth = sum(
      1
      for r in out["requests"]
      if r["path"] == "/v1/messages" and r["account"] == account
    )
    if nth < 2 or account != "A":
      return False
    if S.limits_arm in ["upstream-429", "switch"]:
      self.limits_error(429, "rate_limit_error", "rate limit")
      return True
    if S.limits_arm == "local-503":
      self.limits_error(401, "authentication_error", "authentication failure")
      return True
    if S.limits_arm == "upstream-503":
      self.limits_error(503, "api_error", "unavailable")
      return True
    return False

  def send_stream(
    self,
    events: list[tuple[str, dict[str, Any]]],
    headers: list[tuple[str, str]],
    slow: bool,
  ) -> None:
    """Send a reply as server-sent events, slowly when asked.

    The long case asks for thousands of numbers; stand in for it with a slow
    reply of minutes.

    Args:
      events: The events.
      headers: Headers sent before the content headers.
      slow: Whether to pause between chunks.
    """
    chunks = [
      ("event: " + name + "\ndata: " + json.dumps(event) + "\n\n").encode()
      for name, event in events
    ]
    self.send_response(200)
    for key, value in headers:
      self.send_header(key, value)
    self.send_header("Content-Type", "text/event-stream")
    self.send_header("Content-Length", str(sum(map(len, chunks))))
    self.end_headers()
    try:
      for chunk in chunks:
        self.wfile.write(chunk)
        self.wfile.flush()
        if slow:
          time.sleep(1.4 if S.case == "long" else 0.05)
    except OSError:
      out["upstream_write_aborted"] = True

  def do_POST(self) -> None:  # noqa: N802 - the base class dispatches on the name
    """Record the request and answer it as the case demands."""
    data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
    body = json.loads(data)
    path = self.path.split("?")[0]
    account = self.record(body, path)
    if self.refused(path, account):
      return
    parts = plan(body)
    usage = usage_for(body, account)
    headers = quota_headers() + limits_headers(S.limits_arm == "local-429")
    if body.get("stream"):
      slow = bool(parts and len(parts) > 1)
      self.send_stream(reply_events(parts, usage), headers, slow)
    else:
      self.send_json(
        200, json.dumps(whole_message(parts, usage)).encode(), headers
      )


def read_head(tls: ssl.SSLSocket) -> bytes:
  """Read one request head from a decrypted tunnel.

  Args:
    tls: The server side of the tunnel.

  Returns:
    The bytes up to the blank line, at most 16 KiB.
  """
  head = b""
  while b"\r\n\r\n" not in head and len(head) < 16384:
    chunk = tls.recv(4096)
    if not chunk:
      break
    head += chunk
  return head


def describe_head(head: bytes) -> tuple[str, dict[str, Any]]:
  """Reduce a decrypted request head to its shape; values stay here.

  Args:
    head: The request head.

  Returns:
    The request path and the entry recorded for it.
  """
  lines = head.split(b"\r\n\r\n", 1)[0].decode("latin-1").split("\r\n")
  method, url = lines[0].split(" ")[:2]
  headers = {
    name.strip().lower(): value.strip()
    for name, _, value in (line.partition(":") for line in lines[1:])
  }
  target = urllib.parse.urlsplit(url)
  authorization = headers.get("authorization")
  kind = (
    "absent"
    if authorization is None
    else "session_bearer"
    if authorization.startswith("Bearer sk-ant-gws-")
    else "other_present"
  )
  return target.path, {
    "method": method[:10],
    "path": target.path[:200],
    "query_keys": sorted(urllib.parse.parse_qs(target.query))[:20],
    "header_names": sorted(headers)[:60],
    "authorization": kind,
  }


class Exit(http.server.BaseHTTPRequestHandler):
  """Synthetic stand-in for the account's Oxylabs port.

  It sits behind the gateway's two entrypoints: a CONNECT is logged, then
  refused (or, with --mitm, answered locally); an absolute-URI request,
  which is how the reverse proxy reaches the http:// fake API through this
  exit, is relayed to it. Authenticated runs use the account's real exit
  instead.

  Attributes:
    protocol_version: HTTP/1.1, so that a relayed response can be streamed.
    do_GET: Relayed, like every method but CONNECT.
    do_POST: Relayed.
    do_HEAD: Relayed.
    do_PUT: Relayed.
    do_DELETE: Relayed.
    do_OPTIONS: Relayed.
  """

  protocol_version = "HTTP/1.1"

  def log_message(self, format: str, *args: Any) -> None:
    """Drop the server's own log; the tunnels are recorded in the output.

    Args:
      format: The log line's format.
      *args: Its arguments.
    """

  def relay(self) -> None:
    """Relay an absolute-URI request to the fake API.

    The exit credentials the gateway attached are counted, never kept.
    """
    target = urllib.parse.urlsplit(self.path)
    entry = self.note(target.netloc)
    entry["relayed"] = True
    entry["exit_credentials_present"] = (
      self.headers.get("Proxy-Authorization") is not None
    )
    if target.netloc != "127.0.0.1:8790":
      self.send_error(502)
      return
    body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
    headers = {
      key: value
      for key, value in self.headers.items()
      if key.lower() not in ["proxy-authorization", "proxy-connection"]
    }
    upstream = http.client.HTTPConnection("127.0.0.1", 8790, timeout=600)
    query = "?" + target.query if target.query else ""
    upstream.request(
      self.command, target.path + query, body=body, headers=headers
    )
    response = upstream.getresponse()
    self.send_response(response.status)
    for key, value in response.getheaders():
      if key.lower() not in ["transfer-encoding", "connection"]:
        self.send_header(key, value)
    self.send_header("Connection", "close")
    self.end_headers()
    with contextlib.suppress(OSError):
      while True:
        chunk = response.read(4096)
        if not chunk:
          break
        self.wfile.write(chunk)
        self.wfile.flush()
    self.close_connection = True

  def note(self, target: str) -> dict[str, Any]:
    """Append a proxy log entry, up to the log's cap.

    Args:
      target: The request's target (host:port).

    Returns:
      The entry, for the handler to fill in.
    """
    entry: dict[str, Any] = {
      "method": self.command,
      "target": target[:120],
      "started_utc": utc(),
    }
    if len(out.setdefault("proxy_log", [])) < 200:
      out["proxy_log"].append(entry)
    else:
      out["proxy_log_overflow"] = True
    return entry

  do_GET = do_POST = do_HEAD = do_PUT = do_DELETE = do_OPTIONS = relay  # noqa: N815

  def do_CONNECT(self) -> None:  # noqa: N802 - the base class dispatches on the name
    """Log a tunnel request, then refuse it or answer it locally."""
    entry = self.note(self.path)
    self.close_connection = True
    if S.mitm:
      self.answer_tunnel(entry)
      return
    entry["refused"] = True
    self.send_error(502)

  def answer_tunnel(self, entry: dict[str, Any]) -> None:
    """Terminate the tunnel's TLS and read one request head.

    Values other than the path stay here. The interactive first-run check
    needs its two hello probes to succeed; everything else is refused.

    Args:
      entry: The proxy log entry to fill in.
    """
    self.send_response(200, "Connection Established")
    self.end_headers()
    try:
      context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
      context.load_cert_chain("/mitm.pem")
      with context.wrap_socket(self.connection, server_side=True) as tls:
        tls.settimeout(5)
        path, entry["decrypted"] = describe_head(read_head(tls))
        entry["answered"] = 200 if path.endswith("/hello") else 502
        tls.sendall(OK_EMPTY if entry["answered"] == 200 else BAD_GATEWAY)
    except (OSError, ValueError) as error:
      entry["tls_failure_kind"] = type(error).__name__


def accounts_in_play() -> list[str]:
  """Return the aliases the gateway serves in this run.

  Returns:
    The account, the switch arm's second synthetic account and the cache
    case's account B, as present.
  """
  aliases = [S.account_alias]
  if S.limits_arm == "switch":
    aliases.append(S.account_alias + "-b")
  if S.case == "cache" and S.account_alias_b:
    aliases.append(S.account_alias_b)
  return aliases


def seed_plans(aliases: list[str]) -> None:
  """Put every account on pro from the epoch before the gateway starts.

  Since #88 an account without a plan is no candidate. The subcommand
  creates and migrates the database file. Real runs get the same seed, so
  the inventory's plan column is not consulted here.

  Args:
    aliases: The accounts.

  Raises:
    RuntimeError: When the seeding subcommand fails.
  """
  for alias in aliases:
    seeded = subprocess.run(
      [
        "/gateway",
        "claude",
        "plan",
        "--session-db",
        "/tmp/gateway.sqlite",
        "schedule",
        alias,
        "pro",
        "1970-01-01T00:00:00Z",
      ],  # fmt: skip
      env={"PATH": "/usr/bin:/bin"},
      capture_output=True,
      timeout=20,
    )
    if seeded.returncode != 0:
      raise RuntimeError("plan seed failed")
  out["plans_seeded"] = list(aliases)


def account_entries(
  credentials: dict[str, str] | None,
) -> dict[str, dict[str, str]]:
  """Build the accounts' fields: token and exit per alias.

  The switch arm has a second synthetic account behind the same fake exit;
  the fake upstream tells them apart by token. The cache case's account B
  has its own token and proxy port on the same exit credentials (real), or a
  second synthetic token behind the fake exit. The gateway checks each exit
  against expected_egress_ip at startup; behind the fake exit the check
  reads no IP and pauses nothing, so a synthetic account gets a
  documentation address.

  Args:
    credentials: The resolved credentials in authenticated runs, else None.

  Returns:
    Fields per alias.
  """
  creds = credentials or {}
  if S.auth:
    account = {
      "token": creds["token"],
      "proxy_url": "http://" + S.proxy_host + ":" + str(S.proxy_port),
      "proxy_username": creds["proxy_username"],
      "proxy_password": creds["proxy_password"],
      "expected_egress_ip": S.expected_egress_ip,
    }
  else:
    account = {
      "token": S.account_token,
      "proxy_url": "http://127.0.0.1:8793",
      "proxy_username": "synthetic-exit-user",
      "proxy_password": "synthetic-exit-password",
      "expected_egress_ip": "192.0.2.1",
    }
  accounts = {S.account_alias: account}
  if S.limits_arm == "switch":
    accounts[S.account_alias + "-b"] = dict(
      account, token=S.account_token + "-b"
    )
  if S.case == "cache" and S.account_alias_b:
    accounts[S.account_alias_b] = (
      dict(
        account,
        token=creds["token_b"],
        proxy_url="http://" + S.proxy_host + ":" + str(S.proxy_port_b),
        expected_egress_ip=str(S.expected_egress_ip_b),
      )
      if S.auth
      else dict(account, token=S.account_token + "-b")
    )
  return accounts


def write_inventory(accounts: dict[str, dict[str, str]]) -> dict[str, str]:
  """Write one synthetic inventory file per account.

  The gateway reads an inventory directory and resolves its references
  (ADR 0018). The files hold only the keys the gateway consumes, with
  per-alias references; the values go to the gateway as one JSON object of
  reference to value on its private stdin pipe, the test entry that reads
  no vault.

  Args:
    accounts: Fields per alias.

  Returns:
    Reference to value.
  """
  os.makedirs("/tmp/inventory", exist_ok=True)
  values = {}
  for alias, fields in accounts.items():
    host, port = fields["proxy_url"].removeprefix("http://").rsplit(":", 1)
    references = {
      field: "op://synthetic/" + alias + "/" + field
      for field in ["token", "proxy_username", "proxy_password"]
    }
    Path("/tmp/inventory/" + alias + ".yaml").write_text(
      "access: gateway\nstatus: active\noauth_token: "
      + references["token"]
      + "\nproxy:\n  host: "
      + host
      + "\n  port: "
      + port
      + "\n  expected_egress_ip: "
      + fields["expected_egress_ip"]
      + "\n  username_ref: "
      + references["proxy_username"]
      + "\n  password_ref: "
      + references["proxy_password"]
      + "\n"
    )
    for field, reference in references.items():
      values[reference] = fields[field]
  return values


def wait_for_gateway(process: subprocess.Popen[bytes]) -> None:
  """Wait until the gateway's two listeners and the session socket are up.

  Args:
    process: The gateway.

  Raises:
    RuntimeError: When the gateway stops or is not ready within 8 seconds.
  """
  deadline = time.monotonic() + 8
  while time.monotonic() < deadline and not stop.is_set():
    if process.poll() is not None:
      raise RuntimeError("gateway stopped")
    with contextlib.suppress(OSError):
      for port in [8789, 8791]:
        with socket.create_connection(("127.0.0.1", port), timeout=0.2):
          pass
      if os.path.exists("/capture/session.sock"):
        return
    stop.wait(0.1)
  raise RuntimeError("gateway not ready")


def start_gateway(credentials: dict[str, str] | None) -> None:
  """Start the single-entry gateway and hand it the credentials on stdin.

  The process is held for the cleanup as soon as it exists, ready or not.
  The session database lives in this container's /tmp; the socket sits in
  the shared capture directory so the client container can bind-mount it
  and issue its own session, the way the launcher will.

  Args:
    credentials: The resolved credentials in authenticated runs, else None.
  """
  if os.path.exists("/capture/session.sock"):
    os.unlink("/capture/session.sock")
  seed_plans(accounts_in_play())
  values = write_inventory(account_entries(credentials))
  upstream = "https://api.anthropic.com" if S.auth else "http://127.0.0.1:8790"
  with open("/tmp/gateway.yaml", "w", encoding="utf-8") as config:
    config.write(
      f"upstream: {upstream}\n"
      "listen:\n"
      "  reverse: 127.0.0.1:8789\n"
      "  forward: 127.0.0.1:8791\n"
      "session_socket: /capture/session.sock\n"
      "admin_socket: /tmp/admin.sock\n"
      "session_db: /tmp/gateway.sqlite\n"
      "inventory: /tmp/inventory\n"
      "capture_dir: /capture\n"
      "credentials:\n"
      "  stdin_test_entry: true\n"
    )
  command = ["/gateway", "claude", "serve", "--config", "/tmp/gateway.yaml"]
  process = subprocess.Popen(
    command,
    env={"PATH": "/usr/bin:/bin"},
    stdin=subprocess.PIPE,
    stdout=subprocess.DEVNULL,
    stderr=subprocess.PIPE,
    start_new_session=True,
  )
  held.process = process
  stdin = process.stdin
  assert stdin is not None
  stdin.write(json.dumps(values).encode())
  stdin.close()
  wait_for_gateway(process)


def capture_controls() -> None:
  """Send a known DNS query and TLS ClientHello on loopback.

  The host must find both in its capture.
  """
  labels = [b"capture-control", b"invalid"]
  query = (
    b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00"
    + b"".join(bytes([len(label)]) + label for label in labels)
    + b"\x00\x00\x01\x00\x01"
  )
  with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sender:
    sender.sendto(query, ("127.0.0.1", 53))
  with socket.socket() as listener:
    listener.bind(("127.0.0.1", 8792))
    listener.listen(1)
    raw = socket.create_connection(("127.0.0.1", 8792), timeout=2)
    peer, _ = listener.accept()
    context = ssl.create_default_context()
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE

    def hello() -> None:
      try:
        context.wrap_socket(
          raw, server_hostname="capture-control.invalid"
        ).close()
      except OSError:
        raw.close()

    thread = threading.Thread(target=hello)
    thread.start()
    peer.settimeout(2)
    peer.recv(4096)
    peer.close()
    thread.join(5)


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


def egress_once(
  credentials: dict[str, str], authenticated: bool
) -> dict[str, Any]:
  """Fetch the exit IP through the account's proxy once.

  Only status, exit IP and a 407 flag leave.

  Args:
    credentials: The resolved exit credentials.
    authenticated: Whether to send them.

  Returns:
    The reading.
  """
  user = ""
  if authenticated:
    user = (
      urllib.parse.quote(credentials["proxy_username"], safe="")
      + ":"
      + urllib.parse.quote(credentials["proxy_password"], safe="")
      + "@"
    )
  proxy = "http://" + user + S.proxy_host + ":" + str(S.proxy_port)
  opener = urllib.request.build_opener(
    urllib.request.ProxyHandler({"https": proxy})
  )
  try:
    with opener.open("https://ip.oxylabs.io/location", timeout=15) as response:
      data = response.read(65536).decode("utf-8", errors="replace")
      return {"status": response.status, "ip": exit_address(data)}
  except urllib.error.HTTPError as error:
    return {"status": error.code}
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    return {
      "failure_kind": type(error).__name__,
      "proxy_407": "407" in str(error),
    }


def egress(credentials: dict[str, str]) -> dict[str, Any]:
  """Make one request with proxy credentials and one without.

  Args:
    credentials: The resolved exit credentials.

  Returns:
    The two readings by label.
  """
  return {
    "authenticated": egress_once(credentials, True),
    "unauthenticated": egress_once(credentials, False),
  }


def serve(credentials: dict[str, str] | None) -> None:
  """Start the fakes and the gateway, then keep watch until told to stop.

  Args:
    credentials: What the host sent: resolved credentials or None.
  """
  if S.proxy_down:
    out["proxy_name_is_loopback"] = (
      socket.gethostbyname(S.proxy_host) == "127.0.0.1"
    )
    # Control: this namespace can reach the API directly, so a direct fallback
    # would be visible.
    try:
      socket.create_connection(("api.anthropic.com", 443), timeout=5).close()
      out["direct_control_connected"] = True
    except OSError:
      out["direct_control_connected"] = False
    time.sleep(0.5)
  handlers: list[tuple[int, type[http.server.BaseHTTPRequestHandler]]] = (
    [] if S.auth else [(8793, Exit), (8790, API)]
  )
  for port, handler in handlers:
    server = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    servers.append(server)
  start_gateway(credentials)
  # The restart case keeps the input in this process until the gateway has
  # been started twice.
  if S.case != "restart":
    credentials = None
  out["ready"] = True
  print(json.dumps({"ready": True}), flush=True)
  if S.case == "restart":
    restart_once(credentials)
  watch_gateway()


def restart_once(credentials: dict[str, str] | None) -> None:
  """Restart the gateway with the same input when the host asks for it.

  The host asks once, by creating a file in the shared capture directory.

  Args:
    credentials: What the host sent.

  Raises:
    RuntimeError: When the gateway exits before the request arrives.
  """
  process = held.process
  assert process is not None
  while not stop.wait(0.2) and not os.path.exists("/capture/restart-request"):
    if process.poll() is not None:
      raise RuntimeError("gateway exited")
  if stop.is_set():
    return
  os.killpg(process.pid, signal.SIGTERM)
  _, first_stderr = process.communicate(timeout=10)
  out["first_gateway_exit_code"] = process.returncode
  out["first_gateway_stderr_present"] = stderr_flag(
    first_stderr, accounts_in_play()
  )
  start_gateway(credentials)
  out["restarted"] = True
  print(json.dumps({"ready": True}), flush=True)


def watch_gateway() -> None:
  """Keep watch over the gateway until the host stops the container.

  Raises:
    RuntimeError: When the gateway exits on its own.
  """
  process = held.process
  assert process is not None
  while not stop.wait(0.2):
    if process.poll() is not None:
      raise RuntimeError("gateway exited")
    if (
      S.case == "cache"
      and os.path.exists("/signal/exhaust-request")
      and not os.path.exists("/signal/exhaust-done")
    ):
      inject_exhaustion()


def inject_exhaustion() -> None:
  """Put account A's 5h reading at or over hard, as the client asks.

  The client asks after turn 1. The row that turn 1's response wrote must
  already be there (observed_at not before the turn), or the injected value
  could be overwritten by it; the reset lies in the future, or the reading
  would not count.
  """
  request = json.loads(Path("/signal/exhaust-request").read_text())
  done: dict[str, Any] = {"at_utc": utc(), "ok": False}
  try:
    db = sqlite3.connect("/tmp/gateway.sqlite", timeout=5)
    db.row_factory = sqlite3.Row
    row = db.execute(
      "SELECT utilization, observed_at FROM quota_latest"
      " WHERE alias=? AND window='5h'",
      (S.account_alias,),
    ).fetchone()
    done["prior"] = dict(row) if row else None
    if row is None or row["observed_at"] < request["turn1_started_ms"]:
      done["reason"] = "turn 1 reading not landed"
    else:
      now_ms = int(time.time() * 1000)
      reset_ms = now_ms + 5 * 3600 * 1000
      db.execute(UPSERT_READING, (S.account_alias, reset_ms, now_ms))
      db.commit()
      done.update(
        {
          "ok": True,
          "utilization": 0.95,
          "reset_at_ms": reset_ms,
          "observed_at_ms": now_ms,
        }
      )
    db.close()
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    done["failure_kind"] = type(error).__name__
  out["injection"] = done
  Path("/signal/exhaust-done.tmp").write_text(json.dumps(done))
  os.replace("/signal/exhaust-done.tmp", "/signal/exhaust-done")


def unknown_stderr(stderr: bytes, aliases: list[str]) -> bool:
  """Report whether the gateway wrote anything beyond its known lines.

  Known are persistence failures (counted separately), the startup list of
  candidate accounts when it names only the run's aliases, an egress check
  that read no IP for one of the run's aliases, and the retention sweep's
  counts. Any other line, and a known shape with an alias from elsewhere,
  counts.

  Args:
    stderr: The gateway's stderr.
    aliases: The run's synthetic aliases.

  Returns:
    True when an unknown line is present.
  """
  for raw in stderr.splitlines():
    line = raw.decode("utf-8", errors="replace")
    if PERSISTENCE_FAILURE.decode() in line:
      continue
    match = KNOWN_LINE.match(line)
    if match is None:
      return True
    if match.group("accounts") is not None:
      listed = match.group("accounts").split()
      if not listed or not set(listed) <= set(aliases):
        return True
    elif (
      match.group("alias") is not None and match.group("alias") not in aliases
    ):
      return True
  return False


def stderr_flag(stderr: bytes, aliases: list[str]) -> bool:
  """Report whether a gateway's stderr holds anything the run must not accept.

  Used for the first gateway of a restart run, which has no separate
  persistence failure count: an unknown line and a persistence failure both
  count.

  Args:
    stderr: The gateway's stderr.
    aliases: The run's synthetic aliases.

  Returns:
    True when an unknown line or a persistence failure is present.
  """
  return PERSISTENCE_FAILURE in stderr or unknown_stderr(stderr, aliases)


def collect(process: subprocess.Popen[bytes]) -> None:
  """Stop the gateway and record its exit and stderr shape.

  Args:
    process: The gateway.
  """
  try:
    if process.poll() is None:
      os.killpg(process.pid, signal.SIGTERM)
    try:
      _, gateway_stderr = process.communicate(timeout=6)
    except subprocess.TimeoutExpired:
      os.killpg(process.pid, signal.SIGKILL)
      _, gateway_stderr = process.communicate(timeout=3)
    out["capture_persistence_failure_count"] = gateway_stderr.count(
      PERSISTENCE_FAILURE
    )
    out["other_gateway_stderr_present"] = unknown_stderr(
      gateway_stderr, accounts_in_play()
    )
    out["gateway_exit_code"] = process.returncode
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    out["cleanup_failure_kind"] = type(error).__name__


def read_store() -> None:
  """Read the gateway's own tables once it has stopped.

  The credential is stored only as a hash, which is not read; launch
  metadata is reduced to its key names.
  """
  try:
    db = sqlite3.connect("/tmp/gateway.sqlite")
    db.row_factory = sqlite3.Row
    out["sessions"] = [
      {
        "id": row["id"],
        "client_machine": row["client_machine"],
        "launch_meta_keys": sorted(json.loads(row["launch_meta"]))
        if row["launch_meta"]
        else None,
        "ended": row["ended_at"] is not None,
        "end_reason": row["end_reason"],
      }
      for row in db.execute(
        "SELECT id, client_machine, launch_meta, ended_at, end_reason"
        " FROM sessions ORDER BY created_at LIMIT 64"
      )
    ]
    out["bindings"] = [
      dict(row)
      for row in db.execute(
        "SELECT session_id, alias, reason FROM bindings"
        " ORDER BY bound_at LIMIT 64"
      )
    ]
    out["traffic"] = [
      dict(row)
      for row in db.execute(
        "SELECT ts, session_id, alias, kind, host, port, bytes_up, bytes_down,"
        " duration_ms, result, status, claude_session_id, claude_agent_id"
        " FROM traffic ORDER BY id LIMIT 200"
      )
    ]
    out["traffic_rows"] = db.execute("SELECT COUNT(*) FROM traffic").fetchone()[
      0
    ]
    out["account_state"] = [
      dict(row)
      for row in db.execute(
        "SELECT alias, paused_until, pause_reason FROM account_state"
        " ORDER BY alias"
      )
    ]
    out["binding_history"] = [
      dict(row)
      for row in db.execute(
        "SELECT session_id, alias, from_ts, to_ts, reason FROM binding_history"
        " ORDER BY id LIMIT 64"
      )
    ]
    out["quota_latest"] = [
      dict(row)
      for row in db.execute(
        "SELECT alias, window, utilization, status, reset_at, observed_at,"
        " source FROM quota_latest ORDER BY alias, window"
      )
    ]
    db.close()
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    out["store_read_failure_kind"] = type(error).__name__


def main() -> None:
  """Run the gateway side and print what it observed."""
  global S
  S = Settings(
    **{
      key.lower(): value
      for key, value in json.loads(SETTINGS_FILE.read_text()).items()
    }
  )
  signal.signal(signal.SIGTERM, lambda *_: stop.set())
  signal.signal(signal.SIGINT, lambda *_: stop.set())
  try:
    # The host writes this line once its captures are listening; synthetic
    # mode sends null.
    credentials = json.loads(sys.stdin.readline(4097))
    capture_controls()
    if S.case == "egress":
      out["egress"] = egress(credentials)
    else:
      serve(credentials)
  except Exception as error:  # noqa: BLE001 - recorded, not handled
    out["failure_kind"] = type(error).__name__
  finally:
    credentials = None
    if held.process:
      collect(held.process)
      read_store()
    for server in servers:
      server.shutdown()
      server.server_close()
    print(json.dumps(out), flush=True)


if __name__ == "__main__":
  main()
