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
	"sync/atomic"
	"time"

	"github.com/Luolc/redcoast/credential"
)

// accountAlias matches the inventory's account IDs, which are not secret.
var accountAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Resolver resolves one credential reference to its value. The gateway's own resolver
// is credential.New over the official 1Password SDK; tests and synthetic experiments
// inject one that never reaches a vault.
type Resolver = credential.Resolver

// accountInput is one account with its credentials resolved: the account's own
// credential and the account's exit proxy with its credentials, or, when Direct is
// set, no proxy at all.
type accountInput struct {
	Credential    Credential
	Email         string // shown on the dashboard only
	Direct        bool   // the gateway's own address is the exit
	ProxyURL      string
	ProxyUsername string
	ProxyPassword string
	ExpectedIP    netip.Addr // the exit IP the inventory expects
}

// account is one inventory account with its own outbound side: the real credential,
// the exit, a connection pool that only this account uses and a concurrency limit. The
// exit is a proxy, or, for a direct account, the gateway's own address.
type account struct {
	alias         string
	email         string
	credential    Credential
	proxyUsername string
	proxyPassword string
	exit          *url.URL        // http://host:port of the account's proxy, without credentials; nil when direct
	expectedIP    netip.Addr      // the inventory's expected_egress_ip
	transport     *http.Transport // every connection goes through exit, or, when direct, through dialDirect
	slots         chan struct{}   // a buffered channel used as a semaphore

	// direct is the network of every dial of a direct account, "tcp4" or "tcp6" by the
	// family of expectedIP, so that the gateway's other address is never its origin;
	// empty for an account with a proxy. dial makes those dials; tests replace it.
	direct string
	dial   func(ctx context.Context, network, address string) (net.Conn, error)

	// setAside is why the egress check set this configuration aside from selection
	// ("exit cannot reach <host>: <status>"), or nil. Only the check sets and clears it.
	setAside atomic.Pointer[string]
}

// AccountSet is the accounts the gateway can bind sessions to. A reload replaces the
// whole set and builds the new one with the same provider and concurrency.
type AccountSet struct {
	byAlias     map[string]*account
	provider    Provider        // reads the accounts' credentials from the inventory
	concurrency int             // inference requests one account serves at once
	directHosts map[string]bool // the inventory's direct-hosts.yaml, as targetKey keys
}

// LoadAccounts reads the inventory directory, builds every served account's credential
// with provider and resolves its proxy references with resolve, and builds the account
// set, each account serving at most concurrency inference requests at once, with the
// targets of direct-hosts.yaml. Nothing is served unless everything loaded: every
// failure is collected and reported by alias and field name, and the resolved values
// are never part of an error. A reference shared by several accounts is resolved once.
func LoadAccounts(ctx context.Context, dir string, provider Provider, resolve Resolver, concurrency int) (*AccountSet, error) {
	if concurrency < 1 {
		return nil, errors.New("account concurrency must be at least 1")
	}
	inventory, directHosts, err := readInventory(dir, provider)
	if err != nil {
		return nil, err
	}
	set := &AccountSet{byAlias: make(map[string]*account, len(inventory)), provider: provider, concurrency: concurrency, directHosts: directHosts}
	resolved := make(map[string]string)
	once := func(ctx context.Context, reference string) (string, error) {
		if value, known := resolved[reference]; known {
			return value, nil
		}
		value, err := resolve(ctx, reference)
		if err == nil {
			resolved[reference] = value
		}
		return value, err
	}
	var errs []error
	for _, entry := range inventory {
		if ctx.Err() != nil {
			return nil, errors.Join(append(errs, ctx.Err())...)
		}
		a, err := loadAccount(ctx, entry, once, concurrency)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		set.byAlias[entry.alias] = a
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return set, nil
}

// loadAccount builds one served account's credential, resolves its proxy references
// with resolve and builds the account. Every failure is in the returned error.
func loadAccount(ctx context.Context, entry inventoryAccount, resolve Resolver, concurrency int) (*account, error) {
	in := accountInput{Email: entry.email, Direct: entry.direct, ProxyURL: entry.exit, ExpectedIP: entry.expectedIP}
	var errs []error
	source, err := entry.credential(ctx, resolve)
	if err != nil {
		errs = append(errs, err)
	}
	in.Credential = source
	refs := []struct {
		field, reference string
		value            *string
	}{{"proxy.username_ref", entry.usernameRef, &in.ProxyUsername}, {"proxy.password_ref", entry.passwordRef, &in.ProxyPassword}}
	if entry.direct {
		refs = nil
	}
	for _, ref := range refs {
		if ctx.Err() != nil {
			return nil, errors.Join(append(errs, ctx.Err())...)
		}
		value, err := resolve(ctx, ref.reference)
		if err != nil {
			errs = append(errs, fieldError{entry.alias, ref.field, "cannot resolve: " + err.Error()})
			continue
		}
		*ref.value = value
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	a, err := newAccount(entry.alias, in, concurrency)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", entry.alias, err)
	}
	return a, nil
}

// newAccount validates one account and builds its outbound side. Errors name the field,
// never the value.
func newAccount(alias string, in accountInput, concurrency int) (*account, error) {
	if !accountAlias.MatchString(alias) {
		return nil, errors.New("alias: not an account alias")
	}
	if in.Direct {
		if in.ProxyURL != "" || in.ProxyUsername != "" || in.ProxyPassword != "" {
			return nil, errors.New("proxy: not allowed for a direct account")
		}
		a := &account{alias: alias, email: in.Email, credential: in.Credential, expectedIP: in.ExpectedIP, direct: "tcp6"}
		if in.ExpectedIP.Unmap().Is4() {
			a.direct = "tcp4"
		}
		a.dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		a.transport = newTransport(concurrency)
		a.transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
			return a.dialDirect(ctx, address)
		}
		a.slots = make(chan struct{}, concurrency)
		return a, nil
	}
	// The values end up in the proxy URL and its header, where a line break would
	// inject a header.
	for _, value := range []struct{ field, value string }{{"proxy.username_ref", in.ProxyUsername}, {"proxy.password_ref", in.ProxyPassword}} {
		if value.value == "" || strings.ContainsAny(value.value, "\r\n\x00") {
			return nil, errors.New(value.field + ": empty value or line break")
		}
	}
	exit, err := parseProxy(in.ProxyURL)
	if err != nil {
		return nil, errors.New("proxy: " + err.Error())
	}
	a := &account{alias: alias, email: in.Email, credential: in.Credential, proxyUsername: in.ProxyUsername, proxyPassword: in.ProxyPassword, exit: exit, expectedIP: in.ExpectedIP}
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

// SetAside returns the aliases the egress check has set aside, sorted, as the session
// store's binding evaluation takes them.
func (s *AccountSet) SetAside() []string {
	var out []string
	for _, a := range s.sorted() {
		if a.setAside.Load() != nil {
			out = append(out, a.alias)
		}
	}
	return out
}

// CloseIdleConnections closes every account's idle connections.
func (s *AccountSet) CloseIdleConnections() {
	for _, a := range s.byAlias {
		a.transport.CloseIdleConnections()
	}
}

// exitName is the account's proxy URL without credentials, or "direct".
func (a *account) exitName() string {
	if a.direct != "" {
		return "direct"
	}
	return a.exit.String()
}

// dialDirect dials address from the gateway in the direct account's address family.
// It serves only the hosts the process itself names (the upstream, the token URL, the
// echo service); a client's target goes through the forward proxy's publicDialer.
func (a *account) dialDirect(ctx context.Context, address string) (net.Conn, error) {
	return a.dial(ctx, a.direct, address)
}

// exitAuthorization is the Proxy-Authorization value the account's exit expects.
func (a *account) exitAuthorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.proxyUsername+":"+a.proxyPassword))
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
		// Only a provider with Capture keeps records, and its accounts hold Claude's tokens.
		c, ok := a.credential.(claudeCredential)
		if !ok {
			continue
		}
		tokenMark, partial := partialToken(c.token)
		if !partial {
			tokenMark = knownCredential
		}
		addValue(c.token, tokenMark)
	}
	// The proxy accounts come after the tokens so that a form both share is fully hidden.
	for _, a := range accounts {
		if a.direct != "" {
			continue
		}
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
