package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/Luolc/redcoast/claude"
)

// installationIDShape is the shape of the inventory's installation_id: a lower-case UUID
// v4, as Codex writes to $CODEX_HOME/installation_id.
var installationIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// authFile is the login in an account's directory, in $CODEX_HOME's format.
const authFile = "auth.json"

// maxAuthFile bounds auth.json; a real one is a few kilobytes.
const maxAuthFile = 64 << 10

// account reads installation_id and email from a served account's inventory file and
// returns the source of its credential: the login in accountsDir/alias/auth.json with
// that installation ID. The email is required because an imported login is checked
// against it.
func account(accountsDir, alias string, document []byte) (claude.CredentialSource, error) {
	var file struct {
		Email          string  `yaml:"email"`
		InstallationID *string `yaml:"installation_id"`
	}
	switch {
	case yaml.NewDecoder(bytes.NewReader(document)).Decode(&file) != nil:
		return nil, fmt.Errorf("%s: file: not a Codex account document", alias)
	case file.Email == "":
		return nil, fmt.Errorf("%s: email: missing", alias)
	case file.InstallationID == nil:
		return nil, fmt.Errorf("%s: installation_id: missing", alias)
	case !installationIDShape.MatchString(*file.InstallationID):
		return nil, fmt.Errorf("%s: installation_id: not a lower-case UUID v4", alias)
	}
	installationID := *file.InstallationID
	return func(context.Context, claude.Resolver) (claude.Credential, error) {
		token, accountID, err := readLogin(filepath.Join(accountsDir, alias))
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %v", alias, authFile, err)
		}
		return login{accessToken: token, accountID: accountID, installationID: installationID}, nil
	}, nil
}

// readLogin reads the access token and account ID of the login in dir. The directory
// and the file must be private to this user, and the file a regular one. Errors never
// carry a value from the file.
func readLogin(dir string) (accessToken, accountID string, err error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", "", errors.New("the account directory must exist with no access for group and others")
	}
	path := filepath.Join(dir, authFile)
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", "", errors.New("must be a regular file with no access for group and others")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", errors.New("cannot open")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxAuthFile+1))
	if err != nil || len(data) > maxAuthFile {
		return "", "", errors.New("cannot read, or too large")
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return "", "", errors.New("not a Codex login")
	}
	for _, value := range []struct{ field, value string }{{"tokens.access_token", auth.Tokens.AccessToken}, {"tokens.account_id", auth.Tokens.AccountID}} {
		if !headerValue(value.value) {
			return "", "", errors.New(value.field + ": empty or not a header value")
		}
	}
	return auth.Tokens.AccessToken, auth.Tokens.AccountID, nil
}

// headerValue reports whether value is non-empty printable ASCII without spaces, so
// that it cannot break or inject a header.
func headerValue(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < 0x21 || ch > 0x7e {
			return false
		}
	}
	return true
}

// login is a Codex account's credential: its ChatGPT access token and account ID, and
// the installation ID the account's requests carry.
type login struct {
	accessToken    string
	accountID      string
	installationID string
}

// Authorize removes every credential the client sent, sets the account's token and
// account ID, and puts the account's installation ID in place of the client's.
func (l login) Authorize(out *http.Request) {
	for _, name := range []string{"X-Api-Key", "Cookie", "Proxy-Authorization", "Chatgpt-Account-Id"} {
		out.Header.Del(name)
	}
	out.Header.Set("Authorization", "Bearer "+l.accessToken)
	out.Header.Set("Chatgpt-Account-Id", l.accountID)
	replaceInstallationID(out, l.installationID)
}

// Same reports whether other is the same login and installation ID.
func (l login) Same(other claude.Credential) bool {
	o, ok := other.(login)
	return ok && o == l
}
