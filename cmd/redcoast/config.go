package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
)

// config holds the validated settings of the configuration file.
type config struct {
	listen             string   // reverse proxy (inference)
	forwardListen      string   // forward proxy (side traffic)
	socket             string   // unix socket of the session interface
	sessionListen      string   // TCP address of the session interface for other machines; empty for none
	adminSocket        string   // unix socket of the management interface
	healthListen       string   // health endpoint
	dashboardListen    string   // read-only dashboard
	dashboardDir       string   // the dashboard frontend's built files; empty for none
	hostNames          []string // the Host names /health and the dashboard answer
	egressInterval     time.Duration
	limits             claude.Limits
	accountConcurrency int
	upstream           *url.URL
	sessionDB          string
	captureDir         string
	inventory          string // directory of account YAML files
	tokenRef           string // env:// or file:// reference of the 1Password service account token; empty for none
	credentialsStdin   bool   // test entry: resolve references from a JSON object on stdin
	retention          session.Retention
	backup             *backupConfig // nil takes no backups
	tls                *tls.Config   // every TCP listener's; nil serves plain HTTP
	serverName         string        // the name clients reach the TLS listeners by
	warning            string        // logged at startup; empty for none
}

// tlsFile is listen.tls: one certificate for every TCP listener, all on one machine
// and reached by one name.
type tlsFile struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
}

// backupConfig says where the hourly backups go: a bucket of an S3-compatible service,
// addressed path-style under endpoint, and the references of the writer's key.
type backupConfig struct {
	Endpoint        string `yaml:"endpoint"`
	Bucket          string `yaml:"bucket"`
	Region          string `yaml:"region"`
	Prefix          string `yaml:"prefix"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
}

// fileConfig is the configuration file. Keys it does not name are refused; keys left
// out keep the defaults defaultFileConfig sets.
type fileConfig struct {
	Upstream string `yaml:"upstream"`
	Listen   struct {
		Reverse     string   `yaml:"reverse"`
		Forward     string   `yaml:"forward"`
		Health      string   `yaml:"health"`
		Dashboard   string   `yaml:"dashboard"`
		Session     string   `yaml:"session"`
		HostNames   []string `yaml:"host_names"`
		AllowPublic bool     `yaml:"allow_public"`
		TLS         *tlsFile `yaml:"tls"`
	} `yaml:"listen"`
	SessionSocket  string        `yaml:"session_socket"`
	AdminSocket    string        `yaml:"admin_socket"`
	SessionDB      string        `yaml:"session_db"`
	Inventory      string        `yaml:"inventory"`
	DashboardDir   string        `yaml:"dashboard_dir"`
	CaptureDir     string        `yaml:"capture_dir"`
	EgressInterval time.Duration `yaml:"egress_interval"`
	Limits         struct {
		IdleTimeout        time.Duration `yaml:"idle_timeout"`
		MaxRequestDuration time.Duration `yaml:"max_request_duration"`
		MaxTunnelDuration  time.Duration `yaml:"max_tunnel_duration"`
		MaxTunnels         int           `yaml:"max_tunnels"`
		AccountConcurrency int           `yaml:"account_concurrency"`
	} `yaml:"limits"`
	Retention struct {
		TrafficDays int `yaml:"traffic_days"`
		SessionDays int `yaml:"session_days"`
	} `yaml:"retention"`
	Credentials struct {
		OnePasswordToken string `yaml:"onepassword_token"`
		StdinTestEntry   bool   `yaml:"stdin_test_entry"`
	} `yaml:"credentials"`
	Backup *backupConfig `yaml:"backup"`
}

// maxConfigFile bounds the configuration file; a real one is about a kilobyte.
const maxConfigFile = 64 << 10

// maxRetentionDays bounds the retention days well inside what time.Duration holds; a
// day count that overflowed would turn negative and prune rows still in retention.
const maxRetentionDays = 36500

// The backup bucket follows S3's naming rules, dots included since the bucket is in the
// path, not the host; the region is one lowercase word or several joined by hyphens
// (us-east-1, auto); the prefix is slash-separated plain segments.
var (
	bucketName   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionName   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	backupPrefix = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// hostLabel matches a DNS name: dot-separated labels of lower-case letters, digits and
// inner hyphens.
var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// accountAlias matches the inventory's account IDs, which are not secret.
var accountAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// cgnat is 100.64.0.0/10, the shared address space tailnets use.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// defaultFileConfig returns the settings a file that leaves a key out runs with.
func defaultFileConfig() fileConfig {
	var f fileConfig
	f.Upstream = "https://api.anthropic.com"
	f.Listen.Reverse = "127.0.0.1:8789"
	f.Listen.Forward = "127.0.0.1:8791"
	f.Listen.Health = "127.0.0.1:7804"
	f.Listen.Dashboard = "127.0.0.1:7805"
	f.EgressInterval = 15 * time.Minute
	f.Limits.IdleTimeout = claude.DefaultLimits.Idle
	f.Limits.MaxRequestDuration = claude.DefaultLimits.MaxRequest
	f.Limits.MaxTunnelDuration = claude.DefaultLimits.MaxTunnel
	f.Limits.MaxTunnels = claude.DefaultLimits.Tunnels
	f.Limits.AccountConcurrency = claude.DefaultAccountConcurrency
	f.Retention.TrafficDays = int(session.DefaultTrafficRetention / (24 * time.Hour))
	f.Retention.SessionDays = int(session.DefaultSessionRetention / (24 * time.Hour))
	return f
}

// loadConfig takes the arguments after "serve", whose only flag is --config <path>,
// and reads and validates the file it names.
func loadConfig(arguments []string) (config, error) {
	flags := flag.NewFlagSet("redcoast claude serve", flag.ContinueOnError)
	path := flags.String("config", "", "YAML configuration file (required)")
	if err := flags.Parse(arguments); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 || *path == "" {
		return config{}, errors.New("usage: redcoast claude serve --config <path>")
	}
	file, err := os.Open(*path)
	if err != nil {
		return config{}, fmt.Errorf("config: %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigFile+1))
	if err != nil {
		return config{}, fmt.Errorf("config: %v", err)
	}
	if len(data) > maxConfigFile {
		return config{}, fmt.Errorf("config: larger than %d bytes", maxConfigFile)
	}
	return parseConfig(data)
}

// parseConfig decodes one YAML document strictly, an unknown key being an error, and
// rejects settings the gateway cannot run with. Each error names the key.
func parseConfig(data []byte) (config, error) {
	f := defaultFileConfig()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		return config{}, fmt.Errorf("config: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return config{}, errors.New("config: more than one YAML document")
	}
	var errs []error
	report := func(key, problem string) { errs = append(errs, fmt.Errorf("config: %s: %s", key, problem)) }
	target, err := parseOrigin(f.Upstream)
	if err != nil {
		report("upstream", err.Error())
	}
	public := checkListeners(report, f.Listen.AllowPublic, []setting{
		{"listen.reverse", f.Listen.Reverse}, {"listen.forward", f.Listen.Forward}, {"listen.health", f.Listen.Health},
		{"listen.dashboard", f.Listen.Dashboard}, {"listen.session", f.Listen.Session},
	})
	for _, required := range []setting{{"session_db", f.SessionDB}, {"session_socket", f.SessionSocket}, {"admin_socket", f.AdminSocket}, {"inventory", f.Inventory}} {
		if required.value == "" {
			report(required.key, "missing")
		}
	}
	if f.AdminSocket != "" && f.AdminSocket == f.SessionSocket {
		report("admin_socket", "must differ from session_socket")
	}
	limits := claude.Limits{Idle: f.Limits.IdleTimeout, MaxRequest: f.Limits.MaxRequestDuration, MaxTunnel: f.Limits.MaxTunnelDuration, Tunnels: f.Limits.MaxTunnels}
	checkLimits(report, limits, f.Limits.AccountConcurrency, f.EgressInterval)
	for _, days := range []struct {
		key   string
		value int
	}{{"retention.traffic_days", f.Retention.TrafficDays}, {"retention.session_days", f.Retention.SessionDays}} {
		if days.value < 1 || days.value > maxRetentionDays {
			report(days.key, fmt.Sprintf("must be between 1 and %d", maxRetentionDays))
		}
	}
	token := f.Credentials.OnePasswordToken
	switch {
	case token != "" && f.Credentials.StdinTestEntry:
		report("credentials", "the stdin test entry does not take a 1Password token")
	case token != "" && (strings.HasPrefix(token, "op://") || credential.Check(token) != nil):
		report("credentials.onepassword_token", "must be an env:// or file:// reference")
	}
	if f.Backup != nil {
		checkBackup(report, *f.Backup)
	}
	var serverTLS *tls.Config
	var serverName, warning string
	if f.Listen.TLS != nil {
		serverTLS, serverName = loadTLS(report, *f.Listen.TLS), f.Listen.TLS.ServerName
	} else if len(public) > 0 {
		warning = "listen: " + strings.Join(public, ", ") + " on a public IP without listen.tls; session credentials cross the network in plain text"
	}
	hostNames := readOnlyHosts(report, f.Listen.Health, f.Listen.Dashboard, serverName, f.Listen.HostNames)
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	return config{tls: serverTLS, serverName: serverName, warning: warning, listen: f.Listen.Reverse, forwardListen: f.Listen.Forward, socket: f.SessionSocket, sessionListen: f.Listen.Session, adminSocket: f.AdminSocket,
		healthListen: f.Listen.Health, dashboardListen: f.Listen.Dashboard, dashboardDir: f.DashboardDir, hostNames: hostNames, egressInterval: f.EgressInterval, limits: limits,
		accountConcurrency: f.Limits.AccountConcurrency, upstream: target, sessionDB: f.SessionDB, captureDir: f.CaptureDir, inventory: f.Inventory,
		tokenRef: token, credentialsStdin: f.Credentials.StdinTestEntry,
		retention: session.Retention{Traffic: time.Duration(f.Retention.TrafficDays) * 24 * time.Hour, Sessions: time.Duration(f.Retention.SessionDays) * 24 * time.Hour},
		backup:    f.Backup}, nil
}

// readOnlyHosts returns the Host names /health and the dashboard answer: the IPs of
// their listen addresses, the TLS server name and listen.host_names, which must be DNS
// names without a port.
func readOnlyHosts(report func(key, problem string), health, dashboard, serverName string, extra []string) []string {
	var hosts []string
	for _, address := range []string{health, dashboard} {
		// Checked by checkListeners; a refused address adds nothing.
		host, _, _ := net.SplitHostPort(address)
		if ip, err := netip.ParseAddr(host); err == nil {
			hosts = append(hosts, ip.Unmap().String())
		}
	}
	if serverName != "" {
		hosts = append(hosts, strings.ToLower(serverName))
	}
	for _, name := range extra {
		if len(name) > 253 || !hostLabel.MatchString(name) {
			report("listen.host_names", fmt.Sprintf("%q is not a lower-case DNS name without a port", name))
			continue
		}
		hosts = append(hosts, name)
	}
	return hosts
}

// checkBackup checks that every backup key is set and has a plausible shape: an origin
// without a path, a bucket and region S3 would accept, and the key's two references.
func checkBackup(report func(key, problem string), b backupConfig) {
	if err := checkEndpoint(b.Endpoint); err != nil {
		report("backup.endpoint", err.Error())
	}
	if !bucketName.MatchString(b.Bucket) {
		report("backup.bucket", "not an S3 bucket name")
	}
	if !regionName.MatchString(b.Region) {
		report("backup.region", "not a region name")
	}
	if !backupPrefix.MatchString(b.Prefix) {
		report("backup.prefix", "must be slash-separated segments of letters, digits, dot, underscore and hyphen")
	}
	for _, ref := range []setting{{"backup.access_key_id", b.AccessKeyID}, {"backup.secret_access_key", b.SecretAccessKey}} {
		if err := credential.Check(ref.value); err != nil {
			report(ref.key, err.Error())
		}
	}
}

// checkEndpoint accepts an https origin, or an http one whose host is a loopback or
// private IP literal (a MinIO on the same network); the access key goes nowhere else.
func checkEndpoint(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("must be an origin such as https://s3.us-east-1.amazonaws.com")
	}
	switch endpoint.Scheme {
	case "https":
		return nil
	case "http":
		if ip, err := netip.ParseAddr(endpoint.Hostname()); err == nil && isPrivateListenIP(ip) {
			return nil
		}
		return errors.New("http only to a loopback or private IP; use https")
	}
	return errors.New("must be an https origin")
}

// bucketURL is the path-style URL of bucket at endpoint, which every S3-compatible
// service accepts; object keys follow it after a slash.
func bucketURL(endpoint, bucket string) string {
	return strings.TrimSuffix(endpoint, "/") + "/" + bucket
}

// checkLimits rejects limits whose idle limit is under a second or not shorter than
// both total limits, a tunnel or account concurrency limit under 1, and an egress
// interval under a minute.
func checkLimits(report func(key, problem string), limits claude.Limits, accountConcurrency int, egressInterval time.Duration) {
	if limits.Idle < time.Second || limits.MaxRequest <= limits.Idle || limits.MaxTunnel <= limits.Idle {
		report("limits.idle_timeout", "must be at least a second and shorter than max_request_duration and max_tunnel_duration")
	}
	if limits.Tunnels < 1 {
		report("limits.max_tunnels", "must be at least 1")
	}
	if accountConcurrency < 1 {
		report("limits.account_concurrency", "must be at least 1")
	}
	if egressInterval < time.Minute {
		report("egress_interval", "must be at least one minute")
	}
}

// parseOrigin accepts only the fixed API origin or a loopback origin (synthetic
// experiments run a fake upstream in the same network namespace). Real tokens go to
// no other host. Without a path, ProxyRequest.SetURL forwards each request path
// unchanged.
func parseOrigin(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" ||
		target.User != nil || target.Path != "" || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("not an origin")
	}
	if raw != "https://api.anthropic.com" && !isLoopbackAddress(target.Host) {
		return nil, errors.New("must be the API origin or a loopback origin")
	}
	return target, nil
}

// loadTLS reads listen.tls's certificate and key, which must pair, and checks that the
// certificate covers server_name. The files are read once; a handoff reads them again.
func loadTLS(report func(key, problem string), t tlsFile) *tls.Config {
	var pems [2][]byte
	for i, file := range []setting{{"listen.tls.cert_file", t.CertFile}, {"listen.tls.key_file", t.KeyFile}} {
		var err error
		if file.value == "" {
			report(file.key, "missing")
		} else if pems[i], err = os.ReadFile(file.value); err != nil {
			report(file.key, err.Error())
		}
	}
	if t.ServerName == "" {
		report("listen.tls.server_name", "missing")
	}
	if pems[0] == nil || pems[1] == nil {
		return nil
	}
	cert, err := tls.X509KeyPair(pems[0], pems[1])
	if err != nil {
		report("listen.tls", "cert_file and key_file: "+err.Error())
		return nil
	}
	if t.ServerName != "" && cert.Leaf.VerifyHostname(t.ServerName) != nil {
		report("listen.tls.server_name", "not covered by the certificate")
	}
	return claude.ServerTLS(cert)
}

// setting is one configuration key with its value.
type setting struct {
	key, value string
}

// checkListeners checks the TCP listen addresses: each is one IP literal with a port,
// never a host name or the wildcard that would listen on every interface, and no two
// are the same. The IP must be loopback or private (RFC 1918, 100.64.0.0/10, IPv6 ULA)
// unless allowPublic is set. listen.session may be empty (no network session interface).
// It returns the keys of the listeners on a public IP.
func checkListeners(report func(key, problem string), allowPublic bool, listeners []setting) (public []string) {
	seen := make(map[string]string, len(listeners))
	for _, l := range listeners {
		if l.value == "" && l.key == "listen.session" {
			continue
		}
		host, port, err := net.SplitHostPort(l.value)
		ip, ipErr := netip.ParseAddr(host)
		// ::ffff:0.0.0.0 is the IPv4 wildcard, and Go listens on [::] for it.
		ip = ip.Unmap()
		switch {
		case err != nil || port == "" || ipErr != nil || ip.Zone() != "":
			report(l.key, "must be one IP literal and a port, no host name")
		case ip.IsUnspecified():
			report(l.key, "must be a specific IP, not a wildcard")
		case !allowPublic && !isPrivateListenIP(ip):
			report(l.key, "a public IP needs listen.allow_public: true")
		case seen[net.JoinHostPort(ip.String(), port)] != "":
			report(l.key, "same address as "+seen[net.JoinHostPort(ip.String(), port)])
		default:
			seen[net.JoinHostPort(ip.String(), port)] = l.key
			if !isPrivateListenIP(ip) {
				public = append(public, l.key)
			}
		}
	}
	return public
}

// isPrivateListenIP reports whether ip is loopback, private (RFC 1918 or IPv6 ULA) or
// in 100.64.0.0/10.
func isPrivateListenIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)
}

// isLoopbackAddress reports whether address is a loopback IP literal with a port.
func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}
