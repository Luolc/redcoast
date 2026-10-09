from pathlib import Path
import struct

import hi
import pytest


def test_phase_of_places_a_packet_relative_to_the_workload():
  assert hi.phase_of(5.0, None, None) == "before_workload"
  assert hi.phase_of(5.0, 10.0, 20.0) == "before_workload"
  assert hi.phase_of(15.0, 10.0, 20.0) == "workload"
  assert hi.phase_of(15.0, 10.0, None) == "workload"
  assert hi.phase_of(25.0, 10.0, 20.0) == "after_workload"


def test_dns_name_decodes_labels_and_follows_a_pointer():
  message = b"\x00" * 12 + b"\x03www\x07example\x03com\x00" + b"\xc0\x0c"
  assert hi.dns_name(message, 12) == ("www.example.com", 29)
  assert hi.dns_name(message, 29) == ("www.example.com", 31)


def test_client_hello_sni_reads_the_server_name():
  name = b"capture-control.invalid"
  sni = (
    b"\x00\x00"
    + struct.pack(">H", len(name) + 5)
    + struct.pack(">H", len(name) + 3)
    + b"\x00"
    + struct.pack(">H", len(name))
    + name
  )
  hello = (
    b"\x16\x03\x01\x00\x00\x01\x00\x00\x00\x03\x03"
    + b"\x00" * 32
    + b"\x00"  # session id
    + b"\x00\x02\x13\x01"  # one cipher suite
    + b"\x01\x00"  # one compression method
    + struct.pack(">H", len(sni))
    + sni
  )
  assert hi.client_hello_sni(hello) == "capture-control.invalid"
  assert hi.client_hello_sni(hello[:40]) is None
  assert hi.client_hello_sni(b"\x17" + hello[1:]) is None


def test_expected_outcome_of_picks_the_rule():
  assert (
    hi.expected_outcome_of("limits", False, False, False) == "limits_observed"
  )
  assert (
    hi.expected_outcome_of("egress", False, False, True)
    == "egress_matches_inventory"
  )
  assert (
    hi.expected_outcome_of("hi", True, False, False) == "fails_without_direct"
  )
  assert hi.expected_outcome_of("hi", False, False, False) == "case_passes"
  assert hi.expected_outcome_of("hi", False, True, True) == "case_passes"
  assert (
    hi.expected_outcome_of("hi", False, False, True) == "upstream_rejects_dummy"
  )


def test_pcap_records_splits_frames_and_times_them():
  header = b"\xd4\xc3\xb2\xa1" + b"\x00" * 16 + struct.pack("<I", 276)
  frame = struct.pack("<IIII", 10, 500000, 3, 3) + b"abc"
  link, count, frames = hi.pcap_records(header + frame + frame)
  assert (link, count) == (276, 2)
  assert frames[0] == (10.5, b"abc")


def test_reply_objects_unwraps_gzip_and_redacted_lines():
  classified = {
    "format": "gzip-analysis",
    "analysis": {"format": "classified-json", "data": {"a": 1}},
  }
  assert hi.reply_objects(classified) == [{"a": 1}]
  lines = {"format": "redacted-lines", "data": ['{"b": 2}', "not json"]}
  assert hi.reply_objects(lines) == [{"b": 2}]


def test_note_reply_records_usage_stop_reason_tool_and_error():
  row = {}
  hi.note_reply(
    row,
    {"message": {"usage": {"input_tokens": 5, "cache_read_input_tokens": 9}}},
  )
  hi.note_reply(
    row, {"delta": {"stop_reason": "tool_use"}, "usage": {"output_tokens": 1}}
  )
  hi.note_reply(row, {"content_block": {"type": "tool_use", "name": "Read"}})
  hi.note_reply(row, {"error": {"type": "rate_limit_error"}})
  hi.note_reply(row, "not a dict")
  assert row == {
    "usage": {
      "input_tokens": 5,
      "cache_read_input_tokens": 9,
      "output_tokens": 1,
    },
    "stop_reason": "tool_use",
    "tool_names": ["Read"],
    "error_type": "rate_limit_error",
  }


FIXTURES = Path(__file__).resolve().parent.parent / "fixtures" / "inventory"
GOOD = (FIXTURES / "example-a.yaml").read_text()


def test_load_account_accepts_the_fixture_with_and_without_vaults():
  account = hi.load_account(FIXTURES, "example-a")
  assert account.proxy.port == 8001
  assert account.oauth_token == "op://example-vault/example-a/token"
  checked = hi.load_account(
    FIXTURES, "example-a", "example-vault", "example-vault"
  )
  assert checked == account


@pytest.mark.parametrize(
  ("old", "new", "vaults", "field"),
  [
    ("op://example-vault/example-a/token", "op://other/x/token",
     ("example-vault", "example-vault"), "oauth_token"),
    ("op://example-vault/example-proxy/username", "op://other/x/username",
     ("example-vault", "example-vault"), "proxy.username_ref"),
    ("op://example-vault/example-a/token", "env://TOKEN", (None, None),
     "oauth_token"),
    ("port: 8001", 'port: "8001"', (None, None), "proxy.port"),
    ("port: 8001", "port: 0", (None, None), "proxy.port"),
    ("host: proxy.example.test", 'host: ""', (None, None), "proxy.host"),
    ("192.0.2.10", "not-an-ip", (None, None), "proxy.expected_egress_ip"),
  ],
)  # fmt: skip
def test_load_account_refuses_a_malformed_field_by_name(
  tmp_path, old, new, vaults, field
):
  assert old in GOOD
  (tmp_path / "bad.yaml").write_text(GOOD.replace(old, new))
  with pytest.raises(ValueError, match=field):
    hi.load_account(tmp_path, "bad", *vaults)
