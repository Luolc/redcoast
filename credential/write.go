package credential

import (
	"context"
	"errors"
	"strings"
	"time"
)

// The ways Write refuses or fails. Each is fixed text: nothing a store said, and never
// the value being written.
var (
	// ErrTargetRefused: the reference is not an op:// reference of vault, item and field.
	ErrTargetRefused = errors.New("the reference is not a vault, an item and a field")
	// ErrTargetMissing: the vault, the item (when it may not be created) or the field is not there.
	ErrTargetMissing = errors.New("the vault, the item or the field does not exist")
	// ErrTargetAmbiguous: a name matches more than one vault, item or field, or an
	// archived item has the title.
	ErrTargetAmbiguous = errors.New("a name matches more than one vault, item or field, or an archived item")
	// ErrWriteFailed: the store refused the read or the write, or could not be reached.
	ErrWriteFailed = errors.New("the secret store failed")
)

// Vault is a vault the secret store lets the credential see.
type Vault struct{ ID, Title string }

// ItemInfo is an item of a vault as a listing gives it; Archived items are listed too.
type ItemInfo struct {
	ID, Title string
	Archived  bool
}

// Field is a field of an item, named by its ID and its title.
type Field struct{ ID, Title string }

// Item is an item with the names of its fields, never their values.
type Item struct {
	ID, Title string
	Fields    []Field
	UpdatedAt time.Time
}

// Store is the part of a secret store that Write uses. It is the secret store's write
// path, so an implementation touches exactly what a method names: SetField changes the
// value of one existing field of one item and nothing else, and Create makes one item
// with one concealed field and nothing else. Neither deletes anything.
type Store interface {
	// Vaults lists the vaults the credential can see.
	Vaults(ctx context.Context) ([]Vault, error)
	// Items lists the items of a vault, archived ones included.
	Items(ctx context.Context, vaultID string) ([]ItemInfo, error)
	// Item returns an item's field names and its update time.
	Item(ctx context.Context, vaultID, itemID string) (Item, error)
	// SetField sets the value of the one field of the item that has this ID and title;
	// it fails when there is not exactly one.
	SetField(ctx context.Context, vaultID, itemID string, field Field, value string) error
	// Create makes an item with this title in the vault, holding one concealed field
	// with this title and value.
	Create(ctx context.Context, vaultID, title, fieldTitle, value string) error
}

// target is where a reference points, found in the store.
type target struct {
	vault Vault
	// item is the one active item with the name; zero when none and absent is set.
	item   ItemInfo
	absent bool
	// names from the reference
	itemName, fieldName string
}

// parseWriteReference splits op://vault/item/field; a reference with a section part, or
// anything else, is refused.
func parseWriteReference(reference string) (vault, item, field string, err error) {
	rest, ok := strings.CutPrefix(reference, "op://")
	if !ok || Check(reference) != nil {
		return "", "", "", ErrTargetRefused
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", "", ErrTargetRefused
	}
	return parts[0], parts[1], parts[2], nil
}

// named reports whether a name from a reference picks the thing with this ID and title:
// it is the ID, or the title ignoring case, which is how the store matches too, so a
// name that the store could read as either is counted for both.
func named(name, id, title string) bool {
	return name == id || strings.EqualFold(name, title)
}

// locate finds the vault and the active item a reference names. It fails when the vault
// is missing or not unique, when an archived item or more than one item has the name;
// an item that is not there is reported through absent, not an error.
func locate(ctx context.Context, store Store, reference string) (target, error) {
	vaultName, itemName, fieldName, err := parseWriteReference(reference)
	if err != nil {
		return target{}, err
	}
	vaults, err := store.Vaults(ctx)
	if err != nil {
		return target{}, ErrWriteFailed
	}
	var matched []Vault
	for _, v := range vaults {
		if named(vaultName, v.ID, v.Title) {
			matched = append(matched, v)
		}
	}
	switch len(matched) {
	case 0:
		return target{}, ErrTargetMissing
	case 1:
	default:
		return target{}, ErrTargetAmbiguous
	}
	items, err := store.Items(ctx, matched[0].ID)
	if err != nil {
		return target{}, ErrWriteFailed
	}
	var active []ItemInfo
	for _, item := range items {
		if !named(itemName, item.ID, item.Title) {
			continue
		}
		if item.Archived {
			return target{}, ErrTargetAmbiguous
		}
		active = append(active, item)
	}
	found := target{vault: matched[0], itemName: itemName, fieldName: fieldName}
	switch len(active) {
	case 0:
		found.absent = true
	case 1:
		found.item = active[0]
	default:
		return target{}, ErrTargetAmbiguous
	}
	return found, nil
}

// field finds the one field of item that fieldName picks.
func field(item Item, fieldName string) (Field, error) {
	var matched []Field
	for _, f := range item.Fields {
		if named(fieldName, f.ID, f.Title) {
			matched = append(matched, f)
		}
	}
	switch len(matched) {
	case 0:
		return Field{}, ErrTargetMissing
	case 1:
		return matched[0], nil
	}
	return Field{}, ErrTargetAmbiguous
}

// Write puts value at the op:// reference. When the item exists it sets the one field
// the reference names; it adds no field. When the item does not exist and mayCreate is
// set, it creates the item, titled as the reference names it, with that one field.
// Otherwise it writes nothing: it refuses, before any write, a reference with a section
// part, a missing or non-unique vault, an archived item or more than one item with the
// name, an existing item without the field or with it twice, and a missing item when
// mayCreate is not set. It reports whether it created the item. The errors are the
// package's four and never hold the value.
func Write(ctx context.Context, store Store, reference, value string, mayCreate bool) (created bool, err error) {
	found, err := locate(ctx, store, reference)
	if err != nil {
		return false, err
	}
	if found.absent {
		if !mayCreate {
			return false, ErrTargetMissing
		}
		if err := store.Create(ctx, found.vault.ID, found.itemName, found.fieldName, value); err != nil {
			return false, ErrWriteFailed
		}
		return true, nil
	}
	item, err := store.Item(ctx, found.vault.ID, found.item.ID)
	if err != nil {
		return false, ErrWriteFailed
	}
	f, err := field(item, found.fieldName)
	if err != nil {
		return false, err
	}
	if err := store.SetField(ctx, found.vault.ID, found.item.ID, f, value); err != nil {
		return false, ErrWriteFailed
	}
	return false, nil
}

// Updated returns when the item the reference names was last changed, for a reference
// whose vault and item are unique and whose item has the field. It reads no value.
func Updated(ctx context.Context, store Store, reference string) (time.Time, error) {
	found, err := locate(ctx, store, reference)
	if err != nil {
		return time.Time{}, err
	}
	if found.absent {
		return time.Time{}, ErrTargetMissing
	}
	item, err := store.Item(ctx, found.vault.ID, found.item.ID)
	if err != nil {
		return time.Time{}, ErrWriteFailed
	}
	if _, err := field(item, found.fieldName); err != nil {
		return time.Time{}, err
	}
	return item.UpdatedAt, nil
}
