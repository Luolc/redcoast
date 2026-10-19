package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Synthetic client identity values in the shapes the inventory accepts.
const (
	syntheticDeviceID    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	syntheticAccountUUID = "00000000-0000-4000-8000-00000000000a"
)

// The identity a client sends, distinct from every account's.
const (
	clientDeviceID    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	clientAccountUUID = "00000000-0000-4000-8000-0000000000cc"
	clientSessionID   = "00000000-0000-4000-8000-0000000000dd"
)

// clientUserID is metadata.user_id as Claude Code writes it, a JSON object encoded
// into a JSON string, with the client's IDs.
const clientUserID = `"{\"device_id\":\"` + clientDeviceID + `\",\"account_uuid\":\"` + clientAccountUUID + `\",\"session_id\":\"` + clientSessionID + `\"}"`

// TestIdentityRewrite checks that the upstream receives the bound account's device_id
// and account_uuid in metadata.user_id, "" for an account without a UUID, and every
// other byte as the client sent it: key order, whitespace, unescaped <, > and &, the
// session_id, and the client's device ID where it appears outside metadata. Bodies
// without a user_id of that shape reach the upstream unchanged.
func TestIdentityRewrite(t *testing.T) {
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.ContentLength != int64(len(body)) {
			body = []byte("unreadable body or wrong Content-Length")
		}
		received <- string(body)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	a := h.accounts.lookup("sample-a")
	proxy := h.gateway(upstream.URL)
	client := h.client()
	// A body in Claude Code's key order, with the client's device ID also quoted in a
	// message, and a user_id inside a message that is not metadata.
	claudeBody := `{"model":"claude-synthetic","messages":[{"role":"user","content":"<synthetic> & \"device_id\":\"` + clientDeviceID + `\""},{"role":"user","content":[],"metadata":{"user_id":` + clientUserID + `}}],"metadata":{"user_id":` + clientUserID + `},"max_tokens":1,"stream":true}`
	rewritten := func(body, deviceID, accountUUID string) string {
		user := strings.Replace(strings.Replace(clientUserID, clientDeviceID, deviceID, 1), clientAccountUUID, accountUUID, 1)
		at := strings.LastIndex(body, clientUserID)
		return body[:at] + user + body[at+len(clientUserID):]
	}
	spaced := "{\n  \"metadata\" : {\"other\": [1, 2.50], \"user_id\" : " + clientUserID + " } ,\n  \"model\": \"claude-synthetic\"\n}\n"
	for _, test := range []struct {
		name, deviceID, accountUUID, body, want string
	}{
		{"claude_body", syntheticDeviceID, syntheticAccountUUID, claudeBody, rewritten(claudeBody, syntheticDeviceID, syntheticAccountUUID)},
		{"account_without_uuid", syntheticDeviceID, "", claudeBody, rewritten(claudeBody, syntheticDeviceID, "")},
		{"whitespace", syntheticDeviceID, syntheticAccountUUID, spaced, rewritten(spaced, syntheticDeviceID, syntheticAccountUUID)},
		// The user_id of older Claude Code releases is a flat string.
		{"flat_user_id", syntheticDeviceID, syntheticAccountUUID, `{"metadata":{"user_id":"user_` + clientDeviceID + `_account_` + clientAccountUUID + `_session_` + clientSessionID + `"}}`, ""},
		{"not_json", syntheticDeviceID, syntheticAccountUUID, `{"metadata":{"user_id":` + clientUserID + `}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			a.credential = claudeCredential{token: tokenOf(a), deviceID: test.deviceID, accountUUID: test.accountUUID}
			want := test.want
			if want == "" {
				want = test.body
			}
			res, err := client.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if err := res.Body.Close(); err != nil || res.StatusCode != http.StatusOK {
				t.Fatalf("status %d, %v", res.StatusCode, err)
			}
			if got := <-received; got != want {
				t.Fatalf("upstream got\n%s\nwant\n%s", got, want)
			}
		})
	}
}
