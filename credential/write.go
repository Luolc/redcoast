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
	// ErrUnexpectedFields: an item the write created has fields besides the token's and
	// the expiry date's; the item is left as it is. ExtraFieldsError names them.
	ErrUnexpectedFields = errors.New("the created item has other fields")
	// ErrRateLimited: the secret store refused the read for being asked too often. It is
	// also an ErrWriteFailed. A store reports it by returning an error that is
	// ErrRateLimited.
	ErrRateLimited error = rateLimited{}
	// ErrExpiresMissing: the item has no expires field, or it is empty.
	ErrExpiresMissing = errors.New("the item has no expiry date")
	// ErrExpiresUnreadable: the expires field holds something that is not a date.
	ErrExpiresUnreadable = errors.New("the expiry date is not readable")
)

type rateLimited struct{}

func (rateLimited) Error() string { return "the secret store is rate limited" }

// Is makes a rate limit also an ErrWriteFailed, which is how callers have always met it.
func (rateLimited) Is(target error) bool { return target == ErrWriteFailed }

// failed is the package's error for a store's failure err: ErrRateLimited when the store
// said it was asked too often, else ErrWriteFailed. Nothing the store said is kept.
func failed(err error) error {
	if errors.Is(err, ErrRateLimited) {
		return ErrRateLimited
	}
	return ErrWriteFailed
}

// ExtraFieldsError is ErrUnexpectedFields with the names (titles, else IDs) of the
// fields that should not be there; a name is never a value.
type ExtraFieldsError struct{ Names []string }

func (e ExtraFieldsError) Error() string { return ErrUnexpectedFields.Error() }

// Is makes errors.Is(err, ErrUnexpectedFields) hold.
func (e ExtraFieldsError) Is(target error) bool { return target == ErrUnexpectedFields }

// ExpiresField is the title, ignoring case, of the expiry field that an API Credential
// item carries; Write sets it together with the token and Expires reads it. The value is
// a date written as YYYY-MM-DD.
const ExpiresField = "expires"

// expiresLayout is the layout Write gives the date; Expires also reads RFC 3339.
const expiresLayout = "2006-01-02"

// Vault is a vault the secret store lets the credential see.
type Vault struct{ ID, Title string }

// ItemInfo is an item of a vault as a listing gives it; Archived items are listed too.
type ItemInfo struct {
	ID, Title string
	Archived  bool
}

// Field is a field of an item, named by its ID and its title.
type Field struct{ ID, Title string }

// Item is an item with the names of its fields, and the values only of those fields that
// are named ExpiresField, in the order of Fields; no other value is read.
type Item struct {
	ID, Title string
	Fields    []Field
	Expires   []string
}

// Store is the part of a secret store that Write uses. It is the secret store's write
// path, so an implementation touches exactly what a method names: SetFields changes the
// value of one existing field of one item and the date in the item's expiry field, when
// it has one, and nothing else, and Create makes one item with one concealed field and an
// expiry field and nothing else. Neither deletes anything.
type Store interface {
	// Vaults lists the vaults the credential can see.
	Vaults(ctx context.Context) ([]Vault, error)
	// Items lists the items of a vault, archived ones included.
	Items(ctx context.Context, vaultID string) ([]ItemInfo, error)
	// Item returns an item's field names and the values of its expiry fields.
	Item(ctx context.Context, vaultID, itemID string) (Item, error)
	// SetFields sets the value of the one field of the item that has this ID and title,
	// and the date in its one expiry field; an item without an expiry field gets no
	// field added. It fails when the first is not exactly one, or more than one expiry
	// field is there.
	SetFields(ctx context.Context, vaultID, itemID string, field Field, value, expires string) error
	// Create makes an item with this title in the vault, holding one concealed field
	// with this title and value and an expiry field with this date.
	Create(ctx context.Context, vaultID, title, fieldTitle, value, expires string) error
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
	if len(parts) != 3 || strings.EqualFold(parts[2], ExpiresField) {
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
		return target{}, failed(err)
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
		return target{}, failed(err)
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

// Write puts value at the op:// reference and sets the item's expiry date to expires.
// When the item exists it sets the one field the reference names and the item's own
// expiry field, if it has one; it adds no field. When the item does not exist and
// mayCreate is set, it creates the item, titled as the reference names it, with that one
// field and an expiry field, and then reads the item's fields back: one with other
// fields than those is left as it is and reported as ExtraFieldsError, with created set.
// Otherwise it writes nothing: it refuses,
// before any write, a reference with a section part or one that names the expiry field,
// a missing or non-unique vault, an archived item or more than one item with the name,
// an existing item without the field or with it twice or with two expiry fields, and a
// missing item when mayCreate is not set. It reports whether it created the item. The
// errors are the package's own and never hold the value.
func Write(ctx context.Context, store Store, reference, value string, expires time.Time, mayCreate bool) (created bool, err error) {
	created, _, err = WriteDated(ctx, store, reference, value, expires, mayCreate)
	return created, err
}

// WriteDated is Write that also reports whether the write set the expiry date: a created
// item always gets it, an existing item only when it has an expiry field. On an error,
// dated is false.
func WriteDated(ctx context.Context, store Store, reference, value string, expires time.Time, mayCreate bool) (created, dated bool, err error) {
	created, dated, err = write(ctx, store, reference, value, expires, mayCreate)
	if err != nil {
		dated = false
	}
	return created, dated, err
}

func write(ctx context.Context, store Store, reference, value string, expires time.Time, mayCreate bool) (created, dated bool, err error) {
	found, err := locate(ctx, store, reference)
	if err != nil {
		return false, false, err
	}
	date := expires.UTC().Format(expiresLayout)
	if found.absent {
		if !mayCreate {
			return false, false, ErrTargetMissing
		}
		if err := store.Create(ctx, found.vault.ID, found.itemName, found.fieldName, value, date); err != nil {
			return false, false, ErrWriteFailed
		}
		return true, true, checkCreated(ctx, store, reference)
	}
	item, err := store.Item(ctx, found.vault.ID, found.item.ID)
	if err != nil {
		return false, false, ErrWriteFailed
	}
	f, err := field(item, found.fieldName)
	if err != nil {
		return false, false, err
	}
	if len(item.Expires) > 1 {
		return false, false, ErrTargetAmbiguous
	}
	if err := store.SetFields(ctx, found.vault.ID, found.item.ID, f, value, date); err != nil {
		return false, false, ErrWriteFailed
	}
	return false, len(item.Expires) == 1, nil
}

// isExpiryField reports whether f is an expiry field: its title is ExpiresField, ignoring
// case. A field is not one by its ID.
func isExpiryField(f Field) bool { return strings.EqualFold(f.Title, ExpiresField) }

// checkCreated reads the item that Write created and reports ExtraFieldsError when it
// has a field other than the token's one and one expiry field; the item is not touched.
func checkCreated(ctx context.Context, store Store, reference string) error {
	found, err := locate(ctx, store, reference)
	if err != nil || found.absent {
		return ErrWriteFailed
	}
	item, err := store.Item(ctx, found.vault.ID, found.item.ID)
	if err != nil {
		return ErrWriteFailed
	}
	var extra []string
	tokens, expiries := 0, 0
	for _, f := range item.Fields {
		switch {
		case named(found.fieldName, f.ID, f.Title) && tokens == 0:
			tokens++
		case isExpiryField(f) && expiries == 0:
			expiries++
		default:
			name := f.Title
			if name == "" {
				name = f.ID
			}
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		return ExtraFieldsError{Names: extra}
	}
	return nil
}

// Expires returns the expiry date of the item the reference names, for a reference whose
// vault and item are unique and whose item has the field the reference names. It reads
// no token. It fails with ErrExpiresMissing when the item has no expiry field or an
// empty one, and with ErrExpiresUnreadable when the field holds no date.
func Expires(ctx context.Context, store Store, reference string) (time.Time, error) {
	found, err := locate(ctx, store, reference)
	if err != nil {
		return time.Time{}, err
	}
	if found.absent {
		return time.Time{}, ErrTargetMissing
	}
	item, err := store.Item(ctx, found.vault.ID, found.item.ID)
	if err != nil {
		return time.Time{}, failed(err)
	}
	if _, err := field(item, found.fieldName); err != nil {
		return time.Time{}, err
	}
	switch len(item.Expires) {
	case 0:
		return time.Time{}, ErrExpiresMissing
	case 1:
	default:
		return time.Time{}, ErrTargetAmbiguous
	}
	text := strings.TrimSpace(item.Expires[0])
	if text == "" {
		return time.Time{}, ErrExpiresMissing
	}
	for _, layout := range []string{expiresLayout, time.RFC3339} {
		if at, err := time.Parse(layout, text); err == nil {
			return at.UTC(), nil
		}
	}
	return time.Time{}, ErrExpiresUnreadable
}

// CachedLists returns a store that asks store for the vault list once and for each
// vault's item list once, and answers later asks from memory; everything else goes to
// store. It is for one pass of reads that many references share, such as the expiry date
// of every account's token, and is dropped after the pass: a list that changes during the
// pass is read as it was at its first ask, which costs at most a stale date for the pass,
// and a failed ask is not kept. It is for one goroutine and must not be used for writes,
// which need the lists as they are.
func CachedLists(store Store) Store {
	return &cachedLists{Store: store, items: map[string][]ItemInfo{}}
}

type cachedLists struct {
	Store
	vaults    []Vault
	haveVault bool
	items     map[string][]ItemInfo
}

func (c *cachedLists) Vaults(ctx context.Context) ([]Vault, error) {
	if c.haveVault {
		return c.vaults, nil
	}
	vaults, err := c.Store.Vaults(ctx)
	if err != nil {
		return nil, err
	}
	c.vaults, c.haveVault = vaults, true
	return vaults, nil
}

func (c *cachedLists) Items(ctx context.Context, vaultID string) ([]ItemInfo, error) {
	if items, ok := c.items[vaultID]; ok {
		return items, nil
	}
	items, err := c.Store.Items(ctx, vaultID)
	if err != nil {
		return nil, err
	}
	c.items[vaultID] = items
	return items, nil
}
