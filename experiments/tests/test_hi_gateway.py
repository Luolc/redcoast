import json

import hi_gateway


def test_text_of_joins_text_blocks_and_tool_results():
  message = {
    "content": [
      {"type": "text", "text": "Read it"},
      {"type": "tool_result", "content": "Hello"},
      {"type": "image"},
    ]
  }
  assert hi_gateway.text_of(message) == 'Read it "Hello" '
  assert hi_gateway.text_of({"content": "plain"}) == "plain"


def test_plan_answers_from_what_the_client_sent():
  def body(*texts, **extra):
    messages = [{"role": "user", "content": text} for text in texts]
    return {"messages": messages, **extra}

  assert hi_gateway.plan(body("Hi")) == ["Hi"]
  assert hi_gateway.plan(body("Reply with exactly: Hello")) == ["Hello"]
  assert hi_gateway.plan(body("Count from 1 to 3, one per line.")) == [
    "1\n",
    "2\n",
    "3\n",
  ]
  assert (
    hi_gateway.plan(body("Read the file /x", tools=[{"name": "Read"}])) is None
  )
  tool_result = {
    "messages": [
      {"role": "user", "content": "Read the file"},
      {
        "role": "user",
        "content": [{"type": "tool_result", "content": "Hello\n"}],
      },
    ]
  }
  assert hi_gateway.plan(tool_result) == ["Hello"]
  history = {
    "messages": [
      {"role": "user", "content": "Reply with exactly: Hello"},
      {"role": "assistant", "content": [{"type": "text", "text": "Hello"}]},
      {
        "role": "user",
        "content": "What was your first reply in this conversation?",
      },
    ]
  }
  assert hi_gateway.plan(history) == ["Hello"]
  assert hi_gateway.plan(body("What was your first reply")) == ["NOHISTORY"]


def test_reply_events_carry_the_text_parts_in_order():
  events = hi_gateway.reply_events(["1\n", "2\n"], {"input_tokens": 1})
  names = [name for name, _ in events]
  assert names == [
    "message_start",
    "content_block_start",
    "content_block_delta",
    "content_block_delta",
    "content_block_stop",
    "message_delta",
    "message_stop",
  ]
  deltas = [
    event["delta"]["text"]
    for name, event in events
    if name == "content_block_delta"
  ]
  assert deltas == ["1\n", "2\n"]
  assert events[-2][1]["delta"]["stop_reason"] == "end_turn"


def test_reply_events_for_a_tool_use_ask_for_the_note():
  events = hi_gateway.reply_events(None, {"input_tokens": 1})
  block = events[1][1]["content_block"]
  assert block["type"] == "tool_use" and block["name"] == "Read"
  partial = json.loads(events[2][1]["delta"]["partial_json"])
  assert partial == {"file_path": "/client/work/note.txt"}
  assert events[-2][1]["delta"]["stop_reason"] == "tool_use"


def test_whole_message_joins_the_parts():
  message = hi_gateway.whole_message(
    ["Hi"], {"input_tokens": 1, "output_tokens": 0}
  )
  assert message["content"] == [{"type": "text", "text": "Hi"}]
  assert message["stop_reason"] == "end_turn"


def test_describe_head_keeps_the_shape_and_classifies_authorization():
  head = (
    b"GET /api/hello?probe=1&x=2 HTTP/1.1\r\nHost: api.anthropic.com\r\n"
    b"Authorization: Bearer sk-ant-gws-abc\r\n\r\n"
  )
  path, entry = hi_gateway.describe_head(head)
  assert path == "/api/hello"
  assert entry == {
    "method": "GET",
    "path": "/api/hello",
    "query_keys": ["probe", "x"],
    "header_names": ["authorization", "host"],
    "authorization": "session_bearer",
  }
  _, plain = hi_gateway.describe_head(b"POST /v1 HTTP/1.1\r\nHost: h\r\n\r\n")
  assert plain["authorization"] == "absent"


def test_exit_address_reads_json_or_plain_text():
  assert hi_gateway.exit_address('{"ip": "192.0.2.1"}') == "192.0.2.1"
  assert hi_gateway.exit_address("198.51.100.7\n") == "198.51.100.7"
  assert hi_gateway.exit_address("not an address") is None


KNOWN = (
  b"2000/01/01 00:00:34 accounts: example-a\n"
  b"2000/01/01 00:00:34 egress: check failed alias=example-a, not paused:"
  b' exit IP not read: Get "https://ip.oxylabs.io/location": proxyconnect'
  b" tcp: dial tcp 127.0.0.1:8793: connect: connection refused\n"
  b"2000/01/01 00:00:34 retention: deleted traffic=0 binding_history=0"
  b" sessions=0 vacuum=false\n"
)


def test_unknown_stderr_passes_the_known_startup_lines_only():
  aliases = ["example-a"]
  assert hi_gateway.unknown_stderr(KNOWN, aliases) is False
  persistence = (
    KNOWN + b"2000/01/01 00:00:40 capture persistence failed request_id=x\n"
  )
  assert hi_gateway.unknown_stderr(persistence, aliases) is False


def test_unknown_stderr_flags_any_other_line():
  extra = (
    KNOWN + b"2000/01/01 00:00:35 capture directory must exist with mode 0700\n"
  )
  assert hi_gateway.unknown_stderr(extra, ["example-a"]) is True
  loose = (
    KNOWN + b"2000/01/01 00:00:36 retention: deleted traffic=0 vacuum=maybe\n"
  )
  assert hi_gateway.unknown_stderr(loose, ["example-a"]) is True


def test_unknown_stderr_flags_known_shapes_with_a_foreign_alias():
  assert hi_gateway.unknown_stderr(KNOWN, ["example-aa"]) is True
  foreign = (
    b"2000/01/01 00:00:34 accounts: example-a\n"
    b"2000/01/01 00:00:34 egress: check failed alias=other, not paused:"
    b" exit IP not read: timeout\n"
  )
  assert hi_gateway.unknown_stderr(foreign, ["example-a"]) is True


def test_stderr_flag_accepts_known_lines_and_rejects_persistence_failures():
  assert hi_gateway.stderr_flag(KNOWN, ["example-a"]) is False
  failed = (
    KNOWN + b"2000/01/01 00:00:40 capture persistence failed request_id=x\n"
  )
  assert hi_gateway.stderr_flag(failed, ["example-a"]) is True
  assert (
    hi_gateway.stderr_flag(
      KNOWN + b"2000/01/01 00:00:41 other\n", ["example-a"]
    )
    is True
  )
