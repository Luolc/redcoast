package claude

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/credential"
)

// maxInventoryFile bounds one inventory file; the real ones are a few hundred bytes.
const maxInventoryFile = 64 << 10

// accountStatuses are the values an inventory file's status may take; only active
// accounts are served.
var accountStatuses = map[string]bool{"active": true, "paused": true, "suspended": true, "retired": true, "testing": true}

// inventoryFile is the part of one <inventory>/<alias>.yaml the gateway consumes.
// The other keys belong to other tools and are not validated here.
type inventoryFile struct {
	Email  string `yaml:"email"`
	Access string `yaml:"access"`
	Status string `yaml:"status"`
	Proxy  struct {
		Direct      bool   `yaml:"direct"`
		Host        string `yaml:"host"`
		Port        int    `yaml:"port"`
		ExpectedIP  string `yaml:"expected_egress_ip"`
		UsernameRef string `yaml:"username_ref"`
		PasswordRef string `yaml:"password_ref"`
	} `yaml:"proxy"`
}

// inventoryAccount is one account the gateway serves, as the inventory describes it: the
// alias, the exit without credentials, the references of the exit's credentials and what
// builds the account's own credential. A direct account has no exit and no references.
type inventoryAccount struct {
	alias       string
	email       string // shown on the dashboard only
	direct      bool   // the account's traffic leaves from the gateway's own address
	exit        string // http://host:port
	expectedIP  netip.Addr
	credential  CredentialSource
	usernameRef string
	passwordRef string
}

// fieldError reports a problem with one field of one account. It carries the alias and
// the field name, never the value.
type fieldError struct {
	alias, field, problem string
}

// Error formats the report as alias: field: problem.
func (e fieldError) Error() string {
	return e.alias + ": " + e.field + ": " + e.problem
}

// readInventory reads every *.yaml in dir and returns the accounts the gateway serves
// (status active and access gateway, sorted by alias), with the provider's own fields
// read by provider, and the targets of direct-hosts.yaml. Every file whose name is not an alias, every consumed field that
// is missing or malformed and every bad direct host is reported, all of them together,
// so one run shows everything to fix. No account at all is an error too: a gateway
// without accounts cannot serve.
func readInventory(dir string, provider Provider) ([]inventoryAccount, map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("inventory: %v", err)
	}
	directHosts, err := readDirectHosts(dir)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	var accounts []inventoryAccount
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == directHostsFile {
			continue
		}
		alias := strings.TrimSuffix(entry.Name(), ".yaml")
		if !accountAlias.MatchString(alias) {
			errs = append(errs, fieldError{alias, "file name", "not an account alias"})
			continue
		}
		account, served, err := readInventoryFile(alias, filepath.Join(dir, entry.Name()), provider)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if served {
			accounts = append(accounts, account)
		}
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	if len(accounts) == 0 {
		return nil, nil, errors.New("inventory: no active gateway account")
	}
	// Every direct account leaves from the gateway's one address, so two would share an exit.
	var direct []string
	for _, account := range accounts {
		if account.direct {
			direct = append(direct, account.alias)
		}
	}
	if len(direct) > 1 {
		return nil, nil, fmt.Errorf("inventory: proxy.direct: more than one direct account: %s", strings.Join(direct, ", "))
	}
	return accounts, directHosts, nil
}

// readInventoryFile reads one account file. served is false for an account the gateway
// does not serve (not active, or not on the gateway path); its credential fields are
// then not validated, because the file may describe an account that has none yet.
func readInventoryFile(alias, path string, provider Provider) (account inventoryAccount, served bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return inventoryAccount{}, false, fieldError{alias, "file", "cannot read"}
	}
	if len(data) > maxInventoryFile {
		return inventoryAccount{}, false, fieldError{alias, "file", "too large"}
	}
	var file inventoryFile
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&file); err != nil {
		return inventoryAccount{}, false, fieldError{alias, "file", "not an account document"}
	}
	var errs []error
	if file.Access != "direct" && file.Access != "gateway" {
		errs = append(errs, fieldError{alias, "access", "must be direct or gateway"})
	}
	// A missing status means testing, which the gateway does not serve.
	if file.Status != "" && !accountStatuses[file.Status] {
		errs = append(errs, fieldError{alias, "status", "unknown value"})
	}
	if len(errs) > 0 {
		return inventoryAccount{}, false, errors.Join(errs...)
	}
	if file.Status != "active" || file.Access != "gateway" {
		return inventoryAccount{}, false, nil
	}
	account, err = servedAccount(alias, file, data, provider)
	return account, err == nil, err
}

// servedAccount validates the fields a served account needs, the provider's own among
// them, and builds its description. data is the file the fields were decoded from. A
// proxy with direct: true must have only expected_egress_ip besides; any other proxy
// needs every field, so a file missing them is refused rather than read as direct.
func servedAccount(alias string, file inventoryFile, data []byte, provider Provider) (inventoryAccount, error) {
	var errs []error
	report := func(field, problem string) { errs = append(errs, fieldError{alias, field, problem}) }
	account := inventoryAccount{alias: alias, email: file.Email, direct: file.Proxy.Direct, usernameRef: file.Proxy.UsernameRef, passwordRef: file.Proxy.PasswordRef}
	source, err := provider.Account(alias, data)
	if err != nil {
		errs = append(errs, err)
	}
	account.credential = source
	expected, err := netip.ParseAddr(file.Proxy.ExpectedIP)
	if err != nil || expected.Zone() != "" {
		report("proxy.expected_egress_ip", "not an IP address")
	}
	if account.direct {
		for _, field := range []struct {
			name    string
			present bool
		}{{"proxy.host", file.Proxy.Host != ""}, {"proxy.port", file.Proxy.Port != 0}, {"proxy.username_ref", account.usernameRef != ""}, {"proxy.password_ref", account.passwordRef != ""}} {
			if field.present {
				report(field.name, "not allowed with proxy.direct")
			}
		}
		if len(errs) > 0 {
			return inventoryAccount{}, errors.Join(errs...)
		}
		account.expectedIP = expected.Unmap()
		return account, nil
	}
	for _, ref := range []struct{ field, value string }{{"proxy.username_ref", account.usernameRef}, {"proxy.password_ref", account.passwordRef}} {
		switch {
		case ref.value == "":
			report(ref.field, "missing")
		case credential.Check(ref.value) != nil:
			report(ref.field, "not a credential reference")
		}
	}
	if file.Proxy.Host == "" || strings.ContainsAny(file.Proxy.Host, "/@:?# \t") {
		report("proxy.host", "not a host name")
	}
	if file.Proxy.Port < 1 || file.Proxy.Port > 65535 {
		report("proxy.port", "not a port")
	}
	if len(errs) > 0 {
		return inventoryAccount{}, errors.Join(errs...)
	}
	account.expectedIP = expected.Unmap()
	account.exit = "http://" + net.JoinHostPort(file.Proxy.Host, strconv.Itoa(file.Proxy.Port))
	return account, nil
}
