package claude

import (
	"context"
	"net/http"

	"github.com/Luolc/redcoast/session"
)

// Provider is what one gateway process does that depends on the provider it serves.
// The rest of this package (sessions and bindings, accounts and their exits, reloads,
// the forward proxy, the management interface, health and the dashboard) is the same
// for every provider. Claude is Anthropic's.
type Provider struct {
	// Requests are the method and escaped path pairs the reverse proxy forwards;
	// anything else gets 403 without reaching the upstream.
	Requests map[string]bool
	// Prefix is the start of the client's path that the upstream URL stands for: a
	// forwarded request goes to the upstream URL's path followed by the rest of the
	// client's path. Empty forwards the path as it came.
	Prefix string
	// Screen answers a request before the allowlist and authentication when it returns
	// a status other than 0, with message as the body; nil screens nothing.
	Screen func(r *http.Request) (status int, message string)
	// NotSession is the answer to a credential that does not have the shape of a
	// session credential.
	NotSession string
	// Account reads the provider's own fields of a served account's inventory file and
	// returns what builds the account's credential. Its errors name the alias and the
	// field, never a value.
	Account func(alias string, document []byte) (CredentialSource, error)
	// Readings parses an upstream response's quota readings, recorded under source.
	Readings func(header http.Header, source string) []session.Reading
	// Refused reports whether an upstream 401 means the account's credential was
	// refused, which pauses the account.
	Refused func(res *http.Response) bool
	// Guard checks a request the reverse proxy forwards just before it leaves for the
	// upstream: an error answers 502 and nothing is sent. Nil checks nothing. A quota
	// refresh's Probe is the gateway's own request and does not pass it.
	Guard func(out *http.Request) error
	// Probe is the request a quota refresh sends; nil refuses every refresh.
	Probe *Probe
	// Capture allows capture records: their redaction knows the credentials of this
	// provider's accounts.
	Capture bool
}

// Probe is the smallest request that reads an account's quota: a POST to the upstream
// URL's path followed by Path, with Body and Header besides the account's credential.
type Probe struct {
	Path   string
	Body   string
	Header map[string]string
}

// Credential puts an account's credential on the requests forwarded for it.
type Credential interface {
	// Authorize removes every credential the client sent from out and puts the
	// account's in.
	Authorize(out *http.Request)
	// Same reports whether other is this credential, so that a reload keeps the
	// account's outbound object.
	Same(other Credential) bool
}

// CredentialSource builds an account's credential when the inventory is loaded,
// resolving its references with resolve. Its errors name the alias and the field,
// never a value.
type CredentialSource func(ctx context.Context, resolve Resolver) (Credential, error)
