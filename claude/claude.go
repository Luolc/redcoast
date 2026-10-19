package claude

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/credential"
)

// Claude is the provider of Anthropic's API: an account's credential is the long-lived
// OAuth token its inventory file references as oauth_token.
var Claude = Provider{
	Requests:   allowedRequests,
	NotSession: notSessionCredential,
	Account:    claudeAccount,
	Readings:   quotaReadings,
	Refused:    tokenRefused,
	Probe:      &Probe{Path: "/v1/messages", Body: refreshBody, Header: refreshHeaders},
	Capture:    true,
}

// claudeAccount reads oauth_token, the reference of the account's token.
func claudeAccount(alias string, document []byte) (CredentialSource, error) {
	var file struct {
		OAuthToken *string `yaml:"oauth_token"`
	}
	switch {
	case yaml.NewDecoder(bytes.NewReader(document)).Decode(&file) != nil:
		return nil, fieldError{alias, "oauth_token", "not a string"}
	case file.OAuthToken == nil || *file.OAuthToken == "":
		return nil, fieldError{alias, "oauth_token", "missing"}
	case credential.Check(*file.OAuthToken) != nil:
		return nil, fieldError{alias, "oauth_token", "not a credential reference"}
	}
	reference := *file.OAuthToken
	return func(ctx context.Context, resolve Resolver) (Credential, error) {
		token, err := resolve(ctx, reference)
		if err != nil {
			return nil, fieldError{alias, "oauth_token", "cannot resolve: " + err.Error()}
		}
		// The token ends up in a header, where a line break would inject another.
		if token == "" || strings.ContainsAny(token, "\r\n\x00") {
			return nil, fieldError{alias, "oauth_token", "empty value or line break"}
		}
		for _, ch := range token {
			if ch < 0x21 || ch > 0x7e {
				return nil, fieldError{alias, "oauth_token", "not a header value"}
			}
		}
		return bearer(token), nil
	}, nil
}

// bearer is a Claude account's OAuth token.
type bearer string

// Authorize removes every credential the client sent and sets the account's token, so
// nothing from the client authenticates upstream.
func (b bearer) Authorize(out *http.Request) {
	out.Header.Del("X-Api-Key")
	out.Header.Del("Cookie")
	out.Header.Del("Proxy-Authorization")
	out.Header.Set("Authorization", "Bearer "+string(b))
}

// Same reports whether other is the same token.
func (b bearer) Same(other Credential) bool {
	o, ok := other.(bearer)
	return ok && o == b
}
