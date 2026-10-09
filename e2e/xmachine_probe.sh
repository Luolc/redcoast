#!/bin/sh
# The fake claude of the cross-machine rehearsal, run inside an alpine client container
# (which has no python): one inference through the reverse proxy, one CONNECT through the
# forward proxy, then it holds its session until /client/stop appears. In probe mode it
# does the two requests with a credential read from stdin and prints the two statuses. In issue mode
# it asks the session interface for a session itself.
set -u
proxy_hostport=$(printf '%s' "$HTTPS_PROXY" | sed 's|.*@||')
probe() {
    token="$1"; base="$2"; hostport="$3"
    wget -q -S -O /dev/null --header "Authorization: Bearer $token" --header "Content-Type: application/json" \
        --post-data '{"model":"synthetic","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}' "$base/v1/messages" 2> /tmp/post.err || true
    post=$(grep -m1 -o 'HTTP/1.1 [0-9]*' /tmp/post.err | awk '{print $2}')
    auth=$(printf 'session:%s' "$token" | base64 | tr -d '\n')
    # The writing side stays open while the answer arrives: nc half-closes the connection when its stdin ends,
    # and the gateway treats a client that is gone as a canceled request, which no real client does.
    { printf 'CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\nProxy-Authorization: Basic %s\r\n\r\n' "$auth"; sleep 2; } \
        | nc -w 3 "${hostport%:*}" "${hostport##*:}" > /tmp/connect.out 2>/dev/null
    connect=$(head -1 /tmp/connect.out | awk '{print $2}')
    printf 'post=%s connect=%s\n' "${post:-none}" "${connect:-none}"
    [ "${CONNECT_DEBUG:-}" = 1 ] && tr -d '\r' < /tmp/connect.out | head -8 >&2
}
if [ "${1:-}" = issue ]; then
    # Ask the session interface for a session with the machine credential and the body in ISSUE_BODY;
    # only the status is printed, the session credential stays in this container.
    wget -q -S -O /tmp/issue.out --header "Authorization: Bearer $(cat /cred)" --header "Content-Type: application/json" \
        --post-data "$ISSUE_BODY" "$PROBE_SESSION/sessions" 2> /tmp/issue.err || true
    status=$(grep -m1 -o 'HTTP/1.1 [0-9]*' /tmp/issue.err | awk '{print $2}')
    printf '%s\n' "${status:-none}"
    exit 0
fi
if [ "${1:-}" = probe ]; then
    # The session credential arrives on stdin, so it is in no command line.
    IFS= read -r PROBE_TOKEN
    probe "$PROBE_TOKEN" "$PROBE_BASE" "$PROBE_PROXY"
    exit 0
fi
mkdir -p /client/out
echo $$ > /client/out/pid
probe "$CLAUDE_CODE_OAUTH_TOKEN" "$ANTHROPIC_BASE_URL" "$proxy_hostport" > /client/out/result
printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN" > /client/out/token
# With FAKE_CLAUDE_EXIT set, print the result and exit with that status instead of holding the session.
if [ -n "${FAKE_CLAUDE_EXIT:-}" ]; then
    cat /client/out/result
    exit "$FAKE_CLAUDE_EXIT"
fi
while [ ! -f /client/stop ]; do sleep 0.2; done
exit 0
