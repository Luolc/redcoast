package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/credential"
	"github.com/Luolc/redcoast/session"
)

// AccountSource reads the accounts a load serves and the targets of direct-hosts.yaml.
// An error names aliases and fields, never a value. DirSource and StoreSource are the
// two sources.
type AccountSource func(ctx context.Context, provider Provider) ([]inventoryAccount, map[string]bool, error)

// DirSource reads the inventory directory dir: the source of the Codex process and of
// the first start of a Claude one.
func DirSource(dir string) AccountSource {
	return func(_ context.Context, provider Provider) ([]inventoryAccount, map[string]bool, error) {
		return readInventory(dir, provider)
	}
}

// StoreSource reads the accounts table of store, and direct-hosts.yaml from dir, which
// the table does not hold. A table that was never initialized is an error.
func StoreSource(store *session.Store, dir string) AccountSource {
	return func(ctx context.Context, provider Provider) ([]inventoryAccount, map[string]bool, error) {
		rows, initialized, err := store.Accounts(ctx)
		if err != nil {
			return nil, nil, err
		}
		if !initialized {
			return nil, nil, errors.New("inventory: the accounts table was never imported")
		}
		directHosts, err := readDirectHosts(dir)
		if err != nil {
			return nil, nil, err
		}
		accounts, err := servedRows(rows, provider)
		if err != nil {
			return nil, nil, err
		}
		return accounts, directHosts, nil
	}
}

// servedRows builds the accounts the gateway serves from rows, the others left out.
// Every refused row is reported, and so is a set with no account or two direct ones.
func servedRows(rows []session.AccountRow, provider Provider) ([]inventoryAccount, error) {
	var accounts []inventoryAccount
	var errs []error
	for _, row := range rows {
		if row.Status != "active" || row.Access != "gateway" {
			continue
		}
		file, data, err := rowDocument(row)
		if err == nil {
			var account inventoryAccount
			if account, err = servedAccount(row.Alias, file, data, provider); err == nil {
				accounts = append(accounts, account)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return accounts, checkServed(accounts)
}

// claudeFields are the keys of an account file that only the Claude provider reads.
type claudeFields struct {
	OAuthToken  *string `yaml:"oauth_token"`
	DeviceID    string  `yaml:"device_id"`
	AccountUUID string  `yaml:"account_uuid"`
}

// rowDocument returns the file fields and the provider document that describe row, as
// the directory's file of the account would.
func rowDocument(row session.AccountRow) (inventoryFile, []byte, error) {
	var file inventoryFile
	file.Email, file.ReceiverEmail, file.Access, file.Status = row.Email, row.ReceiverEmail, row.Access, row.Status
	file.Proxy.Direct, file.Proxy.Host, file.Proxy.Port = row.Direct, row.ProxyHost, row.ProxyPort
	file.Proxy.ExpectedIP, file.Proxy.UsernameRef, file.Proxy.PasswordRef = row.ExpectedIP, row.UsernameRef, row.PasswordRef
	fields := claudeFields{DeviceID: row.DeviceID, AccountUUID: row.AccountUUID}
	if row.TokenRef != "" {
		fields.OAuthToken = &row.TokenRef
	}
	data, err := yaml.Marshal(fields)
	if err != nil {
		return inventoryFile{}, nil, fieldError{row.Alias, "file", "cannot be described"}
	}
	return file, data, nil
}

// ImportInventory fills the accounts table of store from the inventory directory dir
// once: it reads every account file, refuses the inventory as readInventory does and
// also a reference field of a file the gateway does not serve that is not a credential
// reference, and writes all the rows and the marker in one transaction. It reports false, and reads
// nothing, when the table was imported before. Files of accounts the gateway does not
// serve are imported too, with their consumed fields as they are.
func ImportInventory(ctx context.Context, store *session.Store, dir string, provider Provider) (bool, error) {
	if _, initialized, err := store.Accounts(ctx); err != nil || initialized {
		return false, err
	}
	// The directory must load as a start from it would: all or nothing, validated
	// on the same bytes that are imported.
	rows, err := inventoryRows(dir)
	if err != nil {
		return false, err
	}
	if err := checkRows(rows, provider); err != nil {
		return false, err
	}
	if _, err := readDirectHosts(dir); err != nil {
		return false, err
	}
	imported, err := store.ImportAccounts(ctx, rows)
	if imported {
		log.Printf("accounts: imported %d from the inventory directory; the accounts table is the source from now on and the directory's account files are ignored", len(rows))
	}
	return imported, err
}

// checkRows refuses rows that could not be stored or served: a reference field of any
// row that is not a credential reference, wherever the account is served or not, and
// whatever refuses a served account. All problems are reported together, by alias and
// field and never a value.
func checkRows(rows []session.AccountRow, provider Provider) error {
	var errs []error
	for _, row := range rows {
		if row.Status != "active" || row.Access != "gateway" {
			// A served row's references are checked, with the rest, by servedRows.
			errs = append(errs, checkRowRefs(row))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	_, err := servedRows(rows, provider)
	return err
}

// checkRowRefs refuses a row whose token or proxy login reference is set but is not a
// credential reference (op://, env:// or file://), so that a pasted value never reaches
// the table, its backups, its export or the dashboard. The error names the alias and
// the field in the inventory's key names, never the value.
func checkRowRefs(row session.AccountRow) error {
	var errs []error
	for _, ref := range []struct{ field, value string }{{"oauth_token", row.TokenRef}, {"proxy.username_ref", row.UsernameRef}, {"proxy.password_ref", row.PasswordRef}} {
		if ref.value != "" && credential.Check(ref.value) != nil {
			errs = append(errs, fieldError{row.Alias, ref.field, "not a credential reference"})
		}
	}
	return errors.Join(errs...)
}

// inventoryRows reads every account file of dir as a row.
func inventoryRows(dir string) ([]session.AccountRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("inventory: %v", err)
	}
	var rows []session.AccountRow
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") || entry.Name() == directHostsFile {
			continue
		}
		alias := strings.TrimSuffix(entry.Name(), ".yaml")
		if !accountAlias.MatchString(alias) {
			errs = append(errs, fieldError{alias, "file name", "not an account alias"})
			continue
		}
		row, err := inventoryRow(alias, filepath.Join(dir, entry.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rows = append(rows, row)
	}
	return rows, errors.Join(errs...)
}

// inventoryRow reads one account file as a row. A key that no column of the accounts table
// holds is refused, by file and key, so that the import never drops one.
func inventoryRow(alias, path string) (session.AccountRow, error) {
	file, data, err := decodeInventoryFile(alias, path)
	if err != nil {
		return session.AccountRow{}, err
	}
	unknown, err := unknownKeys(data)
	if err != nil {
		unknown = []string{"file"}
	}
	var errs []error
	for _, key := range unknown {
		errs = append(errs, fieldError{alias, key, "unknown key; the accounts table has no column for it"})
	}
	if len(errs) > 0 {
		return session.AccountRow{}, errors.Join(errs...)
	}
	var fields claudeFields
	if yaml.NewDecoder(bytes.NewReader(data)).Decode(&fields) != nil {
		return session.AccountRow{}, fieldError{alias, "file", "not a Claude account document"}
	}
	row := session.AccountRow{
		Alias: alias, Email: file.Email, ReceiverEmail: file.ReceiverEmail, Access: file.Access, Status: file.Status,
		ProxyProvider: file.Proxy.Provider, ProxyType: file.Proxy.Type,
		Direct: file.Proxy.Direct, ProxyHost: file.Proxy.Host, ProxyPort: file.Proxy.Port, ExpectedIP: file.Proxy.ExpectedIP,
		UsernameRef: file.Proxy.UsernameRef, PasswordRef: file.Proxy.PasswordRef,
		DeviceID: fields.DeviceID, AccountUUID: fields.AccountUUID,
	}
	if row.Status == "" {
		row.Status = "testing"
	}
	if fields.OAuthToken != nil {
		row.TokenRef = *fields.OAuthToken
	}
	return row, nil
}

// exportFile is the layout of an exported account file: the keys of the inventory
// format that the gateway consumes. A key the account does not have is left out.
type exportFile struct {
	Email         string      `yaml:"email"`
	ReceiverEmail string      `yaml:"receiver_email,omitempty"`
	Access        string      `yaml:"access"`
	Status        string      `yaml:"status"`
	OAuthToken    string      `yaml:"oauth_token,omitempty"`
	DeviceID      string      `yaml:"device_id,omitempty"`
	AccountUUID   string      `yaml:"account_uuid,omitempty"`
	Proxy         *exportExit `yaml:"proxy,omitempty"`
}

// exportExit is the proxy block of an exported file.
type exportExit struct {
	Provider    string `yaml:"provider,omitempty"`
	Type        string `yaml:"type,omitempty"`
	Direct      bool   `yaml:"direct,omitempty"`
	Host        string `yaml:"host,omitempty"`
	Port        int    `yaml:"port,omitempty"`
	ExpectedIP  string `yaml:"expected_egress_ip,omitempty"`
	UsernameRef string `yaml:"username_ref,omitempty"`
	PasswordRef string `yaml:"password_ref,omitempty"`
}

// ExportInventory writes one <alias>.yaml per row into dir, creating dir, in the format
// of the inventory directory, so that reading dir gives the same served accounts. A
// file of the same name is replaced; other files stay. The files hold references,
// never a credential value.
func ExportInventory(dir string, rows []session.AccountRow) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("export: %v", err)
	}
	for _, row := range rows {
		if !accountAlias.MatchString(row.Alias) {
			return fieldError{row.Alias, "alias", "not an account alias"}
		}
		file := exportFile{Email: row.Email, ReceiverEmail: row.ReceiverEmail, Access: row.Access, Status: row.Status, OAuthToken: row.TokenRef, DeviceID: row.DeviceID, AccountUUID: row.AccountUUID}
		if exit := (exportExit{row.ProxyProvider, row.ProxyType, row.Direct, row.ProxyHost, row.ProxyPort, row.ExpectedIP, row.UsernameRef, row.PasswordRef}); exit != (exportExit{}) {
			file.Proxy = &exit
		}
		var data bytes.Buffer
		encoder := yaml.NewEncoder(&data)
		encoder.SetIndent(2)
		if err := errors.Join(encoder.Encode(file), encoder.Close()); err != nil {
			return fmt.Errorf("export: %s: %v", row.Alias, err)
		}
		if err := writeFileAtomic(filepath.Join(dir, row.Alias+".yaml"), data.Bytes()); err != nil {
			return fmt.Errorf("export: %s: %v", row.Alias, err)
		}
	}
	return nil
}

// writeFileAtomic writes data to path through a temporary file in the same directory.
func writeFileAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".export-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// yamlKeys returns the yaml keys of the struct type t.
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" {
			keys[name] = true
		}
	}
	return keys
}

// inventoryKeys and proxyKeys are the keys of an account file, and of its proxy block, that
// a column of the accounts table holds: the ones the file types above decode.
var (
	inventoryKeys = func() map[string]bool {
		keys := yamlKeys(reflect.TypeFor[inventoryFile]())
		maps.Copy(keys, yamlKeys(reflect.TypeFor[claudeFields]()))
		return keys
	}()
	proxyKeys = func() map[string]bool {
		proxy, _ := reflect.TypeFor[inventoryFile]().FieldByName("Proxy")
		return yamlKeys(proxy.Type)
	}()
)

// unknownKeys returns the keys of the account document data that no column of the accounts
// table holds, those under proxy as proxy.<key>. It names keys, never values.
func unknownKeys(data []byte) ([]string, error) {
	var document yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a mapping")
	}
	var unknown []string
	pairs := document.Content[0].Content
	for i := 0; i+1 < len(pairs); i += 2 {
		key, value := pairs[i].Value, pairs[i+1]
		switch {
		case key == "proxy" && value.Kind == yaml.MappingNode:
			for j := 0; j+1 < len(value.Content); j += 2 {
				if name := value.Content[j].Value; !proxyKeys[name] {
					unknown = append(unknown, "proxy."+name)
				}
			}
		case !inventoryKeys[key]:
			unknown = append(unknown, key)
		}
	}
	return unknown, nil
}
