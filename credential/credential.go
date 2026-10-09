// Package credential resolves credential references to their values. A reference says
// where a value lives and is not secret itself; its scheme picks the resolver:
//
//   - op://vault/item/field or op://vault/item/section/field: a 1Password secret
//     reference, resolved by the resolver the caller passes (the SDK in production)
//   - env://NAME: the environment variable NAME of the gateway process
//   - file:///absolute/path: the content of a file, without surrounding whitespace
package credential

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

// Resolver resolves one reference to its value. Its errors never carry the value.
type Resolver func(ctx context.Context, reference string) (string, error)

// maxFile bounds a file:// credential; tokens and keys are far smaller.
const maxFile = 64 << 10

// opSegment is one segment of an op:// reference. Field names in 1Password may contain
// plain spaces, but no other whitespace, and a segment does not start or end with one.
const opSegment = `[^/\s](?:(?:[^/\s]| )*[^/\s])?`

var (
	opReference  = regexp.MustCompile(`^op://` + opSegment + `/` + opSegment + `(?:/` + opSegment + `){1,2}$`)
	envReference = regexp.MustCompile(`^env://[A-Za-z_][A-Za-z0-9_]*$`)
)

// Check reports whether reference has the shape of one of the supported schemes.
// Whether it resolves is the resolver's finding.
func Check(reference string) error {
	if opReference.MatchString(reference) || envReference.MatchString(reference) || isFileReference(reference) {
		return nil
	}
	return errors.New("not a credential reference (op://, env:// or file://)")
}

// isFileReference reports whether reference is file:// with a clean absolute path.
func isFileReference(reference string) bool {
	p, ok := strings.CutPrefix(reference, "file://")
	return ok && p != "/" && path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsAny(p, "\x00\n\r")
}

// New returns a resolver that resolves env:// and file:// references itself and op://
// references with onePassword. A nil onePassword refuses op:// references.
func New(onePassword Resolver) Resolver {
	return func(ctx context.Context, reference string) (string, error) {
		if err := Check(reference); err != nil {
			return "", err
		}
		switch {
		case strings.HasPrefix(reference, "env://"):
			return fromEnv(strings.TrimPrefix(reference, "env://"))
		case strings.HasPrefix(reference, "file://"):
			return fromFile(strings.TrimPrefix(reference, "file://"))
		case onePassword == nil:
			return "", errors.New("op:// reference without 1Password configured")
		default:
			return onePassword(ctx, reference)
		}
	}
}

// fromEnv returns the variable name; unset and empty are both refused.
func fromEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("environment variable %s is unset or empty", name)
	}
	return value, nil
}

// fromFile returns the content of the file at p without surrounding whitespace, so a
// trailing newline from an editor or echo is not part of the value.
func fromFile(p string) (string, error) {
	file, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("credential file: %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil {
		return "", fmt.Errorf("credential file %s: %v", p, err)
	}
	if len(data) > maxFile {
		return "", fmt.Errorf("credential file %s: larger than %d bytes", p, maxFile)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("credential file %s: empty", p)
	}
	return value, nil
}
