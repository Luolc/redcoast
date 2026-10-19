package codex

import (
	"bytes"
	"encoding/json"
	"errors"
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
// request whose installation ID the gateway could not find and replace, because its
// turn metadata is unreadable or its body compressed, gets 400. Pi compresses its
// bodies and has no installation ID, so a compressed body is let through only from Pi
// (originator: pi) and without one in the turn metadata.
func screen(r *http.Request) (int, string) {
	if r.Header.Get("Upgrade") != "" {
		return http.StatusUpgradeRequired, "redcoast: WebSocket is not supported; use HTTP streaming"
	}
	var id string
	if values := r.Header.Values(turnMetadata); len(values) > 0 {
		var ok bool
		if id, ok = installationIDIn(values); !ok {
			return http.StatusBadRequest, "redcoast: unreadable " + strings.ToLower(turnMetadata)
		}
	}
	if compressed(r) && (id != "" || r.Header.Get("Originator") != "pi") {
		return http.StatusBadRequest, "redcoast: a body that may carry an installation ID must not be compressed"
	}
	return 0, ""
}

// compressed reports whether r's body has a content coding.
func compressed(r *http.Request) bool {
	encoding := r.Header.Get("Content-Encoding")
	return encoding != "" && encoding != "identity"
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
// length, so the body keeps its length and, when the header gives the ID, is rewritten
// as it streams. Requests whose turn metadata has no installation ID, such as the
// Codex CLI's memory requests, can still carry it in the body's client_metadata, so
// their body is read whole to find it there, unless it is a compressed body screen let
// through.
func replaceInstallationID(out *http.Request, account string) {
	hasBody := out.Body != nil && out.Body != http.NoBody
	id, _ := installationIDIn(out.Header.Values(turnMetadata))
	switch {
	case id == "" && hasBody && !compressed(out):
		out.Body = replaceInBody(out.Body, account)
	case id == "" || id == account:
	default:
		out.Header.Set(turnMetadata, strings.ReplaceAll(out.Header.Get(turnMetadata), id, account))
		if hasBody {
			out.Body = &swapReader{src: out.Body, old: []byte(id), new: []byte(account)}
		}
	}
}

// maxReadBody bounds a body read whole by replaceInBody.
const maxReadBody = 64 << 20

// errUnreadableBody fails a request whose body may carry an installation ID that the
// gateway cannot find: it is not a JSON object, is larger than maxReadBody, or has an
// x-codex-installation-id in client_metadata that is not a string of
// clientInstallationID's shape. The transport fails before the body leaves, and the
// client gets 502.
var errUnreadableBody = errors.New("redcoast: the request body's installation ID cannot be read")

// replaceInBody returns body with account's installation ID in place of the one in its
// client_metadata, or a body that fails with errUnreadableBody.
func replaceInBody(body io.ReadCloser, account string) io.ReadCloser {
	data, err := io.ReadAll(io.LimitReader(body, maxReadBody+1))
	_ = body.Close()
	var request struct {
		ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
	}
	if err != nil || len(data) > maxReadBody || json.Unmarshal(data, &request) != nil {
		return io.NopCloser(failedReader{})
	}
	if raw, present := request.ClientMetadata["x-codex-installation-id"]; present {
		var id string
		if json.Unmarshal(raw, &id) != nil || !clientInstallationID.MatchString(id) {
			return io.NopCloser(failedReader{})
		}
		replaceAll(data, []byte(id), []byte(account))
	}
	return io.NopCloser(bytes.NewReader(data))
}

// failedReader fails every read with errUnreadableBody.
type failedReader struct{}

// Read fails.
func (failedReader) Read([]byte) (int, error) {
	return 0, errUnreadableBody
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
