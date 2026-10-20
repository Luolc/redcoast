package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// inventoryYAML renders one account file the way the inventory writes it, with the keys
// the gateway does not consume included so that ignoring them is exercised.
func inventoryYAML(alias, status, access, tokenRef, port string) string {
	token := "null"
	if tokenRef != "" {
		token = tokenRef
	}
	return "email: " + alias + "@example.test\nreceiver_email: receiver@example.test\naccess: " + access + "\nstatus: " + status +
		"\noauth_token: " + token + "\nproxy:\n  provider: oxylabs\n  type: dedicated-isp\n  host: proxy.example.test\n  port: " + port +
		"\n  expected_egress_ip: 192.0.2.1\n  username_ref: op://example-proxies/proxy-account/username\n  password_ref: op://example-proxies/proxy-account/password\n"
}

// writeInventory writes files, name to content, into a fresh directory and returns it.
func writeInventory(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestLoadAccounts checks which inventories load and that each loaded account's
// transport uses that account's exit with its own resolved credentials. The refused
// inventories name the alias and the field in the error and never a value; the
// resolver's values are distinctive strings so that leaking one would show.
func TestLoadAccounts(t *testing.T) {
	values := map[string]string{
		"op://example-vault/token-sample-a/credential":       "sk-ant-oat01-synthetic-value-sample-a",
		"op://example-vault/token-sample-b/credential":       "sk-ant-oat01-synthetic-value-sample-b",
		"op://example-vault/token-sample-c/credential":       "sk-ant-oat01-synthetic-value-sample-c",
		"op://example-vault/token-newline/credential":        "synthetic-value\r\nwith-break",
		"op://example-proxies/proxy-account/username":        "synthetic-value-proxy-user",
		"op://example-proxies/proxy-account/password":        "synthetic-value-proxy-password",
		"op://example-vault/token-shared/section/credential": "sk-ant-oat01-synthetic-value-section",
	}
	// sample-a has both client identity fields, sample-b neither.
	a := inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", "8001") + "device_id: " + syntheticDeviceID + "\naccount_uuid: " + syntheticAccountUUID + "\n"
	b := inventoryYAML("sample-b", "active", "gateway", "op://example-vault/token-sample-b/credential", "8002")
	// Malformed identity values; an error must not carry them.
	badDeviceID, badAccountUUID := strings.ToUpper(syntheticDeviceID), "synthetic-not-a-uuid"
	for _, test := range []struct {
		name  string
		files map[string]string
		// aliases loaded, in order; nil when the inventory is refused
		aliases []string
		// substrings the error must carry when refused
		mentions []string
	}{
		{"two_accounts", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b}, []string{"sample-a", "sample-b"}, nil},
		{"section_reference", map[string]string{"sample-a.yaml": strings.Replace(a, "op://example-vault/token-sample-a/credential", "op://example-vault/token-shared/section/credential", 1), "sample-b.yaml": b}, []string{"sample-a", "sample-b"}, nil},
		// Accounts the gateway does not serve are skipped, their references unresolved.
		{"paused_skipped", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b, "sample-c.yaml": inventoryYAML("sample-c", "paused", "gateway", "op://example-vault/token-missing/credential", "8003")}, []string{"sample-a", "sample-b"}, nil},
		{"direct_skipped", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b, "sample-c.yaml": inventoryYAML("sample-c", "active", "direct", "", "8003")}, []string{"sample-a", "sample-b"}, nil},
		{"status_defaults_to_testing", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b, "sample-c.yaml": strings.Replace(inventoryYAML("sample-c", "active", "gateway", "", "8003"), "status: active\n", "", 1)}, []string{"sample-a", "sample-b"}, nil},
		{"other_files_ignored", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b, "README.md": "notes"}, []string{"sample-a", "sample-b"}, nil},
		{"no_account", map[string]string{"sample-c.yaml": inventoryYAML("sample-c", "retired", "gateway", "", "8003")}, nil, []string{"no active gateway account"}},
		{"file_name_not_alias", map[string]string{"sample-a.yaml": a, "Sample B.yaml": b}, nil, []string{"Sample B: file name"}},
		{"not_a_document", map[string]string{"sample-a.yaml": a, "sample-b.yaml": "- just\n- a list\n"}, nil, []string{"sample-b: file"}},
		{"unknown_status", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "status: active", "status: sleeping", 1)}, nil, []string{"sample-b: status"}},
		{"unknown_access", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "access: gateway", "access: tunnel", 1)}, nil, []string{"sample-b: access"}},
		{"active_without_token", map[string]string{"sample-a.yaml": a, "sample-b.yaml": inventoryYAML("sample-b", "active", "gateway", "", "8002")}, nil, []string{"sample-b: oauth_token: missing"}},
		{"not_a_reference", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "op://example-vault/token-sample-b/credential", "token-sample-b", 1)}, nil, []string{"sample-b: oauth_token: not a credential reference"}},
		{"port_as_string", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "port: 8002", "port: \"8002\"", 1)}, nil, []string{"sample-b: file"}},
		{"port_out_of_range", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "port: 8002", "port: 70000", 1)}, nil, []string{"sample-b: proxy.port"}},
		{"expected_ip_missing", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "  expected_egress_ip: 192.0.2.1\n", "", 1)}, nil, []string{"sample-b: proxy.expected_egress_ip"}},
		{"expected_ip_not_an_ip", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "192.0.2.1", "proxy.example.test", 1)}, nil, []string{"sample-b: proxy.expected_egress_ip"}},
		{"host_with_credentials", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "host: proxy.example.test", "host: u:p@proxy.example.test", 1)}, nil, []string{"sample-b: proxy.host"}},
		// Every failure is reported, not only the first one.
		{"unresolvable_references", map[string]string{"sample-a.yaml": strings.Replace(a, "op://example-vault/token-sample-a/credential", "op://example-vault/token-missing/credential", 1), "sample-b.yaml": strings.Replace(b, "op://example-proxies/proxy-account/password", "op://example-proxies/proxy-account/missing", 1)}, nil, []string{"sample-a: oauth_token: cannot resolve", "sample-b: proxy.password_ref: cannot resolve"}},
		{"empty_identity", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b + "device_id: \"\"\naccount_uuid: \"\"\n"}, []string{"sample-a", "sample-b"}, nil},
		{"identity_malformed", map[string]string{"sample-a.yaml": a, "sample-b.yaml": b + "device_id: " + badDeviceID + "\naccount_uuid: " + badAccountUUID + "\n"}, nil, []string{"sample-b: device_id", "sample-b: account_uuid"}},
		{"value_with_line_break", map[string]string{"sample-a.yaml": a, "sample-b.yaml": strings.Replace(b, "op://example-vault/token-sample-b/credential", "op://example-vault/token-newline/credential", 1)}, nil, []string{"sample-b: oauth_token"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := map[string]int{}
			resolve := func(_ context.Context, reference string) (string, error) {
				resolved[reference]++
				value, known := values[reference]
				if !known {
					return "", errors.New("synthetic resolver: unknown reference")
				}
				return value, nil
			}
			accounts, err := LoadAccounts(t.Context(), writeInventory(t, test.files), Claude, resolve, DefaultAccountConcurrency)
			if test.aliases == nil {
				if err == nil {
					t.Fatal("invalid inventory accepted")
				}
				for _, mention := range test.mentions {
					if !strings.Contains(err.Error(), mention) {
						t.Fatalf("error %q does not name %q", err, mention)
					}
				}
				for _, value := range slices.Concat(slices.Collect(maps.Values(values)), []string{badDeviceID, badAccountUUID}) {
					if strings.Contains(err.Error(), value) {
						t.Fatalf("error %q carries a resolved value", err)
					}
				}
				return
			}
			if err != nil || !slices.Equal(accounts.Candidates(), test.aliases) {
				t.Fatalf("got %v %v, want aliases %v", accounts, err, test.aliases)
			}
			for alias, port := range map[string]string{"sample-a": "8001", "sample-b": "8002"} {
				a := accounts.lookup(alias)
				proxy, err := a.transport.Proxy(&http.Request{})
				password, present := proxy.User.Password()
				if err != nil || proxy.Host != "proxy.example.test:"+port || proxy.User.Username() != "synthetic-value-proxy-user" || !present || password != "synthetic-value-proxy-password" {
					t.Fatalf("%s: transport does not use the account's own exit and credentials", alias)
				}
				if a.expectedIP != netip.MustParseAddr("192.0.2.1") || a.email != alias+"@example.test" {
					t.Fatalf("%s: expected exit IP %v, email %q", alias, a.expectedIP, a.email)
				}
				if a.exit.User != nil {
					t.Fatal("exit URL carries credentials")
				}
			}
			if got := accounts.lookup("sample-a"); claudeOf(got).deviceID != syntheticDeviceID || claudeOf(got).accountUUID != syntheticAccountUUID {
				t.Fatalf("sample-a: identity %q %q", claudeOf(got).deviceID, claudeOf(got).accountUUID)
			}
			if got := accounts.lookup("sample-b"); claudeOf(got).deviceID != "" || claudeOf(got).accountUUID != "" {
				t.Fatalf("sample-b: identity %q %q, want empty", claudeOf(got).deviceID, claudeOf(got).accountUUID)
			}
			if accounts.lookup("sample-a").transport == accounts.lookup("sample-b").transport {
				t.Fatal("accounts share a transport")
			}
			// The shared proxy references are resolved once; a skipped account's are not at all.
			if resolved["op://example-proxies/proxy-account/username"] != 1 || resolved["op://example-proxies/proxy-account/password"] != 1 || resolved["op://example-vault/token-missing/credential"] != 0 {
				t.Fatalf("resolver calls %v, want shared references once and skipped accounts never", resolved)
			}
		})
	}
}

// TestLoadAccountsCancellation checks that a resolver that does not return stops the
// load at the deadline instead of being called for every reference.
func TestLoadAccountsCancellation(t *testing.T) {
	a := inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", "8001")
	dir := writeInventory(t, map[string]string{"sample-a.yaml": a})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	resolve := func(ctx context.Context, _ string) (string, error) {
		calls++
		<-ctx.Done()
		return "", ctx.Err()
	}
	if accounts, err := LoadAccounts(ctx, dir, Claude, resolve, DefaultAccountConcurrency); accounts != nil || err == nil || calls != 1 {
		t.Fatalf("got %v %v after %d calls, want a failure after the first stalled call", accounts, err, calls)
	}
}

// TestAccountTokenInjection checks the entrypoint end to end: the bound account's token
// replaces every client credential, records contain neither it nor the session
// credential, and an unknown credential is refused before it reaches the upstream.
func TestAccountTokenInjection(t *testing.T) {
	const requestBody = `{"messages":[{"role":"user","content":"Hi"}]}`
	h := newHarness(t, "sample-a")
	token := tokenOf(h.accounts.lookup("sample-a"))
	received := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		received <- err == nil && string(body) == requestBody && r.Header.Get("Authorization") == "Bearer "+token &&
			r.Header.Get("X-Api-Key") == "" && r.Header.Get("Cookie") == "" && r.Header.Get("Proxy-Authorization") == ""
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"content":[{"type":"text","text":"Hi"}]}`); err != nil {
			return
		}
	}))
	defer upstream.Close()
	records := make(chan captureRecord, 2)
	store := newRecordStore(t.TempDir(), h.accounts.knownValueReplacer())
	proxy := h.captured(upstream.URL, func(record captureRecord) error {
		if err := store.write(record); err != nil {
			return err
		}
		records <- record
		return nil
	})
	client := h.client()
	req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "synthetic-client-key")
	req.Header.Set("Cookie", "synthetic-client-cookie")
	req.Header.Set("Proxy-Authorization", "synthetic-client-proxy")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if res.StatusCode != 200 || readErr != nil || closeErr != nil || !strings.Contains(string(body), "Hi") {
		t.Fatal("controlled response changed")
	}
	if !<-received {
		t.Fatal("account token or original body not received")
	}
	record := <-records
	if len(record.Points["client_in"].Headers["X-Api-Key"]) != 1 || len(record.Points["upstream_out"].Headers["X-Api-Key"]) != 0 {
		t.Fatal("credential policy applied before the incoming observation")
	}
	if got := record.Points["client_in"].Headers["Authorization"]; len(got) != 1 || got[0] != redacted {
		t.Fatalf("session credential saved as %q, want full redaction", got)
	}
	if record.Account != "sample-a" {
		t.Fatal("record does not name the bound account")
	}
	data, err := json.Marshal(record)
	if err != nil || strings.Contains(string(data), token) || strings.Contains(string(data), h.token) || !strings.Contains(string(data), "Hi") {
		t.Fatal("credential capture boundary or safe text changed")
	}
	files, err := os.ReadDir(store.dir)
	if err != nil || len(files) != 1 {
		t.Fatal("observation not committed")
	}
	artifact, err := os.ReadFile(filepath.Join(store.dir, files[0].Name()))
	if err != nil || strings.Contains(string(artifact), token) || strings.Contains(string(artifact), h.token) || !strings.Contains(string(artifact), "Hi") || !strings.Contains(string(artifact), redacted) {
		t.Fatal("committed observation lost its safe boundary")
	}
	// Control arm: a credential the store never issued.
	req, err = http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	res, err = h.clientFor("sk-ant-gws-" + strings.Repeat("A", 43)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	record = <-records
	if res.StatusCode != 401 || record.Points["upstream_out"].Reached || !record.Points["client_out"].Body.Complete || record.Account != "" {
		t.Fatal("unknown session credential reached the upstream")
	}
}

// TestPartialTokenSavedCapture checks saved records at the partial-form boundary: a
// 24-byte account token keeps its first 12 and last 4 bytes, a 23-byte token is fully
// hidden, the proxy account and the session credential are always fully hidden, and
// every record names its account.
func TestPartialTokenSavedCapture(t *testing.T) {
	for _, test := range []struct {
		name, token, shown string
	}{
		// claude- lowercase values are what the model classifier keeps verbatim.
		{"24_bytes", "claude-synthetic-abc1234", "claude-synth***1234"},
		{"23_bytes", "claude-synthetic-abc123", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if len(test.token) != map[string]int{"24_bytes": 24, "23_bytes": 23}[test.name] {
				t.Fatal("fixture length changed")
			}
			h := newHarness(t)
			a := h.add("sample-test", test.token)
			// Header names are canonicalized on the wire; this value already is.
			a.proxyPassword = "Synthetic-Password"
			h.token, h.sessionID = h.issue()
			// The model field is kept verbatim by the classifier, so only the store's
			// replacer stands between the token there and the file.
			requestBody := `{"model":"` + test.token + `","messages":[{"role":"user","content":"Hi"}]}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, `{"content":[{"type":"text","text":"Hi"}]}`); err != nil {
					return
				}
			}))
			defer upstream.Close()
			target, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			entrypoint, err := NewHandler(target, Claude, dir, h.store, h.accounts, DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			served := make(chan struct{}, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entrypoint.Reverse.ServeHTTP(w, r)
				served <- struct{}{}
			}))
			defer proxy.Close()
			req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages", strings.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header[a.proxyPassword] = []string{"x"}
			res, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, res.Body); err != nil || res.Body.Close() != nil || res.StatusCode != 200 {
				t.Fatal("controlled response changed")
			}
			select {
			case <-served:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not finish")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatal("observation not committed")
			}
			artifact, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Account string `json:"account_alias"`
				Points  map[string]struct {
					Headers http.Header `json:"headers"`
					Body    struct {
						Analysis struct {
							Data struct {
								Model string `json:"model"`
							} `json:"data"`
						} `json:"safe_analysis"`
					} `json:"body"`
				} `json:"points"`
			}
			if err := json.Unmarshal(artifact, &saved); err != nil {
				t.Fatal(err)
			}
			wantHeader, wantModel := "Bearer "+test.shown, test.shown
			if test.shown == "" {
				wantHeader, wantModel = redacted, knownCredential
			}
			if saved.Account != "sample-test" {
				t.Fatal("record does not name its account")
			}
			if got := saved.Points["upstream_out"].Headers["Authorization"]; len(got) != 1 || got[0] != wantHeader {
				t.Fatalf("upstream Authorization saved as %q, want %q", got, wantHeader)
			}
			if got := saved.Points["client_in"].Body.Analysis.Data.Model; got != wantModel {
				t.Fatalf("token in a kept field saved as %q, want %q", got, wantModel)
			}
			if got := saved.Points["client_in"].Headers["Authorization"]; len(got) != 1 || got[0] != redacted {
				t.Fatalf("session credential saved as %q", got)
			}
			if len(saved.Points["client_in"].Headers[knownCredential]) != 1 {
				t.Fatal("proxy password not replaced by the full marker")
			}
			if strings.Contains(string(artifact), test.token) || strings.Contains(string(artifact), a.proxyPassword) || strings.Contains(string(artifact), h.token) {
				t.Fatal("saved record kept a whole credential")
			}
		})
	}
}

// TestKnownCredentialsSavedCapture checks that the handler's store removes the token
// and the URL-escaped proxy password from positions the classifiers keep verbatim: the
// model field and a header name.
func TestKnownCredentialsSavedCapture(t *testing.T) {
	h := newHarness(t)
	a := h.add("sample-test", "claude-synthetic-"+strings.ToLower(rand.Text()))
	a.proxyUsername = "synthetic-user-" + strings.ToLower(rand.Text())
	a.proxyPassword = "Synthetic password" + strings.ToLower(rand.Text())
	h.token, h.sessionID = h.issue()
	escaped := url.PathEscape(a.proxyPassword)
	requestBody := `{"model":"` + tokenOf(a) + `","messages":[{"role":"user","content":"Hi"}]}`
	var decoded any
	if err := json.Unmarshal([]byte(requestBody), &decoded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmtSafeJSON(safeValue("", decoded)), tokenOf(a)) || len(safeHeaders(http.Header{escaped: {"x"}})[escaped]) != 1 {
		t.Fatal("classifiers no longer keep the positions this test relies on")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"model":"`+tokenOf(a)+`","content":[{"type":"text","text":"Hi"}]}`); err != nil {
			return
		}
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	entrypoint, err := NewHandler(target, Claude, dir, h.store, h.accounts, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	// The record is saved when the handler returns, which can be after the client has
	// read the response.
	served := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entrypoint.Reverse.ServeHTTP(w, r)
		served <- struct{}{}
	}))
	defer proxy.Close()
	req, err := http.NewRequestWithContext(t.Context(), "POST", proxy.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header[escaped] = []string{"x"}
	res, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil || res.Body.Close() != nil || res.StatusCode != 200 {
		t.Fatal("controlled response changed")
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not finish")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("observation not committed")
	}
	artifact, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(artifact), tokenOf(a)) || strings.Contains(string(artifact), escaped) ||
		!strings.Contains(string(artifact), knownCredential) {
		t.Fatal("saved record kept a known credential")
	}
}

// TestKnownCredentialForms checks each encoded form of the proxy account in a saved
// record, for each of two accounts. Each arm first saves without the replacer to show
// that the record carries the form, then saves with it.
func TestKnownCredentialForms(t *testing.T) {
	set := &AccountSet{byAlias: map[string]*account{}, provider: Claude}
	for _, alias := range []string{"sample-a", "sample-b"} {
		set.byAlias[alias] = &account{alias: alias, credential: claudeCredential{token: "sk-ant-oat01-synthetic-" + rand.Text()}, proxyUsername: "customer-synthetic_" + rand.Text(), proxyPassword: "p@ss word<&" + rand.Text()}
	}
	for _, a := range set.byAlias {
		// secret is the part of form that must not reach the file.
		for _, test := range []struct{ name, form, secret string }{
			{"base64_basic", base64.StdEncoding.EncodeToString([]byte(a.proxyUsername + ":" + a.proxyPassword)), ""},
			{"query_escaped", url.QueryEscape(a.proxyPassword), ""},
			{"path_escaped", url.PathEscape(a.proxyPassword), ""},
			{"proxy_userinfo", url.UserPassword(a.proxyUsername, a.proxyPassword).String(),
				strings.TrimPrefix(url.UserPassword("", a.proxyPassword).String(), ":")},
			{"json_escaped", a.proxyPassword, ""},
		} {
			t.Run(a.alias+"/"+test.name, func(t *testing.T) {
				secret := cmp.Or(test.secret, test.form)
				quoted, err := json.Marshal(secret)
				if err != nil {
					t.Fatal(err)
				}
				saved := string(quoted[1 : len(quoted)-1])
				record := captureRecord{ID: test.name, Points: map[string]point{"client_in": {Reached: true, Headers: http.Header{"X-Leak": {test.form}}}}}
				for _, known := range []*strings.Replacer{nil, set.knownValueReplacer()} {
					store := newRecordStore(t.TempDir(), known)
					if err := store.write(record); err != nil {
						t.Fatal(err)
					}
					artifact, err := os.ReadFile(filepath.Join(store.dir, test.name+".json"))
					if err != nil {
						t.Fatal(err)
					}
					if known == nil && !strings.Contains(string(artifact), saved) {
						t.Fatal("record without the replacer does not carry the form")
					}
					if known != nil && (strings.Contains(string(artifact), saved) || !strings.Contains(string(artifact), knownCredential)) {
						t.Fatal("saved record kept a known credential form")
					}
				}
			})
		}
	}
}

// TestAuthorizationHeaderShapes checks the classifier that decides what a record shows
// of an Authorization value: an account's OAuth token in its partial form, a session
// credential fully redacted, and any other shape fully redacted, including a session
// credential behind two spaces, which would otherwise be classified as an OAuth token.
func TestAuthorizationHeaderShapes(t *testing.T) {
	sessionToken := "sk-ant-gws-" + strings.Repeat("A", 43)
	for _, test := range []struct{ value, want string }{
		{"Bearer sk-ant-oat01-synthetic-account-token", "Bearer sk-ant-oat01***oken"},
		{"bearer sk-ant-oat01-synthetic-account-token", "bearer sk-ant-oat01***oken"},
		{"Bearer " + sessionToken, redacted},
		{"Bearer  " + sessionToken, redacted},
		{"Bearer\t" + sessionToken, redacted},
		{" Bearer " + sessionToken + " ", redacted},
		{"Bearer " + sessionToken + " extra", redacted},
		{"Bearer short", redacted},
		{"Basic c3ludGhldGljOnN5bnRoZXRpYw==", redacted},
		{sessionToken, redacted},
	} {
		if got := safeAuthorization(test.value); got != test.want {
			t.Fatalf("%q classified as %q, want %q", test.value, got, test.want)
		}
	}
}

// TestMalformedBearerIsFullyRedactedInSavedCapture sends a session credential behind
// two spaces: the router refuses it, the capture still records the request, and the
// saved file holds neither the credential nor its last four bytes. Control arm: the
// same credential behind one space is accepted and also fully redacted.
func TestMalformedBearerIsFullyRedactedInSavedCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	store := newRecordStore(t.TempDir(), h.accounts.knownValueReplacer())
	committed := make(chan error, 2)
	server := h.captured(upstream.URL, func(record captureRecord) error {
		err := store.write(record)
		committed <- err
		return err
	})
	for _, test := range []struct {
		header string
		status int
	}{{"Bearer " + h.token, 204}, {"Bearer  " + h.token, 401}} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", test.header)
		res, err := h.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil || res.StatusCode != test.status {
			t.Fatalf("status=%d want=%d", res.StatusCode, test.status)
		}
		select {
		case err := <-committed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("record missing")
		}
	}
	files, err := os.ReadDir(store.dir)
	if err != nil || len(files) != 2 {
		t.Fatal("records not committed")
	}
	for _, file := range files {
		artifact, err := os.ReadFile(filepath.Join(store.dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(artifact), `"Authorization":["`+redacted+`"]`) {
			t.Fatalf("%s: session credential not fully redacted", file.Name())
		}
		if strings.Contains(string(artifact), h.token[len(h.token)-4:]) || strings.Contains(string(artifact), h.token[:12]) {
			t.Fatalf("%s: a piece of the session credential was saved", file.Name())
		}
	}
}

// directExitYAML renders an account file with a direct exit expecting ip.
func directExitYAML(alias, ip string) string {
	return "email: " + alias + "@example.test\naccess: gateway\nstatus: active\noauth_token: op://example-vault/token-" + alias +
		"/credential\nproxy:\n  direct: true\n  expected_egress_ip: " + ip + "\n"
}

// TestDirectAccountInventory checks which direct exits load: proxy.direct with only
// expected_egress_ip loads beside an account with a proxy; direct with any proxy field, a missing proxy and a second direct account
// are refused, naming the field or both aliases. A loaded direct account dials in the
// family of its expected IP and leaves known-value redaction intact.
func TestDirectAccountInventory(t *testing.T) {
	proxied := inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", "8001")
	direct := directExitYAML("sample-d", "192.0.2.7")
	for _, test := range []struct {
		name     string
		files    map[string]string
		mentions []string // nil: loads
	}{
		{"direct_beside_proxy", map[string]string{"sample-a.yaml": proxied, "sample-d.yaml": direct}, nil},
		{"direct_ipv6", map[string]string{"sample-d.yaml": directExitYAML("sample-d", "2001:db8::7")}, nil},
		{"direct_with_host", map[string]string{"sample-d.yaml": direct + "  host: proxy.example.test\n"}, []string{"sample-d: proxy.host: not allowed with proxy.direct"}},
		{"direct_with_port", map[string]string{"sample-d.yaml": direct + "  port: 8004\n"}, []string{"sample-d: proxy.port: not allowed with proxy.direct"}},
		{"direct_with_references", map[string]string{"sample-d.yaml": direct + "  username_ref: op://example-proxies/proxy-account/username\n  password_ref: op://example-proxies/proxy-account/password\n"},
			[]string{"sample-d: proxy.username_ref: not allowed with proxy.direct", "sample-d: proxy.password_ref: not allowed with proxy.direct"}},
		{"direct_without_ip", map[string]string{"sample-d.yaml": strings.Replace(direct, "  expected_egress_ip: 192.0.2.7\n", "", 1)}, []string{"sample-d: proxy.expected_egress_ip"}},
		{"direct_false", map[string]string{"sample-d.yaml": strings.Replace(direct, "direct: true", "direct: false", 1)}, []string{"sample-d: proxy.host", "sample-d: proxy.port", "sample-d: proxy.username_ref"}},
		{"no_proxy", map[string]string{"sample-d.yaml": direct[:strings.Index(direct, "proxy:")]}, []string{"sample-d: proxy.host", "sample-d: proxy.expected_egress_ip"}},
		{"two_direct", map[string]string{"sample-d.yaml": direct, "sample-e.yaml": directExitYAML("sample-e", "192.0.2.8")}, []string{"more than one direct account: sample-d, sample-e"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolve := func(_ context.Context, reference string) (string, error) {
				return "synthetic-value-" + strings.TrimPrefix(reference, "op://"), nil
			}
			set, err := LoadAccounts(t.Context(), writeInventory(t, test.files), Claude, resolve, DefaultAccountConcurrency)
			if test.mentions != nil {
				if err == nil {
					t.Fatal("invalid inventory accepted")
				}
				for _, mention := range test.mentions {
					if !strings.Contains(err.Error(), mention) {
						t.Fatalf("error %q does not name %q", err, mention)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			d := set.lookup("sample-d")
			wantNetwork := map[bool]string{true: "tcp4", false: "tcp6"}[d.expectedIP.Is4()]
			if d.direct != wantNetwork || d.exit != nil || d.proxyUsername != "" || d.proxyPassword != "" || d.exitName() != "direct" {
				t.Fatalf("direct account: network %q, exit %v, proxy account set %t", d.direct, d.exit, d.proxyUsername != "" || d.proxyPassword != "")
			}
			if d.transport.Proxy != nil {
				t.Fatal("direct transport has a proxy")
			}
			if a := set.lookup("sample-a"); a != nil && (a.direct != "" || a.exit == nil) {
				t.Fatal("the account with a proxy became direct")
			}
			if got := set.knownValueReplacer().Replace("plain text"); got != "plain text" {
				t.Fatalf("redaction of plain text gave %q", got)
			}
		})
	}
}

// TestDirectAccountDials checks that every request of a direct account's transport (the
// one inference, quota refresh and token renewal use) dials the target it names from the
// gateway, in the family of expected_egress_ip only, through no proxy.
func TestDirectAccountDials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("request carried proxy credentials")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	for _, ip := range []string{"192.0.2.7", "2001:db8::7"} {
		t.Run(ip, func(t *testing.T) {
			a, err := newAccount("sample-d", accountInput{Credential: claudeCredential{token: "sk-ant-oat01-synthetic-d"}, Direct: true, ExpectedIP: netip.MustParseAddr(ip)}, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer a.transport.CloseIdleConnections()
			var dialed []string
			a.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				dialed = append(dialed, network+" "+address)
				return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
			}
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://upstream.example.test:8080/", nil)
			response, err := a.transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			want := map[bool]string{true: "tcp4", false: "tcp6"}[netip.MustParseAddr(ip).Is4()] + " upstream.example.test:8080"
			if response.StatusCode != http.StatusOK || !slices.Equal(dialed, []string{want}) {
				t.Fatalf("status %d, dialed %v, want [%s]", response.StatusCode, dialed, want)
			}
		})
	}
}
