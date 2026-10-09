package session

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client issues and revokes sessions over the interface: the unix socket (Socket) on
// the gateway's machine, or the TCP address (Address) with this machine's credential
// (Credential) from another machine. Exactly one of Socket and Address is set.
type Client struct {
	Socket     string         // path of the unix socket Server.ServeUnix listens on
	Address    string         // host:port Server.ServeTCP listens on, or https://host:port when it serves TLS
	Credential string         // the machine credential sent with every request over Address
	roots      *x509.CertPool // the CAs an https Address is checked against; nil for the system's
}

// ErrSocketUnavailable means the socket could not be reached.
var ErrSocketUnavailable = errors.New("session socket unavailable")

// maxResponseBody bounds what the client reads from a response.
const maxResponseBody = 8192

// httpClient returns a client for the interface and the origin of its URLs. Over an
// https Address that is the address itself, checked against the CAs; otherwise the
// client dials the socket or address for every request and the host in the origin is
// a placeholder.
func (c *Client) httpClient() (*http.Client, string) {
	if strings.HasPrefix(c.Address, "https://") {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12}}
		return &http.Client{Timeout: 10 * time.Second, Transport: transport}, c.Address
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if c.Address != "" {
					return (&net.Dialer{}).DialContext(ctx, "tcp", c.Address)
				}
				return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
			},
		},
	}, "http://session"
}

// Issue asks the gateway for a session for clientMachine, with launchMeta (nil for
// none) saved on it, and returns the grant. The credential is in Grant.Token and
// nowhere else; errors never contain it. A gateway that cannot serve a new session
// answers with a *Refusal, returned as the error.
func (c *Client) Issue(ctx context.Context, clientMachine string, launchMeta json.RawMessage) (Grant, error) {
	body, err := json.Marshal(issueRequest{ClientMachine: clientMachine, LaunchMeta: launchMeta})
	if err != nil {
		return Grant{}, fmt.Errorf("encode session request: %v", err)
	}
	client, origin := c.httpClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/sessions", bytes.NewReader(body))
	if err != nil {
		return Grant{}, fmt.Errorf("session request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Address != "" {
		req.Header.Set("Authorization", "Bearer "+c.Credential)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return Grant{}, fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return Grant{}, fmt.Errorf("read session response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		var refused Refusal
		if json.Unmarshal(data, &refused) == nil && refused.Code != "" && strings.HasPrefix(refused.Message, "redcoast:") {
			return Grant{}, &refused
		}
		return Grant{}, fmt.Errorf("session interface refused the request: status %d", resp.StatusCode)
	}
	var grant Grant
	if err := json.Unmarshal(data, &grant); err != nil || grant.Token == "" || grant.SessionID == "" || grant.ReverseProxy == "" || grant.ForwardProxy == "" {
		return Grant{}, errors.New("session interface returned an incomplete grant")
	}
	return grant, nil
}

// Revoke ends the session token identifies. ErrUnknownSession reports that the
// gateway no longer knows it.
func (c *Client) Revoke(ctx context.Context, token string) error {
	client, origin := c.httpClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, origin+"/sessions/current", nil)
	if err != nil {
		return fmt.Errorf("session request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSocketUnavailable, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return ErrUnknownSession
	default:
		return fmt.Errorf("session interface refused the revocation: status %d", resp.StatusCode)
	}
}
