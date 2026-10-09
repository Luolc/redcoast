package claude

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// HostGuard answers next's requests only when they name the gateway: the Host header's
// name, whatever its port, must be one of hosts, and a request that carries an Origin
// must come from the page at that same Host. /health and the dashboard have no
// authentication, so a web page whose own name an attacker points at the gateway's
// address (DNS rebinding) would otherwise read them as same-origin; the browser still
// sends the attacker's name as Host. Anything else gets 403 without next's answer.
// hosts are lower-case names or IPs as netip.Addr.String writes them.
func HostGuard(hosts []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(hosts, requestHost(r.Host)) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown host"})
			return
		}
		if origin, ok := r.Header["Origin"]; ok && !sameOrigin(origin, r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestHost is the name in a Host header without its port, lower-cased, or an IP in
// netip's form; empty when there is none.
func requestHost(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.ToLower(host)
}

// sameOrigin reports whether the Origin header names the page r was sent to: https
// over TLS, http otherwise, at r's Host, which the browser writes from the same URL.
// "null" and every other value are foreign.
func sameOrigin(origin []string, r *http.Request) bool {
	if len(origin) != 1 {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	u, err := url.Parse(origin[0])
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}
