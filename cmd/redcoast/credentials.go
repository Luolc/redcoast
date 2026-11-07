package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/1password/onepassword-sdk-go"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/credential"
)

// maxCredentialsInput bounds the JSON object the test entry reads from stdin.
const maxCredentialsInput = 64 << 10

// onePasswordResolver returns a resolver over the official SDK with the service account
// token. The token stays in the client; it is in no error.
func onePasswordResolver(ctx context.Context, token string) (claude.Resolver, error) {
	client, err := onepassword.NewClient(ctx, onepassword.WithServiceAccountToken(token), onepassword.WithIntegrationInfo("redcoast", version))
	if err != nil {
		return nil, fmt.Errorf("1Password client: %v", err)
	}
	return clientResolver(client), nil
}

// onePasswordItems is the write path of the official SDK as credential.Store: it lists
// vaults and items, reads an item's field names and its expiry date, sets one field of an
// item and its expiry date and creates one item with one concealed field and an expiry
// date, and calls nothing else of the Items API.
type onePasswordItems struct{ client *onepassword.Client }

// onePasswordSecretStore returns the secret store over the official SDK with the service
// account token. The token stays in the client; it is in no error.
func onePasswordSecretStore(ctx context.Context, token string) (credential.Store, error) {
	client, err := onepassword.NewClient(ctx, onepassword.WithServiceAccountToken(token), onepassword.WithIntegrationInfo("redcoast", version))
	if err != nil {
		return nil, errors.New("1Password client")
	}
	return onePasswordItems{client}, nil
}

func (s onePasswordItems) Vaults(ctx context.Context) ([]credential.Vault, error) {
	list, err := s.client.Vaults().List(ctx)
	runtime.KeepAlive(s.client)
	if err != nil {
		return nil, err
	}
	vaults := make([]credential.Vault, len(list))
	for i, v := range list {
		vaults[i] = credential.Vault{ID: v.ID, Title: v.Title}
	}
	return vaults, nil
}

func (s onePasswordItems) Items(ctx context.Context, vaultID string) ([]credential.ItemInfo, error) {
	both := onepassword.NewItemListFilterTypeVariantByState(&onepassword.ItemListFilterByStateInner{Active: true, Archived: true})
	list, err := s.client.Items().List(ctx, vaultID, both)
	runtime.KeepAlive(s.client)
	if err != nil {
		return nil, err
	}
	items := make([]credential.ItemInfo, len(list))
	for i, it := range list {
		items[i] = credential.ItemInfo{ID: it.ID, Title: it.Title, Archived: it.State == onepassword.ItemStateArchived}
	}
	return items, nil
}

func (s onePasswordItems) Item(ctx context.Context, vaultID, itemID string) (credential.Item, error) {
	item, err := s.client.Items().Get(ctx, vaultID, itemID)
	runtime.KeepAlive(s.client)
	if err != nil {
		return credential.Item{}, err
	}
	out := credential.Item{ID: item.ID, Title: item.Title}
	for _, f := range item.Fields {
		out.Fields = append(out.Fields, credential.Field{ID: f.ID, Title: f.Title})
		if isExpires(f) {
			out.Expires = append(out.Expires, f.Value)
		}
	}
	return out, nil
}

// isExpires reports whether f is an expiry field, by ID or title.
func isExpires(f onepassword.ItemField) bool {
	return f.ID == credential.ExpiresField || strings.EqualFold(f.Title, credential.ExpiresField)
}

// SetFields reads the item again and sets the value of its one field with this ID and
// title and the date of its one expiry field, which it adds as a date field when there is
// none, so a field that changed since the caller looked is not written blindly; the rest
// of the item is put back as it was read.
func (s onePasswordItems) SetFields(ctx context.Context, vaultID, itemID string, field credential.Field, value, expires string) error {
	item, err := s.client.Items().Get(ctx, vaultID, itemID)
	runtime.KeepAlive(s.client)
	if err != nil {
		return err
	}
	at, due := -1, -1
	for i, f := range item.Fields {
		if f.ID == field.ID && f.Title == field.Title {
			if at >= 0 {
				return errors.New("field not unique")
			}
			at = i
		}
		if isExpires(f) {
			if due >= 0 {
				return errors.New("expiry field not unique")
			}
			due = i
		}
	}
	if at < 0 {
		return errors.New("field not found")
	}
	item.Fields[at].Value = value
	if due >= 0 {
		item.Fields[due].Value = expires
	} else {
		item.Fields = append(item.Fields, expiryField(expires))
	}
	_, err = s.client.Items().Put(ctx, item)
	runtime.KeepAlive(s.client)
	return err
}

// expiryField is the field that holds an expiry date.
func expiryField(date string) onepassword.ItemField {
	return onepassword.ItemField{ID: credential.ExpiresField, Title: credential.ExpiresField, FieldType: onepassword.ItemFieldTypeDate, Value: date}
}

func (s onePasswordItems) Create(ctx context.Context, vaultID, title, fieldTitle, value, expires string) error {
	_, err := s.client.Items().Create(ctx, onepassword.ItemCreateParams{
		Category: onepassword.ItemCategoryAPICredentials, VaultID: vaultID, Title: title,
		Fields: []onepassword.ItemField{{ID: fieldTitle, Title: fieldTitle, FieldType: onepassword.ItemFieldTypeConcealed, Value: value}, expiryField(expires)},
	})
	runtime.KeepAlive(s.client)
	return err
}

// clientResolver returns a resolver over client that keeps client reachable for as long
// as the resolver is. The SDK registers a finalizer on the Client that releases the
// underlying client ID; the method value client.Secrets().Resolve holds only the inner
// client, so after a collection the ID could be released while the resolver is still
// in use and every later resolution would fail.
func clientResolver(client *onepassword.Client) claude.Resolver {
	return func(ctx context.Context, reference string) (string, error) {
		value, err := client.Secrets().Resolve(ctx, reference)
		runtime.KeepAlive(client)
		return value, err
	}
}

// stdinResolver reads one JSON object of references to values, at most
// maxCredentialsInput bytes, from reader and returns a resolver that knows only those
// references. It is the test entry of synthetic experiments, which must not read a
// vault; the production service never reads stdin. Every failure returns the same
// error so input never reaches the log.
func stdinResolver(ctx context.Context, reader io.ReadCloser) (claude.Resolver, error) {
	// Closing the private pipe interrupts a stalled producer without a read goroutine.
	stop := context.AfterFunc(ctx, func() {
		if err := reader.Close(); err != nil {
			// Cancellation already makes the input unusable.
			return
		}
	})
	defer stop()
	invalid := errors.New("invalid credentials input")
	data, err := io.ReadAll(io.LimitReader(reader, maxCredentialsInput+1))
	if err != nil || ctx.Err() != nil || len(data) > maxCredentialsInput {
		return nil, invalid
	}
	var values map[string]string
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&values); err != nil {
		return nil, invalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, invalid
	}
	if len(values) == 0 {
		return nil, invalid
	}
	return func(_ context.Context, reference string) (string, error) {
		value, known := values[reference]
		if !known {
			return "", errors.New("not in the stdin credentials")
		}
		return value, nil
	}, nil
}
