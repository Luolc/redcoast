package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// storeValues are the credential values behind the references of storeFixture; each is
// a distinctive string so that a copy of one in the store or in an answer would show.
var storeValues = map[string]string{
	"op://example-vault/token-sample-a/credential":       "sk-ant-oat01-synthetic-value-store-a",
	"op://example-vault/token-sample-b/credential":       "sk-ant-oat01-synthetic-value-store-b",
	"op://example-vault/token-sample-c/credential":       "sk-ant-oat01-synthetic-value-store-c",
	"op://example-vault/token-sample-f/credential":       "sk-ant-oat01-synthetic-value-store-f",
	"op://example-proxies/proxy-account/username":        "synthetic-value-store-proxy-user",
	"op://example-proxies/proxy-account/password":        "synthetic-value-store-proxy-password",
	"op://example-vault/token-sample-extra/credential":   "sk-ant-oat01-synthetic-value-store-extra",
	"op://example-vault/token-sample-changed/credential": "sk-ant-oat01-synthetic-value-store-changed",
}

// storeResolve resolves storeValues.
func storeResolve(_ context.Context, reference string) (string, error) {
	if value, known := storeValues[reference]; known {
		return value, nil
	}
	return "", errors.New("synthetic resolver: unknown reference")
}

// storeFixture writes an inventory of six accounts: two served with a proxy (one with a
// client identity), a paused one, a testing one without a token, one on the direct
// access path, and a served one with a direct exit. Only the first, second and last are
// served.
func storeFixture(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		"sample-a.yaml": inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", "8080") + "device_id: " + syntheticDeviceID + "\naccount_uuid: " + syntheticAccountUUID + "\n",
		"sample-b.yaml": inventoryYAML("sample-b", "active", "gateway", "op://example-vault/token-sample-b/credential", "8081"),
		"sample-c.yaml": inventoryYAML("sample-c", "paused", "gateway", "op://example-vault/token-sample-c/credential", "8082"),
		"sample-d.yaml": inventoryYAML("sample-d", "testing", "gateway", "", "8083"),
		"sample-e.yaml": "email: sample-e@example.test\naccess: direct\nstatus: active\n",
		"sample-f.yaml": "email: sample-f@example.test\naccess: gateway\nstatus: active\noauth_token: op://example-vault/token-sample-f/credential\nproxy:\n  direct: true\n  expected_egress_ip: 192.0.2.9\n",
	}
	return writeInventory(t, files)
}

// importFixture imports dir into the harness's store.
func importFixture(t *testing.T, h *harness, dir string) {
	t.Helper()
	imported, err := ImportInventory(t.Context(), h.store, dir, Claude)
	if err != nil || !imported {
		t.Fatalf("import = %v, %v", imported, err)
	}
}

// sameServed fails the test unless a and b serve the same aliases with the same
// configuration.
func sameServed(t *testing.T, a, b *AccountSet) {
	t.Helper()
	if !slices.Equal(a.Candidates(), b.Candidates()) {
		t.Fatalf("served accounts differ: %v and %v", a.Candidates(), b.Candidates())
	}
	for _, alias := range a.Candidates() {
		x, y := a.lookup(alias), b.lookup(alias)
		if !x.sameConfig(y) || x.email != y.email || x.direct != y.direct {
			t.Fatalf("%s differs between the sources", alias)
		}
	}
}

// TestImportServesTheSameAccounts checks that, once the inventory is imported, the
// accounts table serves exactly the accounts the directory does, with the same
// configuration, and that the table keeps the accounts the gateway does not serve.
// Control arms: a directory that changes afterwards no longer matters, and a second
// import writes nothing.
func TestImportServesTheSameAccounts(t *testing.T) {
	h := newHarness(t)
	dir := storeFixture(t)
	fromDir, err := LoadAccounts(t.Context(), dir, Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sample-a", "sample-b", "sample-f"}; !slices.Equal(fromDir.Candidates(), want) {
		t.Fatalf("directory serves %v, want %v", fromDir.Candidates(), want)
	}
	importFixture(t, h, dir)
	fromStore, err := LoadAccountsFrom(t.Context(), StoreSource(h.store, dir), Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	sameServed(t, fromDir, fromStore)
	if got := claudeOf(fromStore.lookup("sample-a")); got.deviceID != syntheticDeviceID || got.accountUUID != syntheticAccountUUID {
		t.Fatalf("sample-a serves identity %q %q", got.deviceID, got.accountUUID)
	}
	rows, initialized, err := h.store.Accounts(t.Context())
	if err != nil || !initialized || len(rows) != 6 {
		t.Fatalf("table holds %d rows, initialized %v, %v", len(rows), initialized, err)
	}
	byAlias := map[string]session.AccountRow{}
	for _, row := range rows {
		byAlias[row.Alias] = row
	}
	if byAlias["sample-c"].Status != "paused" || byAlias["sample-d"].Status != "testing" || byAlias["sample-d"].TokenRef != "" || byAlias["sample-e"].Access != "direct" || !byAlias["sample-f"].Direct || byAlias["sample-a"].Rev != 1 {
		t.Fatalf("rows = %+v", rows)
	}

	// The directory is no longer read: a new file and a removed one change nothing.
	if err := os.WriteFile(filepath.Join(dir, "sample-extra.yaml"), []byte(inventoryYAML("sample-extra", "active", "gateway", "op://example-vault/token-sample-extra/credential", "8090")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "sample-b.yaml")); err != nil {
		t.Fatal(err)
	}
	if imported, err := ImportInventory(t.Context(), h.store, dir, Claude); err != nil || imported {
		t.Fatalf("second import = %v, %v", imported, err)
	}
	again, err := LoadAccountsFrom(t.Context(), StoreSource(h.store, dir), Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	sameServed(t, fromDir, again)
	// Control: the directory does serve the other set now.
	changed, err := LoadAccounts(t.Context(), dir, Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil || slices.Equal(changed.Candidates(), fromDir.Candidates()) {
		t.Fatalf("directory after the change serves %v, %v", changed.Candidates(), err)
	}
}

// TestImportAllOrNothing checks that an inventory that does not load writes nothing, not
// even the marker, and that the same directory without the fault imports.
func TestImportAllOrNothing(t *testing.T) {
	h := newHarness(t)
	dir := writeInventory(t, map[string]string{
		"sample-a.yaml": inventoryYAML("sample-a", "active", "gateway", "op://example-vault/token-sample-a/credential", "8080"),
		"sample-b.yaml": inventoryYAML("sample-b", "active", "gateway", "", "8081"), // a served account without a token
	})
	imported, err := ImportInventory(t.Context(), h.store, dir, Claude)
	if err == nil || imported || !strings.Contains(err.Error(), "sample-b: oauth_token: missing") {
		t.Fatalf("import = %v, %v", imported, err)
	}
	if rows, initialized, err := h.store.Accounts(t.Context()); err != nil || initialized || len(rows) != 0 {
		t.Fatalf("table after a refused import: %d rows, initialized %v, %v", len(rows), initialized, err)
	}
	if _, err := LoadAccountsFrom(t.Context(), StoreSource(h.store, dir), Claude, storeResolve, DefaultAccountConcurrency); err == nil || !strings.Contains(err.Error(), "never imported") {
		t.Fatalf("a table that was never imported loaded: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample-b.yaml"), []byte(inventoryYAML("sample-b", "active", "gateway", "op://example-vault/token-sample-b/credential", "8081")), 0o600); err != nil {
		t.Fatal(err)
	}
	importFixture(t, h, dir)
}

// TestImportRefusesAnAccountThatCannotBeStored checks that a row the table refuses
// leaves no row and no marker behind: the import is one transaction.
func TestImportRefusesAnAccountThatCannotBeStored(t *testing.T) {
	h := newHarness(t)
	rows := []session.AccountRow{
		{Alias: "sample-a", Email: "a@example.test", Access: "gateway", Status: "active"},
		{Alias: "sample-b", Email: "b@example.test", Access: "gateway", Status: "not-a-status"},
	}
	if imported, err := h.store.ImportAccounts(t.Context(), rows); err == nil || imported {
		t.Fatalf("import = %v, %v", imported, err)
	}
	if got, initialized, err := h.store.Accounts(t.Context()); err != nil || initialized || len(got) != 0 {
		t.Fatalf("after a refused import: %d rows, initialized %v, %v", len(got), initialized, err)
	}
	if imported, err := h.store.ImportAccounts(t.Context(), rows[:1]); err != nil || !imported {
		t.Fatalf("import without the bad row = %v, %v", imported, err)
	}
}

// TestReloadReadsTheTable checks that a reload reads the accounts table and not the
// directory: with nothing changed in the table a reload reports no difference, though
// the directory has changed. Control arm: the same reload over the directory reports it.
func TestReloadReadsTheTable(t *testing.T) {
	h := newHarness(t)
	dir := storeFixture(t)
	importFixture(t, h, dir)
	src := StoreSource(h.store, dir)
	set, err := LoadAccountsFrom(t.Context(), src, Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	h.accounts.set.Store(set)
	if err := os.Remove(filepath.Join(dir, "sample-b.yaml")); err != nil {
		t.Fatal(err)
	}
	r, err := h.accounts.ReloadFrom(t.Context(), src, storeResolve, nil)
	if err != nil || r.Added != nil || r.Removed != nil || r.Changed != nil {
		t.Fatalf("reload from the table: %+v, %v", r, err)
	}
	r, err = h.accounts.ReloadFrom(t.Context(), DirSource(dir), storeResolve, nil)
	if err != nil || !slices.Equal(r.Removed, []string{"sample-b"}) {
		t.Fatalf("control arm, reload from the directory: %+v, %v", r, err)
	}
}

// TestExportInventory checks that the exported directory serves the same accounts as
// the one imported and imports to the same rows, and that the files hold references and
// no credential value. A tampered export is told apart.
func TestExportInventory(t *testing.T) {
	h := newHarness(t)
	dir := storeFixture(t)
	importFixture(t, h, dir)
	rows, _, err := h.store.Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "export")
	if err := ExportInventory(out, rows); err != nil {
		t.Fatal(err)
	}
	original, err := LoadAccounts(t.Context(), dir, Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := LoadAccounts(t.Context(), out, Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	sameServed(t, original, exported)
	// The files of the accounts the gateway does not serve are exported too.
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 6 {
		t.Fatalf("export holds %d files, %v", len(entries), err)
	}
	// Importing the export gives the rows again.
	again := newHarness(t)
	importFixture(t, again, out)
	got, _, err := again.store.Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !sameRows(rows, got) {
		t.Fatalf("rows differ after export and import:\n%+v\n%+v", rows, got)
	}
	// No credential value is in an exported file.
	entries, _ := os.ReadDir(out)
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(out, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range storeValues {
			if strings.Contains(string(data), value) {
				t.Fatalf("%s holds a credential value", entry.Name())
			}
		}
	}
	// Control: a changed exit IP in one file is a difference sameRows reports.
	path := filepath.Join(out, "sample-a.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "192.0.2.1", "192.0.2.2", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	tampered := newHarness(t)
	importFixture(t, tampered, out)
	got, _, err = tampered.store.Accounts(t.Context())
	if err != nil || sameRows(rows, got) {
		t.Fatalf("a tampered export read as the same rows: %v", err)
	}
}

// sameRows reports whether a and b hold the same accounts, ignoring the times.
func sameRows(a, b []session.AccountRow) bool {
	clean := func(rows []session.AccountRow) []session.AccountRow {
		rows = slices.Clone(rows)
		for i := range rows {
			rows[i].CreatedAt, rows[i].UpdatedAt = time.Time{}, time.Time{}
		}
		return rows
	}
	return slices.Equal(clean(a), clean(b))
}

// TestStoreHoldsNoCredentialValue checks that nothing the gateway resolves reaches the
// store or its backup snapshot: after the accounts are imported and loaded with a
// resolver whose values are distinctive, the snapshot has none of them. Control arm:
// the snapshot does hold a reference, so the search reads what it should.
func TestStoreHoldsNoCredentialValue(t *testing.T) {
	h := newHarness(t)
	dir := storeFixture(t)
	importFixture(t, h, dir)
	if _, err := LoadAccountsFrom(t.Context(), StoreSource(h.store, dir), Claude, storeResolve, DefaultAccountConcurrency); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := h.store.VacuumInto(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "op://example-vault/token-sample-a/credential") {
		t.Fatal("control arm: the snapshot holds no token reference")
	}
	for reference, value := range storeValues {
		if strings.Contains(string(data), value) {
			t.Fatalf("the snapshot holds the value behind %s", reference)
		}
	}
}

// TestDashboardShowsAccountConfig checks that /dashboard.json gives each served account
// its row of the accounts table and lists the others apart, with references as strings
// and no credential value. A provider without a table has no config block.
func TestDashboardShowsAccountConfig(t *testing.T) {
	echo := newEchoService(t, "192.0.2.1")
	h, e := newEgressHarness(t, echo, map[string]string{})
	dir := storeFixture(t)
	importFixture(t, h, dir)
	set, err := LoadAccountsFrom(t.Context(), StoreSource(h.store, dir), Claude, storeResolve, DefaultAccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	h.accounts.set.Store(set)
	reader, err := session.OpenReadOnly(h.path, session.Config{Now: func() time.Time { return h.clock }}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	d := NewDashboard(e, DashboardConfig{Provider: "claude", Reader: reader, Store: h.store, DBPath: h.path, Started: h.clock})
	recorder := httptest.NewRecorder()
	d.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dashboard.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	body := recorder.Body.String()
	var view struct {
		Accounts []struct {
			Alias  string
			Config *accountConfig
		}
		Unserved []accountConfig `json:"unserved_accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var served, unserved []string
	for _, a := range view.Accounts {
		served = append(served, a.Alias)
		if a.Config == nil || a.Config.Alias != a.Alias || a.Config.Rev != 1 {
			t.Fatalf("%s config = %+v", a.Alias, a.Config)
		}
	}
	for _, c := range view.Unserved {
		unserved = append(unserved, c.Alias)
	}
	slices.Sort(served)
	if !slices.Equal(served, []string{"sample-a", "sample-b", "sample-f"}) || !slices.Equal(unserved, []string{"sample-c", "sample-d", "sample-e"}) {
		t.Fatalf("served %v, unserved %v", served, unserved)
	}
	for _, a := range view.Accounts {
		if a.Alias == "sample-a" && (a.Config.TokenRef != "op://example-vault/token-sample-a/credential" || a.Config.Proxy.PasswordRef != "op://example-proxies/proxy-account/password" || a.Config.DeviceID != syntheticDeviceID || a.Config.Proxy.Port != 8080) {
			t.Fatalf("sample-a config = %+v", a.Config)
		}
	}
	for _, value := range storeValues {
		if strings.Contains(body, value) {
			t.Fatal("the dashboard answer holds a credential value")
		}
	}

	// A provider whose accounts are not in the table has no config block and no unserved list.
	other, e2 := newEgressHarness(t, echo, map[string]string{"sample-a": "192.0.2.1"})
	reader2, err := session.OpenReadOnly(other.path, session.Config{}, DefaultLimits.MaxTunnel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader2.Close() })
	d2 := NewDashboard(e2, DashboardConfig{Provider: "codex", Reader: reader2, Store: other.store, DBPath: other.path, Started: other.clock})
	recorder = httptest.NewRecorder()
	d2.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dashboard.json", nil))
	if strings.Contains(recorder.Body.String(), `"config"`) || !strings.Contains(recorder.Body.String(), `"unserved_accounts":[]`) {
		t.Fatalf("answer without a table: %s", recorder.Body.String())
	}
}
