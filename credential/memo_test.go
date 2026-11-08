package credential

import (
	"context"
	"errors"
	"testing"
)

// counting is a resolver that counts its reads per reference.
type counting struct {
	reads map[string]int
	value string
	fail  bool
}

func (c *counting) resolve(_ context.Context, reference string) (string, error) {
	c.reads[reference]++
	if c.fail {
		return "", errors.New("synthetic failure")
	}
	return c.value, nil
}

func TestMemoCached(t *testing.T) {
	const ref = "op://example-vault/item/field"
	inner := &counting{reads: map[string]int{}, value: "synthetic-1"}
	memo := NewMemo()
	cached := memo.Cached(inner.resolve)
	for range 3 {
		if got, err := cached(t.Context(), ref); err != nil || got != "synthetic-1" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	if inner.reads[ref] != 1 {
		t.Fatalf("read %d times, want 1", inner.reads[ref])
	}
	// Another reference text is another key.
	if _, err := cached(t.Context(), "op://example-vault/item/other"); err != nil || len(inner.reads) != 2 {
		t.Fatalf("a new reference was not read: %v %v", err, inner.reads)
	}
	// A fresh read reads again and the cached resolver then answers with the new value.
	inner.value = "synthetic-2"
	if got, _ := memo.Fresh(inner.resolve)(t.Context(), ref); got != "synthetic-2" {
		t.Fatalf("fresh gave %q", got)
	}
	if got, _ := cached(t.Context(), ref); got != "synthetic-2" || inner.reads[ref] != 2 {
		t.Fatalf("cached after fresh: %q, %d reads", got, inner.reads[ref])
	}
}

func TestMemoKeepsNoFailureAndNoLocalReference(t *testing.T) {
	inner := &counting{reads: map[string]int{}, value: "synthetic", fail: true}
	memo := NewMemo()
	cached, fresh := memo.Cached(inner.resolve), memo.Fresh(inner.resolve)
	const ref = "op://example-vault/item/field"
	for range 2 {
		if _, err := cached(t.Context(), ref); err == nil {
			t.Fatal("failure not returned")
		}
	}
	if inner.reads[ref] != 2 {
		t.Fatalf("a failure was kept: %d reads", inner.reads[ref])
	}
	if _, err := fresh(t.Context(), ref); err == nil {
		t.Fatal("failure not returned by fresh")
	}
	inner.fail = false
	if _, err := cached(t.Context(), ref); err != nil || inner.reads[ref] != 4 {
		t.Fatalf("a failed fresh read left a value: %v, %d reads", err, inner.reads[ref])
	}
	// env:// and file:// cost the secret store nothing, so they are read each time.
	for _, local := range []string{"env://EXAMPLE", "file:///example/path"} {
		for _, resolve := range []Resolver{cached, cached, fresh} {
			if _, err := resolve(t.Context(), local); err != nil {
				t.Fatal(err)
			}
		}
		if inner.reads[local] != 3 {
			t.Fatalf("%s read %d times, want 3", local, inner.reads[local])
		}
	}
}

func TestMemoPrune(t *testing.T) {
	inner := &counting{reads: map[string]int{}, value: "synthetic"}
	memo := NewMemo()
	cached := memo.Cached(inner.resolve)
	const kept, dropped = "op://example-vault/kept/field", "op://example-vault/dropped/field"
	for _, ref := range []string{kept, dropped} {
		if _, err := cached(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
	memo.Prune() // both were asked for since the start
	asks := func(refs ...string) {
		for _, ref := range refs {
			if _, err := cached(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
		}
	}
	asks(kept)
	memo.Prune() // only kept was asked for since
	asks(kept, dropped)
	if inner.reads[kept] != 1 || inner.reads[dropped] != 2 {
		t.Fatalf("reads kept=%d dropped=%d, want 1 and 2", inner.reads[kept], inner.reads[dropped])
	}
}
