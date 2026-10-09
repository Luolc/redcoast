import subprocess

import xmachine


def completed(stdout="", stderr="", returncode=0):
  return subprocess.CompletedProcess(["x"], returncode, stdout, stderr)


def test_redact_replaces_every_secret_and_skips_empty():
  text = "token=abc proxy=Secret-9 user=plain"
  assert (
    xmachine.redact(text, ["abc", "", "Secret-9"])
    == "token=<redacted> proxy=<redacted> user=plain"
  )


def test_redact_value_reaches_strings_inside_lists_and_dicts():
  value = {"exit": 3, "message": "refused: tok-1", "more": ["tok-1", 7, None]}
  assert xmachine.redact_value(value, ["tok-1"]) == {
    "exit": 3,
    "message": "refused: <redacted>",
    "more": ["<redacted>", 7, None],
  }


def test_leaking_argument_sees_secret_inside_an_argument():
  secrets = ["sk-ant-oat01-synthetic-x", ""]
  assert xmachine.leaking_argument(
    ["docker", "exec", "-e", "PROBE_TOKEN=sk-ant-oat01-synthetic-x", "a"],
    secrets,
  )
  assert not xmachine.leaking_argument(
    ["docker", "exec", "-i", "a", "/fake-claude", "probe"], secrets
  )


def test_credential_hash_is_sha256_hex_of_the_value():
  assert (
    xmachine.credential_hash("abc")
    == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
  )


def test_new_credential_is_base64url_without_padding_and_unique():
  first, second = xmachine.new_credential(), xmachine.new_credential()
  for value in (first, second):
    assert len(value) == 43
    assert all(c.isalnum() or c in "-_" for c in value)
  assert first != second


def test_launch_outcome_names_the_gateway_reason():
  refused = completed(
    stderr="gateway: machine client-a is at its limit of 1 sessions\n",
    returncode=3,
  )
  assert xmachine.launch_outcome(refused, "machine_limit") == {
    "exit": 3,
    "message": "machine_limit",
  }
  unknown = completed(
    stderr=("gateway: the machine credential matches no registered machine\n"),
    returncode=3,
  )
  assert xmachine.launch_outcome(unknown, "unknown_machine") == {
    "exit": 3,
    "message": "unknown_machine",
  }
  # Another refusal keeps its own text, so a mismatch is visible in the report.
  other = completed(
    stderr="redcoast-client: no session from the gateway\n", returncode=3
  )
  assert xmachine.launch_outcome(other, "machine_limit") == {
    "exit": 3,
    "message": "redcoast-client: no session from the gateway",
  }


def test_log_reading_requires_a_successful_read_with_the_startup_line():
  good = completed(stderr="2026/10/07 " + xmachine.STARTUP_LINE + "\n")
  assert xmachine.log_reading(good, ["tok", ""]) == {
    "log_read": True,
    "hits": 0,
  }
  leaked = completed(stderr=xmachine.STARTUP_LINE + "\nauthorization tok\n")
  assert xmachine.log_reading(leaked, ["tok"]) == {"log_read": True, "hits": 1}
  failed = completed(stderr="Error: No such container\n", returncode=1)
  assert xmachine.log_reading(failed, ["tok"]) == {"log_read": False, "hits": 0}
  missing_line = completed(stderr="something else\n")
  assert xmachine.log_reading(missing_line, ["tok"])["log_read"] is False
