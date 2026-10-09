package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenPathWithURICharacters checks that the store opens the file at exactly the
// path it was given when the path holds characters a URI would interpret ('#', '?',
// '%', a space), and leaves no file at the path a URI parser would truncate it to.
// Go names a repeated subtest "<name>#01", so t.TempDir() produces such paths.
// Control arm: a plain path opens the same way.
func TestOpenPathWithURICharacters(t *testing.T) {
	for _, dir := range []string{"plain", "sub#01", "what?now", "100%", "a space"} {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, dir, "gateway.sqlite")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			// The paths a URI parser would truncate this one to, with their state
			// before: a leftover from an earlier run must not be mistaken for a write.
			truncated := map[string]time.Time{}
			for _, cut := range []string{"#", "?"} {
				if before, _, found := strings.Cut(path, cut); found {
					truncated[before] = modTime(before)
				}
			}
			store, err := Open(context.Background(), path, Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("no store at the given path: %v", err)
			}
			for before, was := range truncated {
				if now := modTime(before); now != was {
					t.Fatalf("a file was written at the truncated path %s", before)
				}
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if names[0] != "gateway.sqlite" {
				t.Fatalf("directory holds %v", names)
			}
		})
	}
}

// modTime is the modification time of path, or the zero time when it does not exist.
func modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
