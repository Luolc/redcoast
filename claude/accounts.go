// Package claude is the Anthropic side of the gateway: the accounts with their
// per-account outbound side, the session-authenticated reverse proxy, the forward
// proxy, and the bounded, redacted capture records. One process serves one provider.
package claude

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Luolc/redcoast/credential"
)

// accountAlias matches the inventory's account IDs, which are not secret.
var accountAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Resolver resolves one credential reference to its value. The gateway's own resolver
// is credential.New over the official 1Password SDK; tests and synthetic experiments
// inject one that never reaches a vault.
type Resolver = credential.Resolver

// accountInput is one account with its credentials resolved: the real token and the
// account's exit proxy with its credentials.
type accountInput struct {
	Token         string
	Email         string // shown on the dashboard only
	ProxyURL      string
	ProxyUsername string
	ProxyPassword string
	ExpectedIP    netip.Addr // the exit IP the inventory expects
}

// account is one inventory account with its own outbound side: the real token, the
// exit proxy, a connection pool that only this account uses and a concurrency limit.
type account struct {
	alias         string
	email         string
	token         string
	proxyUsername string
	proxyPassword string
	exit          *url.URL        // http://host:port of the account's proxy, without credentials
	expectedIP    netip.Addr      // the inventory's expected_egress_ip
	transport     *http.Transport // every connection goes through exit
	slots         chan struct{}   // a buffered channel used as a semaphore
}

// AccountSet is the accounts the gateway can bind sessions to. A reload replaces the
// whole set and builds the new one with the same concurrency.
type AccountSet struct {
	byAlias     map[string]*account
	concurrency int             // inference requests one account serves at once
	directHosts map[string]bool // the inventory's direct-hosts.yaml, as targetKey keys
}

// LoadAccounts reads the inventory directory, resolves every served account's three
// credential references with resolve and builds the account set, each account serving
// at most concurrency inference requests at once, with the targets of direct-hosts.yaml.
// Nothing is served
// unless everything loaded: every failure is collected and reported by alias and field
// name, and the resolved values are never part of an error. A reference shared by
// several accounts is resolved once.
func LoadAccounts(ctx context.Context, dir string, resolve Resolver, concurrency int) (*AccountSet, error) {
	if concurrency < 1 {
		return nil, errors.New("account concurrency must be at least 1")
	}
	inventory, directHosts, err := readInventory(dir)
	if err != nil {
		return nil, err
	}
	set := &AccountSet{byAlias: make(map[string]*account, len(inventory)), concurrency: concurrency, directHosts: directHosts}
	resolved := make(map[string]string)
	var errs []error
	for _, entry := range inventory {
		var in accountInput
		complete := true
		for _, ref := range []struct {
			field, reference string
			value            *string
		}{{"oauth_token", entry.tokenRef, &in.Token}, {"proxy.username_ref", entry.usernameRef, &in.ProxyUsername}, {"proxy.password_ref", entry.passwordRef, &in.ProxyPassword}} {
			if ctx.Err() != nil {
				return nil, errors.Join(append(errs, ctx.Err())...)
			}
			value, known := resolved[ref.reference]
			if !known {
				value, err = resolve(ctx, ref.reference)
				if err != nil {
					errs = append(errs, fieldError{entry.alias, ref.field, "cannot resolve: " + err.Error()})
					complete = false
					continue
				}
				resolved[ref.reference] = value
			}
			*ref.value = value
		}
		if !complete {
			continue
		}
		in.Email, in.ProxyURL, in.ExpectedIP = entry.email, entry.exit, entry.expectedIP
		a, err := newAccount(entry.alias, in, concurrency)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", entry.alias, err))
			continue
		}
		set.byAlias[entry.alias] = a
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return set, nil
}

// newAccount validates one account and builds its outbound side. Errors name the field,
// never the value.
func newAccount(alias string, in accountInput, concurrency int) (*account, error) {
	if !accountAlias.MatchString(alias) {
		return nil, errors.New("alias: not an account alias")
	}
	// The values end up in HTTP headers and the proxy URL, where a line break would
	// inject a header.
	for _, value := range []struct{ field, value string }{{"oauth_token", in.Token}, {"proxy.username_ref", in.ProxyUsername}, {"proxy.password_ref", in.ProxyPassword}} {
		if value.value == "" || strings.ContainsAny(value.value, "\r\n\x00") {
			return nil, errors.New(value.field + ": empty value or line break")
		}
	}
	for _, ch := range in.Token {
		if ch < 0x21 || ch > 0x7e {
			return nil, errors.New("oauth_token: not a header value")
		}
	}
	exit, err := parseProxy(in.ProxyURL)
	if err != nil {
		return nil, errors.New("proxy: " + err.Error())
	}
	a := &account{alias: alias, email: in.Email, token: in.Token, proxyUsername: in.ProxyUsername, proxyPassword: in.ProxyPassword, exit: exit, expectedIP: in.ExpectedIP}
	a.transport = newTransport(concurrency)
	authenticated := *exit
	authenticated.User = url.UserPassword(in.ProxyUsername, in.ProxyPassword)
	a.transport.Proxy = http.ProxyURL(&authenticated)
	a.slots = make(chan struct{}, concurrency)
	return a, nil
}

// parseProxy accepts only http://host:port with an explicit numeric port. Proxy
// credentials never come in the URL, which would put them in error messages.
func parseProxy(raw string) (*url.URL, error) {
	invalid := errors.New("invalid proxy URL")
	proxy, err := url.Parse(raw)
	if err != nil || proxy.Scheme != "http" || proxy.User != nil || proxy.Opaque != "" ||
		proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" {
		return nil, invalid
	}
	host, port, err := net.SplitHostPort(proxy.Host)
	if err != nil || host == "" {
		return nil, invalid
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return nil, invalid
	}
	return proxy, nil
}

// lookup returns the account alias names, or nil.
func (s *AccountSet) lookup(alias string) *account {
	return s.byAlias[alias]
}

// Candidates returns every alias, sorted, for the session store's binding evaluation.
func (s *AccountSet) Candidates() []string {
	return slices.Sorted(maps.Keys(s.byAlias))
}

// CloseIdleConnections closes every account's idle connections.
func (s *AccountSet) CloseIdleConnections() {
	for _, a := range s.byAlias {
		a.transport.CloseIdleConnections()
	}
}

// exitAuthorization is the Proxy-Authorization value the account's exit expects.
func (a *account) exitAuthorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.proxyUsername+":"+a.proxyPassword))
}

// replaceClientAuthentication removes every credential the client sent and sets the
// account's real token, so nothing from the client authenticates upstream.
func (a *account) replaceClientAuthentication(header http.Header) {
	header.Del("X-Api-Key")
	header.Del("Cookie")
	header.Del("Proxy-Authorization")
	header.Set("Authorization", "Bearer "+a.token)
}

// knownCredential replaces the gateway's own credentials in a saved record. It differs
// from redacted so a reader can tell that a classifier let one of them through.
const knownCredential = "[KNOWN_CREDENTIAL]"

// knownValueReplacer returns a replacer for the encoded record that removes every
// account's token and proxy account in every form a record can carry them: as they
// are, URL-escaped, as the proxy's Basic authorization, and each of these escaped
// inside a JSON string. Every form of a token becomes the token's partial form, or
// knownCredential when the token is too short for one; the proxy accounts always
// become knownCredential. Longer forms come first so a value is never replaced only
// in part.
func (s *AccountSet) knownValueReplacer() *strings.Replacer {
	return knownValueReplacerFor(s.sorted())
}

// sorted returns the accounts by alias.
func (s *AccountSet) sorted() []*account {
	var accounts []*account
	for _, alias := range s.Candidates() {
		accounts = append(accounts, s.byAlias[alias])
	}
	return accounts
}

// knownValueReplacerFor builds the replacer of knownValueReplacer for accounts.
func knownValueReplacerFor(accounts []*account) *strings.Replacer {
	replacements := make(map[string]string)
	add := func(form, mark string) {
		// Encoding a string cannot fail.
		quoted, _ := json.Marshal(form)
		replacements[form] = mark
		replacements[string(quoted[1:len(quoted)-1])] = mark
	}
	addValue := func(value, mark string) {
		// url.User escapes the way a proxy URL's user name and password are both escaped.
		for _, form := range []string{value, url.QueryEscape(value), url.PathEscape(value), url.User(value).String()} {
			add(form, mark)
		}
	}
	for _, a := range accounts {
		tokenMark, partial := partialToken(a.token)
		if !partial {
			tokenMark = knownCredential
		}
		addValue(a.token, tokenMark)
	}
	// The proxy accounts come after the tokens so that a form both share is fully hidden.
	for _, a := range accounts {
		addValue(a.proxyUsername, knownCredential)
		addValue(a.proxyPassword, knownCredential)
		add(base64.StdEncoding.EncodeToString([]byte(a.proxyUsername+":"+a.proxyPassword)), knownCredential)
	}
	forms := slices.Collect(maps.Keys(replacements))
	slices.SortFunc(forms, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	var pairs []string
	for _, form := range forms {
		pairs = append(pairs, form, replacements[form])
	}
	return strings.NewReplacer(pairs...)
}
