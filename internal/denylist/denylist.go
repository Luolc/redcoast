// Package denylist names the hosts no gateway tunnel may reach, whichever route it
// would take. Requests to them carry someone else's credentials from a pool account's
// exit: the person's own ChatGPT login (chatgpt.com, auth.openai.com) and a client's
// error reports, which carry the gateway session's proxy address (the Sentry ingest).
package denylist

import "strings"

// hosts are matched exactly; their subdomains are not denied.
var hosts = map[string]bool{
	"chatgpt.com":                true,
	"auth.openai.com":            true,
	"o33249.ingest.us.sentry.io": true,
}

// Denied reports whether host, a host name without a port, is on the list. The
// comparison ignores case and a trailing dot.
func Denied(host string) bool {
	return hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
}
