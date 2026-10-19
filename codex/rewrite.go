package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// turnMetadata is the header in which the Codex CLI sends, among other things, the
// installation ID of its $CODEX_HOME. The request body carries the same ID twice, in
// client_metadata's x-codex-installation-id and inside its x-codex-turn-metadata.
const turnMetadata = "X-Codex-Turn-Metadata"

// clientInstallationID is the shape the gateway replaces: a lower-case UUID, the same
// length as the account's, so that the body keeps its length.
var clientInstallationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// screen answers what the gateway does not forward before anything else: a WebSocket
// upgrade gets 426, so that the client falls back to HTTP streaming at once, and a
// request whose installation ID the gateway could not replace everywhere gets 400.
func screen(r *http.Request) (int, string) {
	if r.Header.Get("Upgrade") != "" {
		return http.StatusUpgradeRequired, "redcoast: WebSocket is not supported; use HTTP streaming"
	}
	values := r.Header.Values(turnMetadata)
	if len(values) == 0 {
		return 0, ""
	}
	id, ok := installationIDIn(values)
	switch {
	case !ok:
		return http.StatusBadRequest, "redcoast: unreadable " + strings.ToLower(turnMetadata)
	case id == "":
		return 0, ""
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return http.StatusBadRequest, "redcoast: a body that carries an installation ID must not be compressed"
	}
	return 0, ""
}

// installationIDIn returns the installation ID of the one turn metadata value, or ""
// when it has none. ok is false when there is more than one value, the value is not a
// JSON object, or its installation_id is not a string of clientInstallationID's shape.
func installationIDIn(values []string) (id string, ok bool) {
	if len(values) != 1 {
		return "", false
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal([]byte(values[0]), &metadata) != nil || metadata == nil {
		return "", false
	}
	raw, present := metadata["installation_id"]
	if !present {
		return "", true
	}
	if json.Unmarshal(raw, &id) != nil || !clientInstallationID.MatchString(id) {
		return "", false
	}
	return id, true
}

// replaceInstallationID puts account's installation ID in place of the client's
// wherever the client's appears in the turn metadata header and the body. screen has
// checked the header and that the body is not compressed. Both IDs have the same
// length, so the body keeps its length and is rewritten as it streams.
func replaceInstallationID(out *http.Request, account string) {
	id, ok := installationIDIn(out.Header.Values(turnMetadata))
	if !ok || id == "" || id == account {
		return
	}
	out.Header.Set(turnMetadata, strings.ReplaceAll(out.Header.Get(turnMetadata), id, account))
	if out.Body != nil && out.Body != http.NoBody {
		out.Body = &swapReader{src: out.Body, old: []byte(id), new: []byte(account)}
	}
}

// swapReader replaces every occurrence of old in src with new, which has old's
// length, while it streams. It holds back the last len(old)-1 bytes it has read until
// more arrive, so that an occurrence split across two reads is found too.
type swapReader struct {
	src      io.ReadCloser
	old, new []byte
	pending  []byte // read and replaced, not yet returned
	ready    int    // the length of pending's prefix that can be returned
	err      error  // the error src returned, kept for after pending is drained
}

// Read returns the rewritten bytes.
func (s *swapReader) Read(p []byte) (int, error) {
	for s.ready == 0 {
		if s.err != nil {
			if len(s.pending) == 0 {
				return 0, s.err
			}
			s.ready = len(s.pending)
			break
		}
		chunk := make([]byte, max(len(p), 32<<10))
		n, err := s.src.Read(chunk)
		s.pending = append(s.pending, chunk[:n]...)
		s.err = err
		replaceAll(s.pending, s.old, s.new)
		s.ready = max(len(s.pending)-(len(s.old)-1), 0)
	}
	n := copy(p, s.pending[:s.ready])
	s.pending, s.ready = s.pending[n:], s.ready-n
	return n, nil
}

// Close closes src.
func (s *swapReader) Close() error {
	return s.src.Close()
}

// replaceAll replaces every occurrence of old in b with new, of the same length, in
// place.
func replaceAll(b, old, new []byte) {
	for at := 0; ; {
		i := bytes.Index(b[at:], old)
		if i < 0 {
			return
		}
		copy(b[at+i:], new)
		at += i + len(new)
	}
}
