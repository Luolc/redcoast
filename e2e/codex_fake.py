"""Fake upstream and fake exit of the Codex rehearsal, run in the gateway container.

The upstream answers every POST that ends in /responses with a short SSE stream
that carries the x-codex-primary-* quota headers and no Content-Type, as the
ChatGPT backend does, and every POST to /v1/messages with a Claude message; it
listens on loopback for the gateways' exits and on the container's address for
the direct control arms. The exit answers CONNECT with 200 and echoes, and
relays absolute-URI requests to the upstream, as an account's inference goes
through its exit. Each request and each CONNECT is one JSON line in the record
file. Credentials are recorded only as their SHA-256; the installation IDs are
synthetic UUIDs and are recorded as they are.

The upstream is also the token endpoint (POST /oauth/token, as the gateways'
codex.token_url): each refresh takes REFRESH_SECONDS, so that refreshes that
overlap show up, and answers a refresh token it has seen before with
refresh_token_reused, as auth.openai.com does. Its record says how many
refreshes were in flight when it arrived and how often its refresh token had
been presented before. Once it has issued an access token, the responses
endpoint takes only the latest one and answers any other with 401; a POST to
/control/revoke from loopback makes it refuse the latest one too, until the
next refresh.

  python3 codex_fake.py <record file> <direct address> <email> <account ID>
"""

import base64
import hashlib
import http.server
import json
import secrets
import socket
import sys
import threading
import time
import urllib.parse

UPSTREAM = ("127.0.0.1", 18790)
EXITS = [("127.0.0.1", 18798), ("127.0.0.1", 18799)]
DIRECT_PORT = 18791
REPLY = "redcoast-rehearsal-ok"
QUOTA_HEADERS = {
  "x-codex-primary-used-percent": "12",
  "x-codex-primary-window-minutes": "300",
  "x-codex-primary-reset-at": "4102444800",
}

REFRESH_SECONDS = 1.5

lock = threading.Lock()
# The record file, the login's email and its account ID, from the command line.
record_file = email = account_id = ""
# The token endpoint's state: the refreshes in flight, the SHA-256 of every
# refresh token presented with its count, and the access token the responses
# endpoint takes (None: any, before the first refresh; "": none).
tokens = {"in_flight": 0, "presented": {}, "accepted": None}


def record(entry: dict) -> None:
  """Append one entry to the record file.

  Args:
    entry: The fields to write.
  """
  with lock, open(record_file, "a") as out:
    out.write(json.dumps(entry) + "\n")


def digest(value: str | None) -> str | None:
  """Return the SHA-256 of a header value, or None when it is absent.

  Args:
    value: The value.

  Returns:
    Lowercase hex, or None.
  """
  return hashlib.sha256(value.encode()).hexdigest() if value else None


def mint(claims: dict) -> str:
  """Return a made-up JWT with claims and a random signature.

  Args:
    claims: The payload.

  Returns:
    The token.
  """

  def part(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

  header = part(b'{"alg":"RS256","typ":"JWT"}')
  payload = part(json.dumps(claims).encode())
  return f"{header}.{payload}.{part(secrets.token_bytes(32))}"


def refresh(body: bytes) -> tuple[int, dict, dict]:
  """Serve one refresh of the token endpoint.

  Args:
    body: The request body, JSON.

  Returns:
    The status, the answer and the record.
  """
  try:
    request = json.loads(body)
  except ValueError:
    request = {}
  presented = digest(request.get("refresh_token"))
  with lock:
    tokens["in_flight"] += 1
    entry = {
      "kind": "token",
      "grant_type": request.get("grant_type"),
      "reason": request.get("refresh_reason"),
      "refresh_sha256": presented,
      "in_flight": tokens["in_flight"],
      "presented_before": tokens["presented"].get(presented, 0),
    }
    tokens["presented"][presented] = entry["presented_before"] + 1
  time.sleep(REFRESH_SECONDS)
  with lock:
    tokens["in_flight"] -= 1
    if entry["presented_before"] or not presented:
      entry["status"] = 401
      return 401, {"error": {"code": "refresh_token_reused"}}, entry
    claims = {
      "email": email,
      "https://api.openai.com/auth": {"chatgpt_account_id": account_id},
      "exp": 4102444800,
    }
    answer = {
      "id_token": mint(claims),
      "access_token": mint(claims),
      "refresh_token": "synthetic-refresh-" + secrets.token_hex(16),
    }
    tokens["accepted"] = answer["access_token"]
    entry.update(
      status=200,
      issued_access_sha256=digest("Bearer " + answer["access_token"]),
      issued_refresh_sha256=digest(answer["refresh_token"]),
    )
  return 200, answer, entry


def installation_ids(headers, body: bytes) -> dict:
  """Read the installation ID from the three places a Codex request carries it.

  Args:
    headers: The request headers.
    body: The request body, uncompressed.

  Returns:
    The ID in the x-codex-turn-metadata header, in client_metadata's
    x-codex-installation-id and in client_metadata's x-codex-turn-metadata,
    each None when absent or unreadable.
  """

  def from_metadata(text):
    try:
      return json.loads(text).get("installation_id")
    except (TypeError, ValueError, AttributeError):
      return None

  try:
    metadata = json.loads(body).get("client_metadata") or {}
  except (ValueError, AttributeError):
    metadata = {}
  return {
    "header": from_metadata(headers.get("x-codex-turn-metadata")),
    "body": metadata.get("x-codex-installation-id"),
    "body_turn_metadata": from_metadata(metadata.get("x-codex-turn-metadata")),
  }


def sse_events(model: str) -> list[dict]:
  """Return the events of one completed response that says REPLY.

  Args:
    model: The model the request named.

  Returns:
    The events, in order.
  """
  base = {"id": "resp_fake", "object": "response", "model": model}
  part = {"type": "output_text", "text": REPLY, "annotations": []}
  item = {
    "type": "message", "id": "msg_fake", "role": "assistant",
    "status": "completed", "content": [part],
  }  # fmt: skip
  where = {"output_index": 0, "item_id": "msg_fake", "content_index": 0}
  usage = {
    "input_tokens": 1, "input_tokens_details": {"cached_tokens": 0},
    "output_tokens": 1, "output_tokens_details": {"reasoning_tokens": 0},
    "total_tokens": 2,
  }  # fmt: skip
  return [
    {"type": "response.created",
     "response": {**base, "status": "in_progress", "output": []}},
    {"type": "response.output_item.added", "output_index": 0,
     "item": {**item, "status": "in_progress", "content": []}},
    {"type": "response.content_part.added", **where,
     "part": {**part, "text": ""}},
    {"type": "response.output_text.delta", **where, "delta": REPLY},
    {"type": "response.output_text.done", **where, "text": REPLY},
    {"type": "response.content_part.done", **where, "part": part},
    {"type": "response.output_item.done", "output_index": 0, "item": item},
    {"type": "response.completed",
     "response": {**base, "status": "completed", "output": [item],
                  "usage": usage}},
  ]  # fmt: skip


class Upstream(http.server.BaseHTTPRequestHandler):
  """The fake upstream: the Codex responses endpoint and Claude's messages."""

  protocol_version = "HTTP/1.1"

  def log_message(self, *args) -> None:
    """Keep the container log for the gateways."""

  def read_body(self) -> bytes:
    """Read the request body, plain or chunked.

    Returns:
      The body as sent, still compressed if it was.
    """
    if self.headers.get("transfer-encoding", "").lower() == "chunked":
      body = b""
      while True:
        size = int(self.rfile.readline().split(b";")[0], 16)
        if size == 0:
          self.rfile.readline()
          return body
        body += self.rfile.read(size)
        self.rfile.readline()
    return self.rfile.read(int(self.headers.get("content-length") or 0))

  def do_POST(self) -> None:
    """Answer one inference and record it."""
    body = self.read_body()
    encoded = self.headers.get("content-encoding", "")
    entry = {
      "kind": "request",
      "method": "POST",
      "port": self.server.server_address[1],
      # The exit relays the gateway's absolute-URI request line as it is.
      "path": urllib.parse.urlsplit(self.path).path,
      "authorization_sha256": digest(self.headers.get("authorization")),
      "x_api_key_sha256": digest(self.headers.get("x-api-key")),
      "chatgpt_account_id": self.headers.get("chatgpt-account-id"),
      "originator": self.headers.get("originator"),
      "content_encoding": encoded,
      "installation_ids": installation_ids(
        self.headers, b"" if encoded else body
      ),
    }
    try:
      model = json.loads(body).get("model", "fake-model") if not encoded else ""
    except (ValueError, AttributeError):
      model = "fake-model"
    loopback = self.client_address[0] == "127.0.0.1"
    if entry["path"] == "/control/revoke" and loopback:
      with lock:
        tokens["accepted"] = ""
      self.answer(204, b"")
      return
    if entry["path"] == "/oauth/token":
      status, answer, entry = refresh(body)
      record(entry)
      self.answer(status, json.dumps(answer).encode())
      return
    accepted = tokens["accepted"]
    if (
      entry["path"].endswith("/responses")
      and accepted is not None
      and self.headers.get("authorization") != f"Bearer {accepted}"
    ):
      entry["status"] = 401
      record(entry)
      self.answer(401, b'{"error": {"code": "token_expired"}}')
      return
    if entry["path"].endswith("/responses"):
      entry["status"] = 200
      record(entry)
      self.send_response(200)
      for name, value in QUOTA_HEADERS.items():
        self.send_header(name, value)
      self.send_header("Transfer-Encoding", "chunked")
      self.end_headers()
      for event in sse_events(model or "fake-model"):
        chunk = f"event: {event['type']}\ndata: {json.dumps(event)}\n\n"
        data = chunk.encode()
        self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
      self.wfile.write(b"0\r\n\r\n")
      return
    if entry["path"] == "/v1/messages":
      entry["status"] = 200
      record(entry)
      answer = json.dumps({
        "id": "msg_fake", "type": "message", "role": "assistant",
        "content": [{"type": "text", "text": REPLY}],
      }).encode()  # fmt: skip
      self.send_response(200)
      self.send_header("Content-Type", "application/json")
      self.send_header("Content-Length", str(len(answer)))
      self.end_headers()
      self.wfile.write(answer)
      return
    entry["status"] = 404
    record(entry)
    self.send_response(404)
    self.send_header("Content-Length", "0")
    self.end_headers()

  def answer(self, status: int, body: bytes) -> None:
    """Send a whole answer.

    Args:
      status: The status.
      body: The body, JSON or empty.
    """
    self.send_response(status)
    if body:
      self.send_header("Content-Type", "application/json")
    self.send_header("Content-Length", str(len(body)))
    self.end_headers()
    self.wfile.write(body)

  def do_GET(self) -> None:
    """Record and refuse any other request."""
    record({
      "kind": "request", "method": "GET",
      "port": self.server.server_address[1],
      "upgrade": self.headers.get("upgrade"),
      "path": urllib.parse.urlsplit(self.path).path,
      "authorization_sha256": digest(self.headers.get("authorization")),
      "x_api_key_sha256": digest(self.headers.get("x-api-key")),
      "status": 404,
    })  # fmt: skip
    self.send_response(404)
    self.send_header("Content-Length", "0")
    self.end_headers()


def pump(source: socket.socket, sink: socket.socket) -> None:
  """Copy bytes from source to sink until source ends, then half-close sink.

  Args:
    source: The socket read.
    sink: The socket written.
  """
  try:
    while data := source.recv(65536):
      sink.sendall(data)
    sink.shutdown(socket.SHUT_WR)
  except OSError:
    pass


def exit_connection(client: socket.socket, port: int) -> None:
  """Serve one connection to the fake exit.

  Args:
    client: The accepted connection.
    port: The exit's port, which names the account.
  """
  head = b""
  while b"\r\n\r\n" not in head:
    chunk = client.recv(4096)
    if not chunk:
      client.close()
      return
    head += chunk
  line = head.split(b"\r\n", 1)[0].decode(errors="replace")
  if line.startswith("CONNECT "):
    record({"kind": "connect", "port": port, "target": line.split()[1]})
    client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
    pump(client, client)
    client.close()
    return
  upstream = socket.create_connection(UPSTREAM)
  upstream.sendall(head)
  back = threading.Thread(target=pump, args=(upstream, client), daemon=True)
  back.start()
  pump(client, upstream)
  back.join()
  upstream.close()
  client.close()


def serve_exit(address: tuple[str, int]) -> None:
  """Accept connections to one fake exit forever.

  Args:
    address: The address to listen on.
  """
  listener = socket.socket()
  listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
  listener.bind(address)
  listener.listen(64)
  while True:
    client, _ = listener.accept()
    threading.Thread(
      target=exit_connection, args=(client, address[1]), daemon=True
    ).start()


def main() -> None:
  """Start the upstream on loopback and the direct address, and the exits."""
  global record_file, email, account_id
  record_file, _, email, account_id = sys.argv[1:5]
  for address in [UPSTREAM, (sys.argv[2], DIRECT_PORT)]:
    server = http.server.ThreadingHTTPServer(address, Upstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
  for address in EXITS[1:]:
    threading.Thread(target=serve_exit, args=(address,), daemon=True).start()
  serve_exit(EXITS[0])


if __name__ == "__main__":
  main()
