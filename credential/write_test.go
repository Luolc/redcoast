package credential

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStore is a secret store in memory that counts the writes it was asked for.
type fakeStore struct {
	vaults []Vault
	items  map[string][]fakeItem // by vault ID
	fail   map[string]bool       // method names that fail

	sets, creates int
	lastValue     string
	lastExpires   string
	// createExtra are fields a created item gets besides those asked for.
	createExtra []Field
}

type fakeItem struct {
	info    ItemInfo
	fields  []Field
	expires []string
}

func (f *fakeStore) Vaults(context.Context) ([]Vault, error) {
	if f.fail["Vaults"] {
		return nil, errors.New("store said sk-ant-oat01-LEAK")
	}
	return f.vaults, nil
}

func (f *fakeStore) Items(_ context.Context, vaultID string) ([]ItemInfo, error) {
	if f.fail["Items"] {
		return nil, errors.New("store said sk-ant-oat01-LEAK")
	}
	var out []ItemInfo
	for _, it := range f.items[vaultID] {
		out = append(out, it.info)
	}
	return out, nil
}

func (f *fakeStore) Item(_ context.Context, vaultID, itemID string) (Item, error) {
	if f.fail["Item"] {
		return Item{}, errors.New("store said sk-ant-oat01-LEAK")
	}
	for _, it := range f.items[vaultID] {
		if it.info.ID == itemID {
			return Item{ID: it.info.ID, Title: it.info.Title, Fields: it.fields, Expires: it.expires}, nil
		}
	}
	return Item{}, errors.New("no such item")
}

func (f *fakeStore) SetFields(_ context.Context, _, _ string, _ Field, value, expires string) error {
	if f.fail["SetFields"] {
		return errors.New("store said sk-ant-oat01-LEAK")
	}
	f.sets++
	f.lastValue, f.lastExpires = value, expires
	return nil
}

func (f *fakeStore) Create(_ context.Context, vaultID, title, fieldTitle, value, expires string) error {
	if f.fail["Create"] {
		return errors.New("store said sk-ant-oat01-LEAK")
	}
	f.creates++
	f.lastValue, f.lastExpires = value, expires
	f.items[vaultID] = append(f.items[vaultID], fakeItem{info: ItemInfo{ID: "new-" + title, Title: title}, fields: append([]Field{{ID: fieldTitle, Title: fieldTitle}, {ID: ExpiresField, Title: ExpiresField}}, f.createExtra...), expires: []string{expires}})
	return nil
}

// newFake has one vault, "example-vault", holding an item "token-sample-a" with the
// field "credential".
func newFake() *fakeStore {
	return &fakeStore{
		vaults: []Vault{{ID: "v1", Title: "example-vault"}, {ID: "v2", Title: "other-vault"}},
		items: map[string][]fakeItem{"v1": {
			{info: ItemInfo{ID: "i1", Title: "token-sample-a"}, fields: []Field{{ID: "credential", Title: "credential"}, {ID: "f2", Title: "notes"}, {ID: "expires", Title: "expires"}}, expires: []string{"2027-03-01"}},
		}},
		fail: map[string]bool{},
	}
}

const writeValue = "sk-ant-oat01-synthetic-new-value"

var writeExpires = time.Date(2027, 11, 6, 22, 30, 0, 0, time.FixedZone("west", -8*3600))

// TestWriteSetsOrCreates: the four writes that are allowed.
func TestWriteSetsOrCreates(t *testing.T) {
	t.Run("existing item gets its one field set", func(t *testing.T) {
		f := newFake()
		created, err := Write(t.Context(), f, "op://example-vault/token-sample-a/credential", writeValue, writeExpires, true)
		if err != nil || created || f.sets != 1 || f.creates != 0 || f.lastValue != writeValue || f.lastExpires != "2027-11-07" {
			t.Fatalf("created=%v err=%v sets=%d creates=%d expires=%q", created, err, f.sets, f.creates, f.lastExpires)
		}
	})
	t.Run("a name matches by ID or ignoring case", func(t *testing.T) {
		f := newFake()
		if _, err := Write(t.Context(), f, "op://v1/i1/Credential", writeValue, writeExpires, false); err != nil || f.sets != 1 {
			t.Fatalf("err=%v sets=%d", err, f.sets)
		}
	})
	t.Run("an item without an expiry field is written, and gets the date", func(t *testing.T) {
		f := newFake()
		f.items["v1"][0].fields = f.items["v1"][0].fields[:2]
		f.items["v1"][0].expires = nil
		if _, err := Write(t.Context(), f, "op://example-vault/token-sample-a/credential", writeValue, writeExpires, false); err != nil || f.sets != 1 || f.lastExpires != "2027-11-07" {
			t.Fatalf("err=%v sets=%d expires=%q", err, f.sets, f.lastExpires)
		}
	})
	t.Run("missing item is created when that is allowed", func(t *testing.T) {
		f := newFake()
		created, err := Write(t.Context(), f, "op://example-vault/token-sample-b/credential", writeValue, writeExpires, true)
		if err != nil || !created || f.creates != 1 || f.sets != 0 || f.lastValue != writeValue || f.lastExpires != "2027-11-07" {
			t.Fatalf("created=%v err=%v sets=%d creates=%d", created, err, f.sets, f.creates)
		}
	})
}

// TestWriteChecksTheCreatedItem: after a create the item's fields are read back; a field
// besides the token's and one expiry date, or a second expiry field, is reported with its
// name, the item is left as it is, and the write counts as created.
func TestWriteChecksTheCreatedItem(t *testing.T) {
	const ref = "op://example-vault/token-sample-b/credential"
	for _, test := range []struct {
		name  string
		extra []Field
		want  []string
	}{
		{"none", nil, nil},
		{"another field", []Field{{ID: "valid_from", Title: "valid from"}}, []string{"valid from"}},
		{"a second token field", []Field{{ID: "credential2", Title: "Credential"}}, []string{"Credential"}},
		{"a second expiry field", []Field{{ID: "expires2", Title: "Expires"}}, []string{"Expires"}},
		{"a field without a title", []Field{{ID: "x1"}}, []string{"x1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFake()
			f.createExtra = test.extra
			created, err := Write(t.Context(), f, ref, writeValue, writeExpires, true)
			var extra ExtraFieldsError
			if test.want == nil {
				if err != nil || !created {
					t.Fatalf("created=%v err=%v", created, err)
				}
				return
			}
			if !errors.Is(err, ErrUnexpectedFields) || !errors.As(err, &extra) || !created || strings.Join(extra.Names, ",") != strings.Join(test.want, ",") {
				t.Fatalf("created=%v err=%v", created, err)
			}
			if f.sets != 0 || f.creates != 1 || len(f.items["v1"]) != 2 || strings.Contains(err.Error(), writeValue) {
				t.Fatalf("sets=%d creates=%d items=%d", f.sets, f.creates, len(f.items["v1"]))
			}
		})
	}
}

// TestWriteRefuses: each refusal writes and creates nothing, and says nothing of the
// value or of what the store said.
func TestWriteRefuses(t *testing.T) {
	tests := []struct {
		name      string
		reference string
		mayCreate bool
		arrange   func(f *fakeStore)
		want      error
	}{
		{"section part", "op://example-vault/token-sample-a/section/credential", true, nil, ErrTargetRefused},
		{"not op", "env://TOKEN", true, nil, ErrTargetRefused},
		{"vault missing", "op://nowhere/token-sample-a/credential", true, nil, ErrTargetMissing},
		{"vault twice", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) {
			f.vaults = append(f.vaults, Vault{ID: "v3", Title: "EXAMPLE-VAULT"})
		}, ErrTargetAmbiguous},
		{"item missing and not allowed to create", "op://example-vault/token-sample-b/credential", false, nil, ErrTargetMissing},
		{"archived item has the title", "op://example-vault/token-sample-b/credential", true, func(f *fakeStore) {
			f.items["v1"] = append(f.items["v1"], fakeItem{info: ItemInfo{ID: "i9", Title: "token-sample-b", Archived: true}})
		}, ErrTargetAmbiguous},
		{"archived item next to an active one", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) {
			f.items["v1"] = append(f.items["v1"], fakeItem{info: ItemInfo{ID: "i9", Title: "token-sample-a", Archived: true}})
		}, ErrTargetAmbiguous},
		{"two items have the title", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) {
			f.items["v1"] = append(f.items["v1"], fakeItem{info: ItemInfo{ID: "i8", Title: "Token-Sample-A"}, fields: []Field{{ID: "credential", Title: "credential"}}})
		}, ErrTargetAmbiguous},
		{"item lacks the field", "op://example-vault/token-sample-a/password", true, nil, ErrTargetMissing},
		{"reference names the expiry field", "op://example-vault/token-sample-a/Expires", true, nil, ErrTargetRefused},
		{"item has two expiry fields", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) {
			f.items["v1"][0].expires = []string{"2027-03-01", "2028-03-01"}
		}, ErrTargetAmbiguous},
		{"item has the field twice", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) {
			f.items["v1"][0].fields = append(f.items["v1"][0].fields, Field{ID: "f9", Title: "Credential"})
		}, ErrTargetAmbiguous},
		{"vault listing fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["Vaults"] = true }, ErrWriteFailed},
		{"item listing fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["Items"] = true }, ErrWriteFailed},
		{"item read fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["Item"] = true }, ErrWriteFailed},
		{"set fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["SetFields"] = true }, ErrWriteFailed},
		{"create fails", "op://example-vault/token-sample-b/credential", true, func(f *fakeStore) { f.fail["Create"] = true }, ErrWriteFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFake()
			if test.arrange != nil {
				test.arrange(f)
			}
			before := len(f.items["v1"])
			created, err := Write(t.Context(), f, test.reference, writeValue, writeExpires, test.mayCreate)
			if !errors.Is(err, test.want) || created || f.sets != 0 || f.creates != 0 || len(f.items["v1"]) != before {
				t.Fatalf("err=%v want %v created=%v sets=%d creates=%d", err, test.want, created, f.sets, f.creates)
			}
			if strings.Contains(err.Error(), "LEAK") || strings.Contains(err.Error(), writeValue) {
				t.Fatalf("the error carries more than its fixed text: %v", err)
			}
		})
	}
}

// TestExpires reads the expiry date without a token, and fails like Write does.
func TestExpires(t *testing.T) {
	const ref = "op://example-vault/token-sample-a/credential"
	read := func(arrange func(f *fakeStore)) (time.Time, error) {
		f := newFake()
		if arrange != nil {
			arrange(f)
		}
		return Expires(t.Context(), f, ref)
	}
	want := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	if at, err := read(nil); err != nil || !at.Equal(want) {
		t.Fatalf("date: at=%v err=%v", at, err)
	}
	if at, err := read(func(f *fakeStore) { f.items["v1"][0].expires = []string{"2027-03-01T05:00:00+05:00"} }); err != nil || !at.Equal(want) {
		t.Fatalf("RFC 3339: at=%v err=%v", at, err)
	}
	for _, test := range []struct {
		name    string
		arrange func(f *fakeStore)
		want    error
	}{
		{"no expiry field", func(f *fakeStore) { f.items["v1"][0].expires = nil }, ErrExpiresMissing},
		{"empty expiry field", func(f *fakeStore) { f.items["v1"][0].expires = []string{" "} }, ErrExpiresMissing},
		{"not a date", func(f *fakeStore) { f.items["v1"][0].expires = []string{"next spring"} }, ErrExpiresUnreadable},
		{"two expiry fields", func(f *fakeStore) { f.items["v1"][0].expires = []string{"2027-03-01", "2027-03-02"} }, ErrTargetAmbiguous},
		{"item missing", func(f *fakeStore) { f.items["v1"] = nil }, ErrTargetMissing},
		{"item read fails", func(f *fakeStore) { f.fail["Item"] = true }, ErrWriteFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := read(test.arrange); !errors.Is(err, test.want) {
				t.Fatalf("err=%v want %v", err, test.want)
			}
		})
	}
	f := newFake()
	if _, err := Expires(t.Context(), f, "op://example-vault/token-sample-a/password"); !errors.Is(err, ErrTargetMissing) {
		t.Fatalf("missing field: %v", err)
	}
}
