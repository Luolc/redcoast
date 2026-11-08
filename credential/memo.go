package credential

import (
	"context"
	"strings"
	"sync"
)

// Memo keeps the values of op:// references that its resolvers read, in memory only, so
// a pass that reads the same references again, such as a reload, asks the secret store
// only for those it has not read. A reference whose text changed is another key and is
// read; a value that changed behind an unchanged reference is read again only when a
// Fresh resolver reads it. env:// and file:// references are never kept: they cost no
// secret store request, so they are read each time.
type Memo struct {
	mu     sync.Mutex
	values map[string]string
	// used are the references a Cached resolver answered or read since the last Prune.
	used map[string]bool
}

// NewMemo returns an empty Memo.
func NewMemo() *Memo {
	return &Memo{values: map[string]string{}, used: map[string]bool{}}
}

func isSecretStore(reference string) bool { return strings.HasPrefix(reference, "op://") }

// Cached returns a resolver that answers an op:// reference from memory when it has read
// the reference before, and otherwise reads it with inner and keeps the value. A failed
// read is not kept.
func (m *Memo) Cached(inner Resolver) Resolver {
	return func(ctx context.Context, reference string) (string, error) {
		if !isSecretStore(reference) {
			return inner(ctx, reference)
		}
		m.mu.Lock()
		value, ok := m.values[reference]
		if ok {
			m.used[reference] = true
		}
		m.mu.Unlock()
		if ok {
			return value, nil
		}
		value, err := inner(ctx, reference)
		if err != nil {
			return value, err
		}
		m.mu.Lock()
		m.values[reference], m.used[reference] = value, true
		m.mu.Unlock()
		return value, nil
	}
}

// Fresh returns a resolver that always reads with inner and keeps the value in place of
// the one it had, for the references whose value may have changed in the store.
func (m *Memo) Fresh(inner Resolver) Resolver {
	return func(ctx context.Context, reference string) (string, error) {
		value, err := inner(ctx, reference)
		if err != nil || !isSecretStore(reference) {
			return value, err
		}
		m.mu.Lock()
		m.values[reference] = value
		m.mu.Unlock()
		return value, nil
	}
}

// Prune forgets the values that no Cached resolver asked for since the last Prune: those
// of references that the last full load no longer names.
func (m *Memo) Prune() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for reference := range m.values {
		if !m.used[reference] {
			delete(m.values, reference)
		}
	}
	m.used = map[string]bool{}
}
