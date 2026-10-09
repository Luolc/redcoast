import hi_client


def test_said_accepts_the_expected_text_without_trailing_punctuation():
  assert hi_client.said({"is_error": False, "result": "Hi."}, ["Hi"])
  assert not hi_client.said({"is_error": True, "result": "Hi"}, ["Hi"])
  assert not hi_client.said({"is_error": False, "result": "Hello"}, ["Hi"])


def test_classify_cli_names_the_outcome():
  assert hi_client.classify_cli(None, b"", 1, ["Hi"]) == "invalid_json"
  assert (
    hi_client.classify_cli({"is_error": False, "result": "Hi"}, b"", 0, ["Hi"])
    == "expected"
  )
  refused = {"is_error": True, "result": "API Error: 401 authentication_error"}
  assert hi_client.classify_cli(refused, b"", 1, ["Hi"]) == "auth_text"
  assert (
    hi_client.classify_cli({"is_error": True}, b"request timed out", 1, ["Hi"])
    == "timeout_text"
  )
  assert (
    hi_client.classify_cli({"is_error": True, "result": "?"}, b"", 1, ["Hi"])
    == "unknown_error"
  )


def test_parse_json_returns_none_for_bad_or_huge_output():
  assert hi_client.parse_json(b'{"a": 1}') == {"a": 1}
  assert hi_client.parse_json(b"not json") is None
  assert hi_client.parse_json(b"[" + b"1," * 600000 + b"1]") is None


def test_exit_address_reads_json_or_plain_text():
  assert hi_client.exit_address('{"ip": "192.0.2.1"}') == "192.0.2.1"
  assert hi_client.exit_address("198.51.100.7\n") == "198.51.100.7"
  assert hi_client.exit_address("not an address") is None
