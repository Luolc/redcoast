package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// directHostsFile is the inventory directory's list of CONNECT targets the forward
// proxy reaches from the gateway's own address instead of the account's exit. It sits
// beside the account files, so readInventory skips the name.
const directHostsFile = "direct-hosts.yaml"

// hostName matches a lower-case DNS name whose last label starts with a letter. Every
// top-level domain does; an IPv4 literal in any notation (192.0.2.1, 127.1, 0x7f.1) does
// not, so no address can be listed.
var hostName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z]([a-z0-9-]*[a-z0-9])?$`)

// readDirectHosts reads dir's direct-hosts.yaml and returns its targets as normalized
// host:port keys. A directory without the file lists nothing: every tunnel goes
// through the account's exit. Each entry is an exact host:port; there are no
// wildcards.
func readDirectHosts(dir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, directHostsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil || len(data) > maxInventoryFile {
		return nil, fmt.Errorf("%s: cannot read", directHostsFile)
	}
	var file struct {
		Hosts []string `yaml:"hosts"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s: not a list of hosts", directHostsFile)
	}
	direct := make(map[string]bool, len(file.Hosts))
	var errs []error
	for _, entry := range file.Hosts {
		key, ok := targetKey(entry)
		if !ok || !hostName.MatchString(strings.Split(key, ":")[0]) {
			errs = append(errs, fmt.Errorf("%s: %q: not a host name and port", directHostsFile, entry))
			continue
		}
		direct[key] = true
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return direct, nil
}

// targetKey normalizes a host:port the way a CONNECT target is compared with the list:
// the host lower-cased without a trailing dot, the port as a number.
func targetKey(target string) (string, bool) {
	host, port, err := net.SplitHostPort(target)
	number, numErr := strconv.Atoi(port)
	if err != nil || numErr != nil || number < 1 || number > 65535 {
		return "", false
	}
	return strings.ToLower(strings.TrimSuffix(host, ".")) + ":" + strconv.Itoa(number), true
}

// direct reports whether the forward proxy reaches target from the gateway's own
// address.
func (s *AccountSet) direct(target string) bool {
	key, ok := targetKey(target)
	return ok && s.directHosts[key]
}

// DirectHosts returns the listed targets, sorted.
func (s *AccountSet) DirectHosts() []string {
	var hosts []string
	for key := range s.directHosts {
		hosts = append(hosts, key)
	}
	slices.Sort(hosts)
	return hosts
}

// nonPublic are the ranges no tunnel reaches beyond those publicAddress rules out by
// kind: "this network" and the shared address space the tailnet uses.
var nonPublic = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10")}

// publicAddress reports whether a tunnel on either route may reach addr: a global
// unicast address outside the private (IPv6 ULA included), tailnet and "this network"
// ranges. Loopback, link-local (cloud metadata at 169.254.169.254 included), multicast
// and unspecified addresses are not global unicast.
func publicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// accountTarget reports whether the account route may hand target to the exit: a host
// name, which the exit resolves out of the gateway's sight, or an IP literal
// publicAddress accepts. A number some resolvers read as an address (127.1,
// 0x7f.0.0.1, 2130706433) is neither.
func accountTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return publicAddress(addr)
	}
	return hostName.MatchString(strings.ToLower(strings.TrimSuffix(host, ".")))
}

// dialDirect resolves a listed target's host and dials its public addresses in turn
// until one connects. It dials the address it checked, so a name that resolves to the
// gateway machine, a private network, the tailnet or link-local is never reached,
// whatever the list says.
func (f *forwardProxy) dialDirect(ctx context.Context, target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	addrs, err := f.resolve(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	errs := []error{fmt.Errorf("direct %s: no public address connected", target)}
	for _, addr := range addrs {
		if !publicAddress(addr) {
			log.Printf("forward: direct %s resolved to %s, not a public address; not dialed", target, addr)
			continue
		}
		conn, err := f.dial(ctx, "tcp", net.JoinHostPort(addr.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}
