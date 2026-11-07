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
}

type fakeItem struct {
	info   ItemInfo
	fields []Field
	update time.Time
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
			return Item{ID: it.info.ID, Title: it.info.Title, Fields: it.fields, UpdatedAt: it.update}, nil
		}
	}
	return Item{}, errors.New("no such item")
}

func (f *fakeStore) SetField(_ context.Context, _, _ string, _ Field, value string) error {
	if f.fail["SetField"] {
		return errors.New("store said sk-ant-oat01-LEAK")
	}
	f.sets++
	f.lastValue = value
	return nil
}

func (f *fakeStore) Create(_ context.Context, vaultID, title, fieldTitle, value string) error {
	if f.fail["Create"] {
		return errors.New("store said sk-ant-oat01-LEAK")
	}
	f.creates++
	f.lastValue = value
	f.items[vaultID] = append(f.items[vaultID], fakeItem{info: ItemInfo{ID: "new-" + title, Title: title}, fields: []Field{{ID: fieldTitle, Title: fieldTitle}}})
	return nil
}

// newFake has one vault, "example-vault", holding an item "token-sample-a" with the
// field "credential".
func newFake() *fakeStore {
	return &fakeStore{
		vaults: []Vault{{ID: "v1", Title: "example-vault"}, {ID: "v2", Title: "other-vault"}},
		items: map[string][]fakeItem{"v1": {
			{info: ItemInfo{ID: "i1", Title: "token-sample-a"}, fields: []Field{{ID: "credential", Title: "credential"}, {ID: "f2", Title: "notes"}}, update: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		}},
		fail: map[string]bool{},
	}
}

const writeValue = "sk-ant-oat01-synthetic-new-value"

// TestWriteSetsOrCreates: the four writes that are allowed.
func TestWriteSetsOrCreates(t *testing.T) {
	t.Run("existing item gets its one field set", func(t *testing.T) {
		f := newFake()
		created, err := Write(t.Context(), f, "op://example-vault/token-sample-a/credential", writeValue, true)
		if err != nil || created || f.sets != 1 || f.creates != 0 || f.lastValue != writeValue {
			t.Fatalf("created=%v err=%v sets=%d creates=%d", created, err, f.sets, f.creates)
		}
	})
	t.Run("a name matches by ID or ignoring case", func(t *testing.T) {
		f := newFake()
		if _, err := Write(t.Context(), f, "op://v1/i1/Credential", writeValue, false); err != nil || f.sets != 1 {
			t.Fatalf("err=%v sets=%d", err, f.sets)
		}
	})
	t.Run("missing item is created when that is allowed", func(t *testing.T) {
		f := newFake()
		created, err := Write(t.Context(), f, "op://example-vault/token-sample-b/credential", writeValue, true)
		if err != nil || !created || f.creates != 1 || f.sets != 0 || f.lastValue != writeValue {
			t.Fatalf("created=%v err=%v sets=%d creates=%d", created, err, f.sets, f.creates)
		}
	})
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
		{"item has the field twice", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) {
			f.items["v1"][0].fields = append(f.items["v1"][0].fields, Field{ID: "f9", Title: "Credential"})
		}, ErrTargetAmbiguous},
		{"vault listing fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["Vaults"] = true }, ErrWriteFailed},
		{"item listing fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["Items"] = true }, ErrWriteFailed},
		{"item read fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["Item"] = true }, ErrWriteFailed},
		{"set fails", "op://example-vault/token-sample-a/credential", true, func(f *fakeStore) { f.fail["SetField"] = true }, ErrWriteFailed},
		{"create fails", "op://example-vault/token-sample-b/credential", true, func(f *fakeStore) { f.fail["Create"] = true }, ErrWriteFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFake()
			if test.arrange != nil {
				test.arrange(f)
			}
			before := len(f.items["v1"])
			created, err := Write(t.Context(), f, test.reference, writeValue, test.mayCreate)
			if !errors.Is(err, test.want) || created || f.sets != 0 || f.creates != 0 || len(f.items["v1"]) != before {
				t.Fatalf("err=%v want %v created=%v sets=%d creates=%d", err, test.want, created, f.sets, f.creates)
			}
			if strings.Contains(err.Error(), "LEAK") || strings.Contains(err.Error(), writeValue) {
				t.Fatalf("the error carries more than its fixed text: %v", err)
			}
		})
	}
}

// TestUpdated dates an item without reading a value, and fails like Write does.
func TestUpdated(t *testing.T) {
	f := newFake()
	at, err := Updated(t.Context(), f, "op://example-vault/token-sample-a/credential")
	if err != nil || !at.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("at=%v err=%v", at, err)
	}
	if _, err := Updated(t.Context(), f, "op://example-vault/token-sample-b/credential"); !errors.Is(err, ErrTargetMissing) {
		t.Fatalf("missing item: %v", err)
	}
	if _, err := Updated(t.Context(), f, "op://example-vault/token-sample-a/password"); !errors.Is(err, ErrTargetMissing) {
		t.Fatalf("missing field: %v", err)
	}
}
