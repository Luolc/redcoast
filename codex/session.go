package codex

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// MinSessionKey is the shortest key that signs session credentials, the size of the
// HMAC-SHA256 output.
const MinSessionKey = 32

// fakeAccountID is the account ID every session credential carries. The client sends
// it back as chatgpt-account-id, which the gateway replaces with the account's own.
const fakeAccountID = "redcoast"

// jwtHeader is the encoded header of every session credential.
var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// maxSessionCredential bounds a presented credential before any decoding; one this
// gateway issues is under 300 bytes.
const maxSessionCredential = 1024

// SessionForm hands out session credentials as JWTs signed with HS256 under a key of
// the gateway's: the claims are the fake account ID and, as sid, the credential the
// session store issued, whose SHA-256 is all the store keeps. The JWT has no expiry;
// the store decides how long a session lives.
type SessionForm struct {
	key []byte
}

// NewSessionForm returns the form that signs with key, which must be at least
// MinSessionKey bytes.
func NewSessionForm(key []byte) (SessionForm, error) {
	if len(key) < MinSessionKey {
		return SessionForm{}, errors.New("session key shorter than 32 bytes")
	}
	return SessionForm{key: key}, nil
}

// claims are the session credential's claims. Pi reads the account ID from the
// https://api.openai.com/auth claim and sends no request with a bearer that is not a
// JWT carrying one.
type claims struct {
	Auth struct {
		AccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
	Sid string `json:"sid"`
}

// Wrap returns credential as a signed JWT.
func (f SessionForm) Wrap(credential string) string {
	var c claims
	c.Auth.AccountID, c.Sid = fakeAccountID, credential
	// Encoding strings cannot fail.
	payload, _ := json.Marshal(c)
	signed := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(f.sign(signed))
}

// Unwrap returns the credential inside a JWT that Wrap made with this key.
func (f SessionForm) Unwrap(presented string) (string, bool) {
	if len(presented) > maxSessionCredential {
		return "", false
	}
	header, rest, _ := strings.Cut(presented, ".")
	payload, signature, found := strings.Cut(rest, ".")
	if !found || header != jwtHeader {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(mac, f.sign(header+"."+payload)) {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	var c claims
	if err != nil || json.Unmarshal(decoded, &c) != nil || c.Sid == "" {
		return "", false
	}
	return c.Sid, true
}

// sign returns the HMAC-SHA256 of signed under the key.
func (f SessionForm) sign(signed string) []byte {
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}
