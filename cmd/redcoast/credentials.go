package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/1password/onepassword-sdk-go"

	"github.com/Luolc/redcoast/claude"
	"github.com/Luolc/redcoast/credential"
)

// maxCredentialsInput bounds the JSON object the test entry reads from stdin.
const maxCredentialsInput = 64 << 10

// credentials is where the gateway's references are resolved and its secret store is
// opened. Values of op:// references are kept in memory by memo, and one SDK client
// serves every read and write for as long as the service account token is the same, so
// that a reload or an account write asks 1Password only for what it has not read. Values
// live in the process and nowhere else.
type credentials struct {
	cfg  config
	memo *credential.Memo

	mu     sync.Mutex
	stdin  claude.Resolver // the test entry's, read once
	token  string          // the service account token client was built with
	client *onepassword.Client
	sdk    claude.Resolver // resolves with client, retrying a rate limit

	// connect opens the client for a token and the resolver over it; tests replace it.
	connect func(ctx context.Context, token string) (*onepassword.Client, claude.Resolver, error)
}

func newCredentials(cfg config) *credentials {
	return &credentials{cfg: cfg, memo: credential.NewMemo(), connect: connectOnePassword}
}

// resolver builds the resolver the configuration asks for: the stdin test entry, or
// env:// and file:// references with op:// ones through the 1Password SDK when a
// service account token is configured. The token reference is read each time, so a
// rotated service account token is picked up. The resolver answers an op:// reference it
// has read before from memory; with fresh it reads from 1Password and keeps the value, for
// a write that must see the store as it is now.
func (c *credentials) resolver(ctx context.Context, fresh bool) (claude.Resolver, error) {
	loadCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	if c.cfg.credentialsStdin {
		return c.stdinResolver(loadCtx)
	}
	resolve := credential.New(nil)
	if c.cfg.tokenRef == "" {
		return resolve, nil
	}
	token, err := resolve(loadCtx, c.cfg.tokenRef)
	if err != nil {
		return nil, fmt.Errorf("1Password service account token: %v", err)
	}
	_, sdk, err := c.sdkFor(loadCtx, token)
	if err != nil {
		return nil, err
	}
	if fresh {
		return credential.New(c.memo.Fresh(sdk)), nil
	}
	return credential.New(c.memo.Cached(sdk)), nil
}

// stdinResolver reads the test entry's credentials once and returns that resolver after.
func (c *credentials) stdinResolver(ctx context.Context) (claude.Resolver, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		inputCtx, cancel := context.WithTimeout(ctx, credentialsInputTimeout)
		defer cancel()
		resolve, err := stdinResolver(inputCtx, os.Stdin)
		if err != nil {
			return nil, err
		}
		c.stdin = resolve
	}
	return c.stdin, nil
}

// secretStore opens the secret store that setup writes to, over the same client the
// references are read with.
func (c *credentials) secretStore(ctx context.Context) (credential.Store, error) {
	if c.cfg.tokenRef == "" || c.cfg.credentialsStdin {
		return nil, errors.New("no 1Password service account token configured")
	}
	loadCtx, cancel := context.WithTimeout(ctx, accountsLoadTimeout)
	defer cancel()
	token, err := credential.New(nil)(loadCtx, c.cfg.tokenRef)
	if err != nil {
		return nil, errors.New("1Password service account token")
	}
	client, _, err := c.sdkFor(loadCtx, token)
	if err != nil {
		return nil, errors.New("1Password client")
	}
	return onePasswordItems{client}, nil
}

// sdkFor returns the SDK client for token and the resolver over it, building them only
// when token is not the one they were built with. The token stays in the client; it is
// in no error.
func (c *credentials) sdkFor(ctx context.Context, token string) (*onepassword.Client, claude.Resolver, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sdk != nil && c.token == token {
		return c.client, c.sdk, nil
	}
	client, sdk, err := c.connect(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	c.token, c.client, c.sdk = token, client, sdk
	return client, sdk, nil
}

// connectOnePassword opens the SDK client for token and the resolver over it.
func connectOnePassword(ctx context.Context, token string) (*onepassword.Client, claude.Resolver, error) {
	client, err := onepassword.NewClient(ctx, onepassword.WithServiceAccountToken(token), onepassword.WithIntegrationInfo("redcoast", version))
	if err != nil {
		return nil, nil, fmt.Errorf("1Password client: %v", err)
	}
	return client, clientResolver(client), nil
}

// onePasswordItems is the write path of the official SDK as credential.Store: it lists
// vaults and items, reads an item's field names and its expiry date, sets one field of an
// item and the date of its expiry field and creates one item with one concealed field and
// an expiry date, and calls nothing else of the Items API.
type onePasswordItems struct{ client *onepassword.Client }

// readError is err, or credential.ErrRateLimited when the SDK says the account was asked
// too often, so the credential package can tell it without knowing the SDK.
func readError(err error) error {
	if isRateLimited(err) {
		return credential.ErrRateLimited
	}
	return err
}

func isRateLimited(err error) bool {
	var limited *onepassword.RateLimitExceededError
	return errors.As(err, &limited)
}

func (s onePasswordItems) Vaults(ctx context.Context) ([]credential.Vault, error) {
	list, err := s.client.Vaults().List(ctx)
	runtime.KeepAlive(s.client)
	if err != nil {
		return nil, readError(err)
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
		return nil, readError(err)
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
		return credential.Item{}, readError(err)
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

// isExpires reports whether f is an expiry field: its title is the expiry field's name,
// ignoring case.
func isExpires(f onepassword.ItemField) bool {
	return strings.EqualFold(f.Title, credential.ExpiresField)
}

// SetFields reads the item again and sets the value of its one field with this ID and
// title and the date of its one expiry field, if it has one, so a field that changed
// since the caller looked is not written blindly; the rest of the item is put back as it
// was read, and no field is added.
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

// rateLimitBackoff is how long retryRateLimited waits before each of its retries. It
// only rides out a short burst: the service's allowance runs per hour and per day.
var rateLimitBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// clientResolver returns a resolver over client that keeps client reachable for as long
// as the resolver is. The SDK registers a finalizer on the Client that releases the
// underlying client ID; the method value client.Secrets().Resolve holds only the inner
// client, so after a collection the ID could be released while the resolver is still
// in use and every later resolution would fail. It retries a rate limited resolution.
func clientResolver(client *onepassword.Client) claude.Resolver {
	return retryRateLimited(func(ctx context.Context, reference string) (string, error) {
		value, err := client.Secrets().Resolve(ctx, reference)
		runtime.KeepAlive(client)
		return value, err
	}, rateLimitBackoff, accountsLoadTimeout)
}

// retryRateLimited returns a resolver that resolves with inner and, when inner says the
// account was asked too often, waits backoff[0], tries again, waits backoff[1], and so
// on, at most len(backoff) retries. It returns at once when ctx ends, with the rate limit
// error joined to ctx's error, and when the retries run out with inner's last error,
// unchanged. Any other error is returned at once. Once the retries have run out for one
// reference, the references asked for during the next cool are tried once and not
// retried, so a load of many accounts under a lasting limit fails fast instead of
// spending its whole timeout in waits.
func retryRateLimited(inner claude.Resolver, backoff []time.Duration, cool time.Duration) claude.Resolver {
	var mu sync.Mutex
	var coolUntil time.Time
	return func(ctx context.Context, reference string) (string, error) {
		for retry := 0; ; retry++ {
			value, err := inner(ctx, reference)
			if err == nil || !isRateLimited(err) {
				return value, err
			}
			mu.Lock()
			spent := retry == len(backoff) || time.Now().Before(coolUntil)
			if retry == len(backoff) {
				coolUntil = time.Now().Add(cool)
			}
			mu.Unlock()
			if spent {
				return value, err
			}
			wait := time.NewTimer(backoff[retry])
			select {
			case <-ctx.Done():
				wait.Stop()
				return "", errors.Join(err, ctx.Err())
			case <-wait.C:
			}
		}
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
