package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/1password/onepassword-sdk-go"

	"github.com/Luolc/redcoast/claude"
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
