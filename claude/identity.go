package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
)

// maxRewriteBody is the largest request body whose client identity is rewritten: the
// Messages API's request size limit, 32 MB as read on 2026-10-19. The upstream refuses
// a larger body anyway, so it is forwarded as it came.
const maxRewriteBody = 32 << 20

// rewriteIdentity puts the account's deviceID and accountUUID into the device_id and
// account_uuid of out's metadata.user_id, so that each account reaches the upstream as
// a client of its own. Claude Code writes user_id as a JSON object encoded into a JSON
// string. Only the bytes of those two values change; every other byte of the body is
// kept, key order and escapes included, so the body still reads as the client wrote
// it. A body that is encoded, too large, unreadable, not a JSON object or without a
// user_id of that shape is forwarded unchanged.
func rewriteIdentity(out *http.Request, deviceID, accountUUID string) {
	if out.Body == nil || out.Body == http.NoBody {
		return
	}
	if encoding := out.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return
	}
	body := out.Body
	head, err := io.ReadAll(io.LimitReader(body, maxRewriteBody+1))
	if err != nil || len(head) > maxRewriteBody {
		// Reading the rest of body again returns the error, if there was one.
		out.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), body), body}
		return
	}
	rewritten := withIdentity(head, deviceID, accountUUID)
	out.Body = struct {
		io.Reader
		io.Closer
	}{bytes.NewReader(rewritten), body}
	out.ContentLength = int64(len(rewritten))
}

// withIdentity returns body with the device_id and account_uuid in every string
// metadata.user_id that holds a JSON object replaced by deviceID and accountUUID, or
// body itself when there is none.
func withIdentity(body []byte, deviceID, accountUUID string) []byte {
	var edits []edit
	for _, metadata := range memberValues(body, "metadata") {
		for _, userID := range memberValues(body[metadata.start:metadata.end], "user_id") {
			userID.start, userID.end = userID.start+metadata.start, userID.end+metadata.start
			raw := body[userID.start:userID.end]
			var inner string
			if json.Unmarshal(raw, &inner) != nil || !bytes.Equal(encodeString(inner), raw) {
				// Encoding the new value the same way would change more than the two IDs.
				continue
			}
			replaced, ok := replaceIDs(inner, deviceID, accountUUID)
			if ok {
				edits = append(edits, edit{userID, encodeString(replaced)})
			}
		}
	}
	return apply(body, edits)
}

// replaceIDs returns the JSON object text inner with every device_id and account_uuid
// member set to deviceID and accountUUID. ok is false when inner is not an object,
// when one of those members is not a string, or when it has neither.
func replaceIDs(inner string, deviceID, accountUUID string) (string, bool) {
	doc := []byte(inner)
	var edits []edit
	for key, value := range map[string]string{"device_id": deviceID, "account_uuid": accountUUID} {
		for _, s := range memberValues(doc, key) {
			if doc[s.start] != '"' {
				return "", false
			}
			edits = append(edits, edit{s, encodeString(value)})
		}
	}
	if len(edits) == 0 {
		return "", false
	}
	return string(apply(doc, edits)), true
}

// span is the byte range [start, end) of one value in a document.
type span struct{ start, end int }

// edit replaces the bytes of a span.
type edit struct {
	at   span
	with []byte
}

// apply returns doc with the edits made; the spans must not overlap.
func apply(doc []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return doc
	}
	slices.SortFunc(edits, func(a, b edit) int { return a.at.start - b.at.start })
	var out bytes.Buffer
	last := 0
	for _, e := range edits {
		out.Write(doc[last:e.at.start])
		out.Write(e.with)
		last = e.at.end
	}
	out.Write(doc[last:])
	return out.Bytes()
}

// memberValues returns the spans of the values of every member named key of the JSON
// object doc, in order, or nil when doc is not one valid JSON object.
func memberValues(doc []byte, key string) []span {
	if !json.Valid(doc) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(doc))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var spans []span
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
		end := int(decoder.InputOffset())
		start := end - len(value)
		// The decoder's offset is the end of the value it returned; confirm that the
		// bytes before it are that value before using them as its span.
		if start < 0 || !bytes.Equal(doc[start:end], value) {
			return nil
		}
		if name == key {
			spans = append(spans, span{start, end})
		}
	}
	return spans
}

// encodeString encodes s as a JSON string the way JSON.stringify does for the text
// Claude Code puts there: without escaping <, > and &.
func encodeString(s string) []byte {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = encoder.Encode(s)
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}
