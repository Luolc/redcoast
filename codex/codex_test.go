package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/session"
)

// codexBody is a request body of the Codex CLI's shape that carries the client's
// installation ID in its two places.
const codexBody = `{"model":"gpt-synthetic","input":[],"client_metadata":{"x-codex-installation-id":"` + clientInstallation +
	`","x-codex-turn-metadata":"{\"installation_id\":\"` + clientInstallation + `\",\"turn_id\":\"t1\"}"},"stream":true}`

// codexHeader is the turn metadata header of the same request.
func codexHeader() http.Header {
	return http.Header{"X-Codex-Turn-Metadata": {`{"installation_id":"` + clientInstallation + `","turn_id":"t1"}`}}
}

// TestForwarding checks what reaches the upstream for a Codex request: the path under
// the upstream's base URL, the account's token and account ID in place of the client's
// credentials, and the account's installation ID in place of the client's in the turn
// metadata header and both places in the body, at the same length. The control arm, a
// Pi request without turn metadata, reaches the upstream with its body unchanged.
func TestForwarding(t *testing.T) {
	g := newGateway(t)
	header := codexHeader()
	header.Set("Chatgpt-Account-Id", fakeAccountID)
	header.Set("X-Api-Key", "synthetic-client-key")
	header.Set("Cookie", "synthetic-client-cookie")
	if status, body := g.do(http.MethodPost, "/backend-api/codex/responses", codexBody, header); status != http.StatusOK {
		t.Fatalf("status %d %q", status, body)
	}
	piBody := `{"model":"gpt-synthetic","input":[],"prompt_cache_key":"` + clientInstallation + `"}`
	if status, body := g.do(http.MethodPost, "/backend-api/codex/responses", piBody, http.Header{"Originator": {"pi"}}); status != http.StatusOK {
		t.Fatalf("Pi request: status %d %q", status, body)
	}
	seen := g.exit.exchanges()
	if len(seen) != 2 {
		t.Fatalf("the exit saw %d requests, want 2", len(seen))
	}
	codex, pi := seen[0], seen[1]
	if codex.path != "/backend-api/codex/responses" {
		t.Fatalf("upstream path %q", codex.path)
	}
	if codex.header.Get("Authorization") != "Bearer "+syntheticAccessToken || codex.header.Get("Chatgpt-Account-Id") != syntheticAccountID {
		t.Fatal("the upstream did not get the account's token and account ID")
	}
	if codex.header.Get("X-Api-Key") != "" || codex.header.Get("Cookie") != "" || strings.Contains(codex.header.Get("Authorization"), g.token) {
		t.Fatal("a client credential reached the upstream")
	}
	want := strings.ReplaceAll(codexBody, clientInstallation, accountInstallation)
	if codex.body != want || len(codex.body) != len(codexBody) || codex.header.Get("Content-Length") != strconv.Itoa(len(codexBody)) {
		t.Fatalf("upstream body\n%s\nwant\n%s", codex.body, want)
	}
	if got := codex.header.Get("X-Codex-Turn-Metadata"); got != strings.ReplaceAll(codexHeader().Get("X-Codex-Turn-Metadata"), clientInstallation, accountInstallation) {
		t.Fatalf("upstream turn metadata %q", got)
	}
	if pi.body != piBody || pi.header.Get("Originator") != "pi" || pi.header.Get("Authorization") != "Bearer "+syntheticAccessToken {
		t.Fatalf("control arm: Pi's request reached the upstream as %q", pi.body)
	}
}

// TestBodyInstallationID checks requests whose turn metadata carries no installation
// ID, as the Codex CLI's memory requests do: the client's ID in the body's
// client_metadata reaches the upstream as the account's, at the same length. A body
// whose ID the gateway cannot read gets 502 and nothing of it reaches the upstream; a compressed body gets 400 unless it comes from Pi, whose compressed body
// goes upstream unchanged.
func TestBodyInstallationID(t *testing.T) {
	g := newGateway(t)
	memoryBody := `{"model":"gpt-synthetic","input":[],"client_metadata":{"x-codex-installation-id":"` + clientInstallation + `"},"stream":true}`
	memoryHeader := http.Header{"X-Codex-Turn-Metadata": {`{"turn_id":"t2"}`}}
	for _, test := range []struct {
		name, body string
		header     http.Header
		status     int
		upstream   string // the body the upstream gets, when it gets one
	}{
		{"memory", memoryBody, memoryHeader, http.StatusOK, strings.ReplaceAll(memoryBody, clientInstallation, accountInstallation)},
		{"no_turn_metadata", memoryBody, nil, http.StatusOK, strings.ReplaceAll(memoryBody, clientInstallation, accountInstallation)},
		{"id_upper_case", strings.ReplaceAll(memoryBody, clientInstallation, strings.ToUpper(clientInstallation)), memoryHeader, http.StatusBadGateway, ""},
		{"not_json", "synthetic " + memoryBody, memoryHeader, http.StatusBadGateway, ""},
		{"compressed", memoryBody, http.Header{"Content-Encoding": {"zstd"}}, http.StatusBadRequest, ""},
		{"compressed_pi", "synthetic-zstd-frame", http.Header{"Content-Encoding": {"zstd"}, "Originator": {"pi"}}, http.StatusOK, "synthetic-zstd-frame"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(g.exit.exchanges())
			status, body := g.do(http.MethodPost, "/backend-api/codex/responses", test.body, test.header)
			seen := g.exit.exchanges()[before:]
			for _, e := range seen {
				if strings.Contains(strings.ToLower(e.body), clientInstallation) {
					t.Fatalf("the client's installation ID reached the upstream: %q", e.body)
				}
			}
			if status != test.status {
				t.Fatalf("status %d %q, want %d", status, body, test.status)
			}
			if test.upstream == "" {
				if len(seen) != 0 {
					t.Fatalf("%d requests upstream, want none", len(seen))
				}
				return
			}
			if len(seen) != 1 || seen[0].body != test.upstream || seen[0].header.Get("Content-Length") != strconv.Itoa(len(test.body)) {
				t.Fatalf("upstream got %+v, want body %q", seen, test.upstream)
			}
		})
	}
}

// TestScreen checks the requests answered before anything goes upstream: a WebSocket
// upgrade (426), turn metadata the gateway cannot rewrite (400), a compressed body with
// an installation ID (400), a request line off the list (403) and one that would leave
// with a header name twice (502). The control arm sends the same request without the
// fault and it reaches the upstream.
func TestScreen(t *testing.T) {
	g := newGateway(t)
	for _, test := range []struct {
		name, method, path string
		header             http.Header
		status             int
	}{
		{"websocket", http.MethodGet, "/backend-api/codex/responses", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"c3ludGhldGljLWtleS0xMjM0"}}, http.StatusUpgradeRequired},
		{"metadata_not_json", http.MethodPost, "/backend-api/codex/responses", http.Header{"X-Codex-Turn-Metadata": {"synthetic"}}, http.StatusBadRequest},
		{"metadata_short_id", http.MethodPost, "/backend-api/codex/responses", http.Header{"X-Codex-Turn-Metadata": {`{"installation_id":"a"}`}}, http.StatusBadRequest},
		{"metadata_twice", http.MethodPost, "/backend-api/codex/responses", http.Header{"X-Codex-Turn-Metadata": {codexHeader().Get("X-Codex-Turn-Metadata"), "{}"}}, http.StatusBadRequest},
		{"compressed", http.MethodPost, "/backend-api/codex/responses", http.Header{"X-Codex-Turn-Metadata": codexHeader()["X-Codex-Turn-Metadata"], "Content-Encoding": {"zstd"}}, http.StatusBadRequest},
		{"not_listed", http.MethodPost, "/backend-api/codex/models", nil, http.StatusForbidden},
		{"get", http.MethodGet, "/backend-api/codex/responses", nil, http.StatusForbidden},
		{"account_id_twice", http.MethodPost, "/backend-api/codex/responses", http.Header{"chatgpt_account_id": {"synthetic-other-account"}}, http.StatusBadGateway},
		{"header_twice", http.MethodPost, "/backend-api/codex/responses", http.Header{"Accept": {"text/event-stream", "*/*"}}, http.StatusBadGateway},
		{"control", http.MethodPost, "/backend-api/codex/responses", codexHeader(), http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(g.exit.exchanges())
			body := codexBody
			if test.method == http.MethodGet {
				body = ""
			}
			status, _ := g.do(test.method, test.path, body, test.header)
			reached := len(g.exit.exchanges()) - before
			if status != test.status || reached != map[bool]int{true: 1, false: 0}[test.status == http.StatusOK] {
				t.Fatalf("status %d, %d requests upstream; want %d", status, reached, test.status)
			}
		})
	}
}

// TestCrossProviderCredentials checks that a session credential of one provider is
// refused by the other's gateway before anything goes upstream: inference gets 401 and
// CONNECT 407, for a Claude session credential at the Codex gateway and a Codex one at
// a Claude gateway in front of the same exit. The control arms send each gateway its
// own credential.
func TestCrossProviderCredentials(t *testing.T) {
	g := newGateway(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "inventory", "sample-a.yaml"), inventoryYAML(g.exit.port(t), "oauth_token: env://SYNTHETIC_CLAUDE\n"), 0o700, 0o600)
	set, err := claude.LoadAccounts(t.Context(), filepath.Join(dir, "inventory"), claude.Claude, resolve, 4)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open(t.Context(), filepath.Join(dir, "claude.sqlite"), session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SetPlanSchedule(t.Context(), session.PlanPeriod{Alias: "sample-a", Plan: "pro", From: time.UnixMilli(0), Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse("http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	entrypoint, err := claude.NewHandler(upstream, claude.Claude, "", store, claude.NewAccounts(set, store), claude.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	claudeGateway, claudeForward := httptest.NewServer(entrypoint.Reverse), httptest.NewServer(entrypoint.Forward)
	t.Cleanup(claudeGateway.Close)
	t.Cleanup(claudeForward.Close)
	issued, err := store.Issue(t.Context(), "test-machine", session.LocalSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	post := func(server, path, token string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, test := range []struct {
		name, server, path, token string
		status                    int
	}{
		{"claude_at_codex", g.server.URL, "/backend-api/codex/responses", issued.Token, http.StatusUnauthorized},
		{"codex_at_claude", claudeGateway.URL, "/v1/messages", g.token, http.StatusUnauthorized},
		{"codex_at_codex", g.server.URL, "/backend-api/codex/responses", g.token, http.StatusOK},
		{"claude_at_claude", claudeGateway.URL, "/v1/messages", issued.Token, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(g.exit.exchanges())
			status := post(test.server, test.path, test.token)
			reached := len(g.exit.exchanges()) - before
			if status != test.status || (status == http.StatusOK) != (reached == 1) {
				t.Fatalf("status %d, %d requests upstream; want %d", status, reached, test.status)
			}
		})
	}
	// The tunnels go to a name the exit cannot open; only whether the proxy
	// authenticated the credential matters.
	for _, test := range []struct {
		name, address, token string
		refused              bool
	}{
		{"connect_claude_at_codex", g.forward.Listener.Addr().String(), issued.Token, true},
		{"connect_codex_at_claude", claudeForward.Listener.Addr().String(), g.token, true},
		{"connect_codex_at_codex", g.forward.Listener.Addr().String(), g.token, false},
		{"connect_claude_at_claude", claudeForward.Listener.Addr().String(), issued.Token, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := connect(t, test.address, "example.invalid:443", test.token); (status == http.StatusProxyAuthRequired) != test.refused {
				t.Fatalf("status %d, refused %v", status, test.refused)
			}
		})
	}
}

// TestForwardProxyDenyList checks that the Codex gateway's forward proxy refuses the
// hosts of the shared deny list with 403 for its own session, before any exit is
// dialed; the control arm, another host, is not refused.
func TestForwardProxyDenyList(t *testing.T) {
	g := newGateway(t)
	for _, test := range []struct {
		target string
		denied bool
	}{
		{"chatgpt.com:443", true},
		{"auth.openai.com:443", true},
		{"o33249.ingest.us.sentry.io:443", true},
		{"example.invalid:443", false},
	} {
		before := len(g.exit.exchanges())
		status := connect(t, g.forward.Listener.Addr().String(), test.target, g.token)
		if (status == http.StatusForbidden) != test.denied || test.denied && len(g.exit.exchanges()) != before {
			t.Fatalf("%s: status %d, want denied %v", test.target, status, test.denied)
		}
	}
}

// TestSessionForm checks the session credential: a JWT that Pi accepts (the fake
// account ID at the claim Pi reads, no expiry), signed so that a changed claim, another
// key or another algorithm is refused, and holding the store's credential, which comes
// back out.
func TestSessionForm(t *testing.T) {
	form, err := NewSessionForm([]byte(syntheticSessionKey))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewSessionForm([]byte(syntheticSessionKey + "-other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSessionForm([]byte("short")); err == nil {
		t.Fatal("a short key was accepted")
	}
	inner := session.TokenPrefix + strings.Repeat("A", 43)
	jwt := form.Wrap(inner)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %d parts", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth["chatgpt_account_id"] != fakeAccountID || claims["exp"] != nil {
		t.Fatalf("claims %v", claims)
	}
	if got, ok := form.Unwrap(jwt); !ok || got != inner {
		t.Fatal("the form did not give back its own credential")
	}
	forged := strings.Replace(string(payload), "redcoast", "forged00", 1)
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + "."
	for name, presented := range map[string]string{
		"other_key":     other.Wrap(inner),
		"changed_claim": parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + parts[2],
		"alg_none":      none,
		"bare":          inner,
		"two_parts":     parts[0] + "." + parts[1],
		"oversize":      jwt + strings.Repeat("A", maxSessionCredential),
	} {
		if _, ok := form.Unwrap(presented); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestQuotaReadings checks the parsing of the x-codex-* headers: a plan with a weekly
// primary window and an empty secondary one, a plan with both windows, and readings
// that are malformed or of a window length the store has none for.
func TestQuotaReadings(t *testing.T) {
	reset := time.Unix(1700000000, 0).UTC()
	for _, test := range []struct {
		name   string
		header map[string]string
		want   []session.Reading
	}{
		{"weekly_only", map[string]string{"Primary-Used-Percent": "3", "Primary-Window-Minutes": "10080", "Primary-Reset-At": "1700000000", "Secondary-Used-Percent": "0", "Secondary-Window-Minutes": "0", "Secondary-Reset-At": ""},
			[]session.Reading{{Window: session.Window7d, Utilization: 0.03, ResetAt: reset, Source: "response"}}},
		{"both", map[string]string{"Primary-Used-Percent": "42.5", "Primary-Window-Minutes": "300", "Secondary-Used-Percent": "7", "Secondary-Window-Minutes": "10080", "Secondary-Reset-At": "1700000000"},
			[]session.Reading{{Window: session.Window5h, Utilization: 0.425, Source: "response"}, {Window: session.Window7d, Utilization: 0.07, ResetAt: reset, Source: "response"}}},
		{"malformed", map[string]string{"Primary-Used-Percent": "NaN", "Primary-Window-Minutes": "300", "Secondary-Used-Percent": "7", "Secondary-Window-Minutes": "10080", "Secondary-Reset-At": "soon"}, nil},
		{"other_window", map[string]string{"Primary-Used-Percent": "5", "Primary-Window-Minutes": "60"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			for name, value := range test.header {
				header.Set("X-Codex-"+name, value)
			}
			got := quotaReadings(header, "response")
			if len(got) != len(test.want) {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("got %+v, want %+v", got, test.want)
				}
			}
		})
	}
}

// TestQuotaRecordedFromHeadOfStream checks that the readings of an upstream stream are
// stored once its head arrives, without a Content-Type, even when the client leaves
// before the stream ends; and that the client leaving closes the upstream's request
// at once. Control: the readings were absent before the request.
func TestQuotaRecordedFromHeadOfStream(t *testing.T) {
	g := newGateway(t)
	closed := make(chan struct{})
	g.exit.setAnswer(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.Header().Set("X-Codex-Primary-Used-Percent", "12")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(closed)
		case <-time.After(10 * time.Second):
		}
	})
	if quota, err := g.store.LatestQuota(t.Context(), "sample-a"); err != nil || len(quota) != 0 {
		t.Fatalf("readings before the request: %v %v", quota, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.server.URL+"/backend-api/codex/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	res, err := g.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = res.Body.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream's request stayed open after the client left")
	}
	quota, err := g.store.LatestQuota(t.Context(), "sample-a")
	if err != nil || quota[session.Window7d].Utilization != 0.12 {
		t.Fatalf("stored readings %+v %v", quota, err)
	}
}

// TestUpstreamUnauthorizedPauses checks that an upstream 401 pauses the account, and
// that a 200 does not.
func TestUpstreamUnauthorizedPauses(t *testing.T) {
	g := newGateway(t)
	if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", "{}", nil); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if reason, err := g.store.PauseReason(t.Context(), "sample-a"); err != nil || reason != "" {
		t.Fatalf("control arm: paused for %q after a 200, %v", reason, err)
	}
	g.exit.setAnswer(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	if status, _ := g.do(http.MethodPost, "/backend-api/codex/responses", "{}", nil); status != http.StatusUnauthorized {
		t.Fatalf("status %d", status)
	}
	if reason, err := g.store.PauseReason(t.Context(), "sample-a"); err != nil || reason != "upstream_401" {
		t.Fatalf("paused for %q, %v", reason, err)
	}
}

// TestAccountInventory checks a Codex account's inventory fields and login: the
// installation ID must be present and a lower-case UUID v4, the login private and
// complete, and the email present. Errors name the alias and field and carry no value.
func TestAccountInventory(t *testing.T) {
	login := authJSON()
	for _, test := range []struct {
		name, extra, auth string
		authMode          os.FileMode
		problem           string // empty when the account loads
	}{
		{"valid", "installation_id: " + accountInstallation + "\n", login, 0o600, ""},
		{"missing", "", login, 0o600, "sample-a: installation_id: missing"},
		{"no_email", "installation_id: " + accountInstallation + "\n", login, 0o600, "sample-a: email: missing"},
		{"null", "installation_id: null\n", login, 0o600, "sample-a: installation_id: missing"},
		{"not_a_document", "installation_id: [" + accountInstallation + "]\n", login, 0o600, "sample-a: file: not a Codex account document"},
		{"upper_case", "installation_id: " + strings.ToUpper(accountInstallation) + "\n", login, 0o600, "sample-a: installation_id: not a lower-case UUID v4"},
		{"version_1", "installation_id: 0b8c7e2a-3f41-1d2e-9a6b-5c1d2e3f4a5b\n", login, 0o600, "sample-a: installation_id: not a lower-case UUID v4"},
		{"readable_login", "installation_id: " + accountInstallation + "\n", login, 0o644, "sample-a: auth.json: must be a regular file"},
		{"no_account_id", "installation_id: " + accountInstallation + "\n", strings.Replace(login, syntheticAccountID, "", 1), 0o600, "sample-a: auth.json: tokens.account_id"},
		{"token_with_space", "installation_id: " + accountInstallation + "\n", strings.Replace(login, syntheticAccessToken, "synthetic token", 1), 0o600, "sample-a: auth.json: tokens.access_token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			file := inventoryYAML("8001", test.extra)
			if test.name == "no_email" {
				file = strings.Replace(file, "email: sample-a@example.test\n", "", 1)
			}
			writeFile(t, filepath.Join(dir, "inventory", "sample-a.yaml"), file, 0o700, 0o600)
			writeFile(t, filepath.Join(dir, "accounts", "sample-a", authFile), test.auth, 0o700, test.authMode)
			set, err := claude.LoadAccounts(t.Context(), filepath.Join(dir, "inventory"), New(filepath.Join(dir, "accounts")), resolve, 1)
			if test.problem == "" {
				if err != nil || len(set.Candidates()) != 1 {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("got %v, want %q", err, test.problem)
			}
			for _, value := range []string{syntheticAccessToken, syntheticAccountID, strings.ToUpper(accountInstallation)} {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("error %q carries a value", err)
				}
			}
		})
	}
}

// TestSwapReader checks the streaming replacement of an installation ID: every
// occurrence, one split across reads included, at the same length, and a body without
// one passes unchanged.
func TestSwapReader(t *testing.T) {
	for _, body := range []string{
		codexBody,
		clientInstallation + clientInstallation + "x" + clientInstallation,
		strings.Repeat("y", 70000) + clientInstallation + strings.Repeat("z", 3),
		"no identifier here",
		"",
	} {
		src := io.NopCloser(iotest.OneByteReader(strings.NewReader(body)))
		got, err := io.ReadAll(&swapReader{src: src, old: []byte(clientInstallation), new: []byte(accountInstallation)})
		if want := strings.ReplaceAll(body, clientInstallation, accountInstallation); err != nil || string(got) != want {
			t.Fatalf("got %q %v, want %q", got, err, want)
		}
		got, err = io.ReadAll(&swapReader{src: io.NopCloser(bytes.NewReader([]byte(body))), old: []byte(clientInstallation), new: []byte(accountInstallation)})
		if want := strings.ReplaceAll(body, clientInstallation, accountInstallation); err != nil || string(got) != want {
			t.Fatalf("whole reads: got %q %v", got, err)
		}
	}
}
