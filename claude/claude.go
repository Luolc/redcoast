package claude

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/credential"
)

// Claude is the provider of Anthropic's API: an account's credential is the long-lived
// OAuth token its inventory file references as oauth_token, with the client identity
// (device_id, account_uuid) its requests carry.
var Claude = Provider{
	Requests:   allowedRequests,
	NotSession: notSessionCredential,
	Account:    claudeAccount,
	Readings:   quotaReadings,
	Usage:      claudeUsage,
	Refused:    tokenRefused,
	Probe:      &Probe{Path: "/v1/messages", Body: refreshBody, Header: refreshHeaders},
	Capture:    true,
}

// The shapes of the two client identity fields: Claude Code's device ID is 32 random
// bytes in lower-case hex, the account UUID is lower case.
var (
	deviceIDShape    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	accountUUIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// claudeAccount reads oauth_token, the reference of the account's token, and the two
// optional identity fields.
func claudeAccount(alias string, document []byte) (CredentialSource, error) {
	var file struct {
		OAuthToken  *string `yaml:"oauth_token"`
		DeviceID    string  `yaml:"device_id"`
		AccountUUID string  `yaml:"account_uuid"`
	}
	if yaml.NewDecoder(bytes.NewReader(document)).Decode(&file) != nil {
		return nil, fieldError{alias, "file", "not a Claude account document"}
	}
	var errs []error
	switch {
	case file.OAuthToken == nil || *file.OAuthToken == "":
		errs = append(errs, fieldError{alias, "oauth_token", "missing"})
	case credential.Check(*file.OAuthToken) != nil:
		errs = append(errs, fieldError{alias, "oauth_token", "not a credential reference"})
	}
	// Both identity fields are optional; an empty one is sent as "".
	if file.DeviceID != "" && !deviceIDShape.MatchString(file.DeviceID) {
		errs = append(errs, fieldError{alias, "device_id", "not 64 lower-case hex digits"})
	}
	if file.AccountUUID != "" && !accountUUIDShape.MatchString(file.AccountUUID) {
		errs = append(errs, fieldError{alias, "account_uuid", "not a lower-case UUID"})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	reference, deviceID, accountUUID := *file.OAuthToken, file.DeviceID, file.AccountUUID
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
		return claudeCredential{token: token, deviceID: deviceID, accountUUID: accountUUID}, nil
	}, nil
}

// claudeCredential is a Claude account's OAuth token and the client identity its
// requests carry (rewriteIdentity), each identity field "" when the inventory has none.
type claudeCredential struct {
	token       string
	deviceID    string
	accountUUID string
}

// Authorize removes every credential the client sent and sets the account's token, so
// nothing from the client authenticates upstream, and puts the account's identity in
// the body.
func (c claudeCredential) Authorize(out *http.Request) {
	out.Header.Del("X-Api-Key")
	out.Header.Del("Cookie")
	out.Header.Del("Proxy-Authorization")
	out.Header.Set("Authorization", "Bearer "+c.token)
	rewriteIdentity(out, c.deviceID, c.accountUUID)
}

// Same reports whether other is the same token and identity.
func (c claudeCredential) Same(other Credential) bool {
	o, ok := other.(claudeCredential)
	return ok && o == c
}
