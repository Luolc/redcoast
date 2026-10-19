import base64
import hashlib
import json

import pytest

import codex
import codex_fake


def test_new_records_keeps_what_came_after_the_snapshot_without_egress_checks():
  before = [{"kind": "request", "port": 18790}]
  egress = {"kind": "connect", "port": 18799, "target": codex.EGRESS_ECHO}
  connect = {"kind": "connect", "port": 18799, "target": "example.test:443"}
  request = {"kind": "request", "port": 18790}
  assert codex.new_records(before, before + [egress, connect, request]) == [
    connect,
    request,
  ]


def test_summarize_counts_requests_and_connects():
  records = [
    {"kind": "request"},
    {"kind": "connect", "target": "example.test:443"},
    {"kind": "request"},
  ]
  assert codex.summarize(records) == {"upstream_requests": 2, "exit_connects": 1}


def test_fake_jwt_carries_the_account_id_where_pi_reads_it():
  token = codex.fake_jwt(codex.login_claims("a@example.test", "acct-1"))
  payload = token.split(".")[1]
  claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
  assert claims["https://api.openai.com/auth"]["chatgpt_account_id"] == "acct-1"
  assert claims["email"] == "a@example.test"


def test_download_accepts_a_cached_copy_only_at_the_pinned_sha256(tmp_path):
  cached = tmp_path / "cached.tgz"
  cached.write_bytes(b"package")
  pinned = hashlib.sha256(b"package").hexdigest()
  codex.download("https://example.test/unused", pinned, tmp_path / "a.tgz", cached)
  assert (tmp_path / "a.tgz").read_bytes() == b"package"
  with pytest.raises(RuntimeError, match="SHA-256"):
    codex.download("https://example.test/unused", "0" * 64, tmp_path / "b.tgz", cached)


def test_installation_ids_reads_the_three_places():
  header_id, body_id, inner_id = (
    "11111111-1111-4111-8111-111111111111",
    "22222222-2222-4222-8222-222222222222",
    "33333333-3333-4333-8333-333333333333",
  )
  headers = {"x-codex-turn-metadata": json.dumps({"installation_id": header_id})}
  body = json.dumps({
    "client_metadata": {
      "x-codex-installation-id": body_id,
      "x-codex-turn-metadata": json.dumps({"installation_id": inner_id}),
    }
  }).encode()  # fmt: skip
  assert codex_fake.installation_ids(headers, body) == {
    "header": header_id,
    "body": body_id,
    "body_turn_metadata": inner_id,
  }


def test_installation_ids_of_a_body_without_metadata_are_none():
  assert codex_fake.installation_ids({}, b"\x28\xb5\x2f\xfd") == {
    "header": None,
    "body": None,
    "body_turn_metadata": None,
  }
