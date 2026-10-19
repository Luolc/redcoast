// Package codex is the OpenAI side of the gateway: the Codex provider that the gateway
// core in package claude serves to Pi and the Codex CLI. An account is a ChatGPT login
// kept in a directory of its own on the gateway host; requests reach the ChatGPT
// backend's responses endpoint with the account's token, account ID and installation
// ID in place of the client's. One process serves one provider.
package codex

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Luolc/redcoast/claude"
)

// Upstream is the base URL of the ChatGPT backend's Codex API, the only upstream other
// than a loopback one.
const Upstream = "https://chatgpt.com/backend-api/codex"

// prefix is the start of the client's path that the upstream URL stands for: Pi adds
// /codex/responses to its base URL <gateway>/backend-api, and the Codex CLI adds
// /responses to <gateway>/backend-api/codex.
const prefix = "/backend-api/codex"

// requests are the only request lines forwarded: both clients send every inference
// request to the same endpoint over HTTP streaming.
var requests = map[string]bool{"POST " + prefix + "/responses": true}

// notSession answers a credential that is not a session credential of this process,
// such as a Claude session credential or a person's own ChatGPT login.
const notSession = "not a redcoast Codex session credential; start pi or codex through redcoast-client"

// New returns the Codex provider whose accounts' logins are in accountsDir, one
// directory per alias.
func New(accountsDir string) claude.Provider {
	return claude.Provider{
		Requests:   requests,
		Prefix:     prefix,
		Screen:     screen,
		NotSession: notSession,
		Account: func(alias string, document []byte) (claude.CredentialSource, error) {
			return account(accountsDir, alias, document)
		},
		Readings: quotaReadings,
		// The client cannot change how a request authenticates upstream: the gateway
		// sets both headers. So any 401 is the account's login refused.
		Refused: func(*http.Response) bool { return true },
		Guard:   uniqueHeaders,
	}
}

// errRepeatedHeader refuses a request that would leave with a header name twice.
var errRepeatedHeader = errors.New("a header name repeats")

// uniqueHeaders refuses a request in which a header name occurs more than once,
// comparing names without case and with '_' as '-': a repeated name could carry a
// second credential or account ID next to the gateway's, and the upstream or a proxy
// on the way might read either.
func uniqueHeaders(out *http.Request) error {
	seen := make(map[string]bool, len(out.Header))
	for name, values := range out.Header {
		key := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
		if seen[key] || len(values) > 1 {
			return errRepeatedHeader
		}
		seen[key] = true
	}
	return nil
}
