package claude

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/Luolc/redcoast/session"
)

// egressEchoURL is the IP echo service the egress check reads through the account's
// proxy. It is the only URL the check requests; a redirect is not followed.
const egressEchoURL = "https://ip.oxylabs.io/location"

const (
	egressTimeout = 15 * time.Second // one check, the proxy's CONNECT included
	maxEgressBody = 64 << 10         // the echo service's answer is a few hundred bytes
)

// EgressMismatchReason is the pause reason of an account whose exit IP differs from the
// inventory's expected_egress_ip.
const EgressMismatchReason = "egress_ip_mismatch"

// errEgressMismatch is wrapped by Check when the exit IP differs. Any other error from
// Check means the check could not read an IP.
var errEgressMismatch = errors.New("exit IP differs from expected_egress_ip")

// Egress checks each account's exit IP against the inventory: at startup and every
// interval after (Sweep), and on reload (Check is the reload's EgressCheck). A
// different IP pauses the account until a person resumes it. A check that could not
// read an IP (a timeout, a 5xx, an answer without an IP) pauses nothing: the exit may
// be right and only the check broken. It is logged and shows on the health endpoint
// until the account's next check reads an IP.
//
// Unless the IP differs, the check also asks the exit to CONNECT to the upstream's
// host. An answer other than 200 sets the account configuration aside from selection,
// and a later 200 ends that; neither touches a pause. A CONNECT without an answer
// changes nothing and shows on the health endpoint, as an unread IP does.
type Egress struct {
	accounts *Accounts
	store    *session.Store
	echo     string
	upstream string         // host:port of the upstream, the target of the reach check
	host     string         // the upstream's host name, as reasons name it
	rootCAs  *x509.CertPool // nil: the system roots; tests trust their fake echo service

	mu   sync.Mutex
	last map[*account]EgressResult // each account object's last check

	// Problems names further problems the health endpoint reports; nil names none.
	Problems func() []string
}

// EgressResult is the outcome of one account's last check since the gateway started:
// ok, mismatch, or unread when the check could not read an IP, and the exit's answer to
// the CONNECT to the upstream: ok, refused (with the status) or unanswered; empty when
// the IP differed and the CONNECT was not asked. The IP read is not kept.
type EgressResult struct {
	At             time.Time `json:"at"`
	Outcome        string    `json:"outcome"`
	Upstream       string    `json:"upstream,omitempty"`
	UpstreamStatus int       `json:"upstream_status,omitempty"`
}

// NewEgress returns the egress check over the accounts, pausing through store, that
// asks each exit to reach the host of upstream, the process's upstream URL.
func NewEgress(accounts *Accounts, store *session.Store, upstream *url.URL) *Egress {
	port := upstream.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[upstream.Scheme]
	}
	return &Egress{accounts: accounts, store: store, echo: egressEchoURL, upstream: net.JoinHostPort(upstream.Hostname(), port),
		host: upstream.Hostname(), last: make(map[*account]EgressResult)}
}

// Check reads a's exit IP through a's proxy, without the account's token, and compares
// it with the inventory's. The error wraps errEgressMismatch when the IPs differ. Unless
// they differ, it then asks the exit to reach the upstream and sets a aside or ends its
// set-aside by the answer; that outcome is no error, so a reload admits the account set
// aside and a later check can end it. The outcome is kept per account object, so a
// check of a configuration a reload replaced says nothing about the new one.
func (e *Egress) Check(ctx context.Context, a *account) error {
	got, err := e.read(ctx, a)
	result := EgressResult{At: time.Now().UTC(), Outcome: "ok"}
	switch {
	case err != nil:
		result.Outcome, err = "unread", fmt.Errorf("exit IP not read: %v", err)
	case got != a.expectedIP:
		result.Outcome, err = "mismatch", fmt.Errorf("%w: read %s, expected %s", errEgressMismatch, got, a.expectedIP)
	}
	if result.Outcome != "mismatch" {
		result.Upstream, result.UpstreamStatus = e.settle(a, e.reach(ctx, a))
	}
	e.mu.Lock()
	e.last[a] = result
	e.mu.Unlock()
	return err
}

// reach asks a's exit, with the proxy credentials and nothing else, to CONNECT to the
// upstream and closes the connection once the exit answered. It returns the exit's
// status, or 0 when the exit could not be dialed or sent no response head in time. A
// direct account has no exit to ask: the upstream is dialed and the connection closed
// before anything is sent, a connection counting as 200 and a failed dial as no answer.
func (e *Egress) reach(ctx context.Context, a *account) int {
	ctx, cancel := context.WithTimeout(ctx, egressTimeout)
	defer cancel()
	if a.direct != "" {
		conn, err := a.dialDirect(ctx, e.upstream)
		if err != nil {
			return 0
		}
		closeQuietly(conn)
		return http.StatusOK
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", a.exit.Host)
	if err != nil {
		return 0
	}
	defer closeQuietly(conn)
	stop := context.AfterFunc(ctx, func() { closeQuietly(conn) })
	defer stop()
	_, status := openTunnel(conn, e.upstream, a.exitAuthorization())
	return status
}

// settle applies the exit's answer to the reach check to a: 200 ends a's set-aside, any
// other status sets it aside, and no answer (0) changes nothing. It returns the
// check's upstream outcome and the status of a refusal.
func (e *Egress) settle(a *account, status int) (string, int) {
	switch status {
	case 0:
		log.Printf("egress: alias=%s CONNECT %s got no answer, set-aside unchanged", a.alias, e.upstream)
		return "unanswered", 0
	case http.StatusOK:
		if a.setAside.Swap(nil) != nil {
			log.Printf("egress: alias=%s exit reaches %s again, no longer set aside", a.alias, e.host)
		}
		return "ok", 0
	}
	reason := fmt.Sprintf("exit cannot reach %s: %d", e.host, status)
	a.setAside.Store(&reason)
	log.Printf("egress: alias=%s set aside: %s", a.alias, reason)
	return "refused", status
}

// read requests the echo service through a's proxy, or from the gateway for a direct
// account, on a connection of its own and returns the IP the service saw.
func (e *Egress) read(ctx context.Context, a *account) (netip.Addr, error) {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{RootCAs: e.rootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	if a.direct != "" {
		transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
			return a.dialDirect(ctx, address)
		}
	} else {
		proxy := *a.exit
		proxy.User = url.UserPassword(a.proxyUsername, a.proxyPassword)
		transport.Proxy = http.ProxyURL(&proxy)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       egressTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.echo, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("echo service answered %d", response.StatusCode)
	}
	var body struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxEgressBody)).Decode(&body); err != nil {
		return netip.Addr{}, errors.New("echo service answer is not a JSON object")
	}
	ip, err := netip.ParseAddr(body.IP)
	if err != nil {
		return netip.Addr{}, errors.New("echo service answer has no IP")
	}
	return ip.Unmap(), nil
}

// Sweep checks every account of the current set at once and pauses each one whose
// exit IP differs. Only a failure to write a pause is returned.
func (e *Egress) Sweep(ctx context.Context) error {
	accounts := e.accounts.set.Load().sorted()
	errs := make([]error, len(accounts))
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Go(func() { errs[i] = e.sweepOne(ctx, a) })
	}
	wg.Wait()
	// Outcomes of configurations a reload replaced are no longer read.
	e.mu.Lock()
	for a := range e.last {
		if e.accounts.set.Load().lookup(a.alias) != a {
			delete(e.last, a)
		}
	}
	e.mu.Unlock()
	return errors.Join(errs...)
}

// sweepOne checks a and pauses it when its exit IP differs. The check runs outside the
// region; the pause is written under its shared lock and only while a is still the
// account's configuration, so a reload cannot publish a new configuration between the
// comparison and the pause. An account a reload replaced is left alone: the reload
// checked its new configuration.
func (e *Egress) sweepOne(ctx context.Context, a *account) error {
	err := e.Check(ctx, a)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errEgressMismatch) {
		log.Printf("egress: check failed alias=%s, not paused: %v", a.alias, err)
		return nil
	}
	if !e.accounts.noExclusion {
		if err := e.accounts.region.rlock(ctx); err != nil {
			return fmt.Errorf("egress: %s: not paused, the gateway is shutting down or reloading: %v", a.alias, err)
		}
		defer e.accounts.region.runlock()
	}
	if e.accounts.set.Load().lookup(a.alias) != a {
		log.Printf("egress: alias=%s replaced by a reload during its check, not paused: %v", a.alias, err)
		return nil
	}
	if e.accounts.beforePause != nil {
		e.accounts.beforePause()
	}
	log.Printf("egress: alias=%s paused reason=%s: %v", a.alias, EgressMismatchReason, err)
	if err := e.store.Pause(ctx, a.alias, indefinitePause, EgressMismatchReason); err != nil {
		return fmt.Errorf("egress: %s: %v", a.alias, err)
	}
	return nil
}

// Run sweeps every interval until ctx ends. A pause the store cannot write is logged
// and tried again on the next sweep; the gateway goes on serving.
func (e *Egress) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.Sweep(ctx); err != nil {
				log.Print(err)
			}
		}
	}
}

// failedCheck reports whether the last check of account object a could not read an IP.
func (e *Egress) failedCheck(a *account) bool {
	return e.lastCheck(a).Outcome == "unread"
}

// unanswered reports whether the last check of account object a got no answer to its
// CONNECT to the upstream.
func (e *Egress) unanswered(a *account) bool {
	return e.lastCheck(a).Upstream == "unanswered"
}

// lastCheck returns the last check of account object a; zero when none ran.
func (e *Egress) lastCheck(a *account) EgressResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last[a]
}
