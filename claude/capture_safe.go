package claude

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Luolc/redcoast/session"
)

// The classifiers in this file only see the record's copy of a request or response;
// forwarded bytes never pass through them. They keep a value only when an allowlist
// names it and replace everything else with one of two markers. Some allowlist
// entries are synthetic test inputs (client.example, /v1/a/b, "synthetic prompt") so
// tests can check that safe values survive.

// hidden replaces a value that no allowlist covers; it may or may not be sensitive.
const hidden = "[UNCLASSIFIED]"

// redacted replaces a value that is known to be a credential or an identity.
const redacted = "[REDACTED]"

// partialTokenMinimum is the shortest OAuth token that a record shows in part. Its
// first 12 and last 4 bytes are enough to tell which account's token was sent; a
// shorter token would give away too large a share of itself.
const partialTokenMinimum = 24

// partialToken returns the first 12 bytes of token, "***" and its last 4 bytes, and
// false when token is shorter than partialTokenMinimum.
func partialToken(token string) (string, bool) {
	if len(token) < partialTokenMinimum {
		return "", false
	}
	return token[:12] + "***" + token[len(token)-4:], true
}

// safeAuthorization keeps an account's Bearer token in its partial form and redacts
// everything else: session credentials, which a record identifies by the traffic
// table's session ID instead, Bearer tokens too short for a partial form, and any
// value that is not exactly a scheme and a token, so that extra whitespace cannot
// disguise a session credential as an account token.
func safeAuthorization(value string) string {
	fields := strings.Fields(value)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || strings.HasPrefix(fields[1], session.TokenPrefix) {
		return redacted
	}
	if shown, ok := partialToken(fields[1]); ok {
		return fields[0] + " " + shown
	}
	return redacted
}

// safeHost keeps host when it is an IP literal or one of the known host names.
func safeHost(host string) string {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	if net.ParseIP(name) != nil || name == "api.anthropic.com" || name == "client.example" {
		return host
	}
	return hidden
}

// safePath keeps an escaped path only when it exactly matches a known path.
func safePath(path string) string {
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens", "/api/hello", "/stream", "/warm", "/infer", "/v1/a/b", "/v1/a%2Fb", "/":
		return path
	}
	return hidden
}

// safeQuery replaces every query value and keeps the number of values. Query names
// come from the client, so only credential names survive; the rest are counted under
// one placeholder name.
func safeQuery(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, items := range values {
		name, value := hidden, hidden
		switch strings.ToLower(key) {
		case "api_key", "key", "token", "access_token", "auth", "authorization", "password":
			name, value = key, redacted
		}
		for range items {
			out[name] = append(out[name], value)
		}
	}
	return out
}

// safeHeaders classifies every value of every header and keeps the number of values.
// Header names are kept.
func safeHeaders(headers http.Header) http.Header {
	out := make(http.Header, len(headers))
	for key, values := range headers {
		name := strings.ToLower(key)
		for _, value := range values {
			switch {
			case name == "authorization":
				value = safeAuthorization(value)
			case sensitiveHeader(name):
				value = redacted
			case !safeHeaderValue(name, value):
				value = hidden
			}
			out[key] = append(out[key], value)
		}
	}
	return out
}

// sensitiveHeader reports whether a lower-case header name carries credentials or
// account identity.
func sensitiveHeader(name string) bool {
	switch name {
	case "authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization", "anthropic-organization-id", "anthropic-workspace-id", "anthropic-ratelimit-unified-representative-claim":
		return true
	}
	return false
}

// safeHeaderValue reports whether value is an allowed value of the lower-case header name.
func safeHeaderValue(name, value string) bool {
	switch name {
	case "content-type":
		return value == "application/json" || value == "text/event-stream" || value == "text/plain; charset=utf-8"
	case "content-encoding", "accept-encoding":
		return value == "gzip" || value == "br" || value == "identity"
	case "anthropic-version":
		return value == "2023-06-01"
	}
	return safeQuotaHeader(name, value)
}

// safeQuotaHeader keeps values of named quota headers only when they match the field's type.
// Requests pass through safeHeaders too, so unlisted names and other shapes stay hidden.
func safeQuotaHeader(key, value string) bool {
	switch key {
	case "anthropic-ratelimit-unified-status", "anthropic-ratelimit-unified-5h-status",
		"anthropic-ratelimit-unified-7d-status", "anthropic-ratelimit-unified-overage-status":
		return value == "allowed" || value == "allowed_warning" || value == "rejected"
	case "anthropic-ratelimit-unified-reset", "anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset", "anthropic-ratelimit-unified-overage-reset",
		"anthropic-ratelimit-unified-5h-utilization", "anthropic-ratelimit-unified-7d-utilization",
		"anthropic-ratelimit-unified-fallback-percentage", "retry-after":
		return value != "" && len(value) <= 20 && strings.Trim(value, "0123456789.") == ""
	case "x-should-retry":
		return value == "true" || value == "false"
	}
	return false
}

// safeValue classifies one value decoded by encoding/json. key is the field name the
// value sits under: array elements inherit their parent's key, and the top level is "".
// Every allowlist is keyed by it.
func safeValue(key string, value any) any {
	if sensitiveField(key) {
		return redacted
	}
	switch v := value.(type) {
	case map[string]any:
		return safeObject(key, v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = safeValue(key, item)
		}
		return out
	case string:
		return safeString(key, v)
	case float64, bool, nil:
		if scalarField(key) {
			return v
		}
	}
	return hidden
}

// safeObject classifies the fields of an object found under key. Field names can be
// user data (tool arguments, schema properties), so only protocol names under protocol
// containers are kept; the rest are only counted.
func safeObject(key string, object map[string]any) map[string]any {
	out := make(map[string]any, len(object))
	container := protocolContainer(key)
	unclassified := 0
	for name, value := range object {
		if !container || !protocolField(name) {
			unclassified++
			continue
		}
		out[name] = safeValue(name, value)
	}
	if unclassified > 0 {
		out[hidden] = map[string]any{"fields": unclassified}
	}
	return out
}

// safeString keeps a string only when the allowlist for key contains it. Otherwise
// only its length is kept.
func safeString(key, value string) any {
	switch key {
	case "role":
		if value == "user" || value == "assistant" {
			return value
		}
	case "type":
		switch value {
		case "text", "message", "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "text_delta", "input_json_delta", "tool_use", "tool_result", "ping", "error",
			"authentication_error", "permission_error", "invalid_request_error", "rate_limit_error", "overloaded_error", "api_error", "not_found_error":
			return value
		}
	case "model":
		if strings.HasPrefix(value, "claude-") && len(value) < 80 && strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789-.") == "" {
			return value
		}
	case "text", "content":
		if value == "Hi" || value == "Hi!" || value == "Hello" || value == "Hello!" {
			return value
		}
	case "name":
		switch value {
		case "Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch", "TodoWrite", "Task":
			return value
		}
	case "stop_reason":
		if value == "end_turn" || value == "max_tokens" || value == "tool_use" {
			return value
		}
	}
	return map[string]any{"kind": "unclassified_string", "bytes": len(value)}
}

// sensitiveField reports whether a field holds credentials or identity, whatever its value.
func sensitiveField(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization", "token", "access_token", "refresh_token", "api_key", "password", "user_id", "account_id", "email", "id":
		return true
	}
	return false
}

// protocolContainer reports whether objects under key are Messages API structures
// whose field names can be classified. The top level ("") is one.
func protocolContainer(key string) bool {
	switch key {
	case "", "messages", "message", "content", "content_block", "delta", "usage", "metadata", "system", "tools", "input_schema", "properties", "items", "cache_control", "tool_choice", "thinking", "output_config", "error":
		return true
	}
	return false
}

// scalarField reports whether numbers, booleans and nulls under key are request
// parameters or token usage that can be kept.
func scalarField(key string) bool {
	switch key {
	case "max_tokens", "temperature", "top_p", "stream", "index", "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens":
		return true
	}
	return false
}

// protocolField reports whether name is a Messages API field that safeValue classifies.
// It is an explicit list rather than the union of the other allowlists so that the
// set of field names that can appear in a record is reviewed in one place.
func protocolField(name string) bool {
	switch name {
	case "messages", "message", "content", "content_block", "delta", "usage", "metadata", "system", "tools", "input_schema", "properties", "items", "cache_control", "tool_choice", "thinking", "output_config", "error",
		"role", "type", "model", "text", "name", "stop_reason",
		"max_tokens", "temperature", "top_p", "stream", "index", "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
		"token", "access_token", "refresh_token", "api_key", "password", "user_id", "account_id", "email", "id":
		return true
	}
	return false
}

// syntheticSSE is a test event that safeBody keeps verbatim.
const syntheticSSE = "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi!\"}}\r\n\r\n"

// safeBody classifies the kept bytes of a body: synthetic test inputs verbatim, a JSON
// document through safeValue, an SSE stream line by line, anything else not at all.
// The result names its format so a reader knows how to interpret it.
func safeBody(data []byte) any {
	if len(data) == 0 || bytes.Equal(data, []byte("synthetic prompt")) || bytes.Equal(data, []byte(syntheticSSE)) || bytes.Equal(data, []byte{0x1f, 0x8b, 0x00, 0xff}) {
		return map[string]any{"format": "raw-base64", "data": base64.StdEncoding.EncodeToString(data)}
	}
	var decoded any
	if json.Unmarshal(data, &decoded) == nil {
		return map[string]any{"format": "classified-json", "data": safeValue("", decoded)}
	}
	if bytes.Contains(data, []byte("data:")) {
		return map[string]any{"format": "redacted-lines", "data": safeEventLines(data)}
	}
	return map[string]any{"format": "unclassified", "data": hidden}
}

// safeEventLines classifies an SSE stream: blank lines stay, data lines holding JSON
// are classified, and every other line is hidden.
func safeEventLines(data []byte) []string {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if line == "" || line == "\r" {
			continue
		}
		var decoded any
		if strings.HasPrefix(line, "data:") && json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &decoded) == nil {
			lines[i] = fmtSafeJSON(safeValue("", decoded))
		} else {
			lines[i] = hidden
		}
	}
	return lines
}

// fmtSafeJSON encodes a classified value as one line of JSON.
func fmtSafeJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return hidden
	}
	return string(data)
}

// safeEncodedBody classifies a body with the given Content-Encoding. gzip bodies are
// decompressed, up to captureBodyLimit bytes, before classification; other encodings
// are classified as they are.
func safeEncodedBody(data []byte, encoding string) any {
	if encoding != "gzip" {
		return safeBody(data)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return map[string]any{"format": "gzip-analysis-failed"}
	}
	decoded, readErr := io.ReadAll(io.LimitReader(reader, captureBodyLimit+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(decoded) > captureBodyLimit {
		return map[string]any{"format": "gzip-analysis-incomplete"}
	}
	return map[string]any{"format": "gzip-analysis", "analysis": safeBody(decoded)}
}
