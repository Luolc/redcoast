package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// TestQuotaReadingsFromHeaders (E4.1) checks which response headers yield a reading: a
// complete 200 and a 429 with rejected status do, utilization stored as the fraction it
// is, above 1 included; a window with a missing, malformed, negative or infinite
// utilization, a malformed status or a malformed reset yields none for that window; a
// reset already in the past is stored as read, the reader treats it later.
func TestQuotaReadingsFromHeaders(t *testing.T) {
	reset := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	header := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			h.Set("Anthropic-Ratelimit-Unified-"+pairs[i], pairs[i+1])
		}
		return h
	}
	for _, test := range []struct {
		name    string
		header  http.Header
		windows []string
	}{
		{"complete", header("5h-Utilization", "0.25", "5h-Status", "allowed", "5h-Reset", strconv.FormatInt(reset.Unix(), 10), "7d-Utilization", "0.5", "7d-Status", "allowed_warning", "7d-Reset", strconv.FormatInt(reset.Unix(), 10)), []string{"5h", "7d"}},
		{"rejected", header("5h-Utilization", "1.0", "5h-Status", "rejected", "7d-Utilization", "0.5"), []string{"5h", "7d"}},
		// The headers carry fractions: a client that shows 4.0% and 2.0% reads 0.04 and 0.02.
		{"small_fractions", header("5h-Utilization", "0.04", "7d-Utilization", "0.02"), []string{"5h", "7d"}},
		{"past_the_quota", header("5h-Utilization", "1.05", "5h-Status", "rejected"), []string{"5h"}},
		{"no_headers", header(), nil},
		{"missing_7d_utilization", header("5h-Utilization", "0.25", "7d-Status", "allowed"), []string{"5h"}},
		{"malformed_utilization", header("5h-Utilization", "25%", "7d-Utilization", "0.5"), []string{"7d"}},
		{"utilization_negative", header("5h-Utilization", "-0.1", "7d-Utilization", "0.5"), []string{"7d"}},
		{"utilization_not_a_number", header("5h-Utilization", "NaN", "7d-Utilization", "0.5"), []string{"7d"}},
		{"utilization_infinite", header("5h-Utilization", "+Inf", "7d-Utilization", "-Inf"), nil},
		{"malformed_status", header("5h-Utilization", "0.25", "5h-Status", "synthetic_status", "7d-Utilization", "0.5"), []string{"7d"}},
		{"malformed_reset", header("5h-Utilization", "0.25", "5h-Reset", "tomorrow", "7d-Utilization", "0.5", "7d-Reset", strconv.FormatInt(reset.Unix(), 10)), []string{"7d"}},
		{"reset_in_the_past", header("7d-Utilization", "0.5", "7d-Reset", "1700000000"), []string{"7d"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			readings := quotaReadings(test.header, "response")
			var windows []string
			for _, r := range readings {
				windows = append(windows, r.Window)
			}
			if len(windows) != len(test.windows) {
				t.Fatalf("windows=%v want %v", windows, test.windows)
			}
			for i := range windows {
				if windows[i] != test.windows[i] {
					t.Fatalf("windows=%v want %v", windows, test.windows)
				}
			}
			if test.name == "complete" {
				if readings[0].Utilization != 0.25 || readings[0].Status != "allowed" || !readings[0].ResetAt.Equal(reset) || readings[1].Utilization != 0.5 || readings[1].Status != "allowed_warning" {
					t.Fatalf("readings=%+v", readings)
				}
			}
			if test.name == "rejected" && readings[0].Utilization != 1 {
				t.Fatalf("1.0 not read as the whole window: %+v", readings[0])
			}
			if test.name == "measured" && (readings[0].Utilization != 0.03 || readings[1].Utilization != 0.01) {
				t.Fatalf("measured readings not stored as read: %+v", readings)
			}
			if test.name == "past_the_quota" && readings[0].Utilization != 1.05 {
				t.Fatalf("a reading past the quota was changed: %+v", readings[0])
			}
			if test.name == "reset_in_the_past" && !readings[0].ResetAt.Equal(time.Unix(1700000000, 0)) {
				t.Fatalf("past reset changed: %+v", readings[0])
			}
		})
	}
}

// TestUpstreamResponsesRecordQuota (E4.1, end to end) checks that the readings of each
// forwarded response reach quota_latest for the bound account, a 429 is recorded with
// its source, and a response with malformed headers leaves the previous good reading
// in place. Control arm: a later well-formed response does replace it.
func TestUpstreamResponsesRecordQuota(t *testing.T) {
	var headers http.Header
	var status int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for key, values := range headers {
			w.Header()[key] = values
		}
		w.WriteHeader(status)
	}))
	defer upstream.Close()
	h := newHarness(t, "sample-a")
	server := h.gateway(upstream.URL)
	send := func(code int, utilization5h, utilization7d, status7d string) {
		t.Helper()
		headers = http.Header{}
		headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", utilization5h)
		headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", utilization7d)
		if status7d != "" {
			headers.Set("Anthropic-Ratelimit-Unified-7d-Status", status7d)
		}
		status = code
		if got, _ := post(t, h, server, h.token, "", "", ""); got != code {
			t.Fatalf("status=%d want=%d", got, code)
		}
	}
	quota := func() map[string]session.Quota {
		t.Helper()
		q, err := h.store.LatestQuota(context.Background(), "sample-a")
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	send(200, "0.1", "0.2", "allowed")
	if q := quota(); q["5h"].Utilization != 0.1 || q["7d"].Utilization != 0.2 || q["7d"].Status != "allowed" || q["7d"].Source != "response" {
		t.Fatalf("after 200: %+v", q)
	}
	// Malformed: the 5h value carries a percent sign, the 7d status is unknown; both windows keep the first reading.
	send(200, "30%", "0.4", "synthetic_status")
	if q := quota(); q["5h"].Utilization != 0.1 || q["7d"].Utilization != 0.2 || q["7d"].Status != "allowed" {
		t.Fatalf("malformed reading overwrote a good one: %+v", q)
	}
	// NaN in one window must neither be stored nor stop the other window's reading.
	send(200, "NaN", "0.3", "allowed")
	if q := quota(); q["5h"].Utilization != 0.1 || q["7d"].Utilization != 0.3 {
		t.Fatalf("after a NaN window: %+v", q)
	}
	// Control arm: a well-formed 429 replaces both and names its source; a reading past
	// the quota is stored as read.
	send(429, "1.05", "0.95", "rejected")
	if q := quota(); q["5h"].Utilization != 1.05 || q["7d"].Utilization != 0.95 || q["7d"].Status != "rejected" || q["7d"].Source != "429" || q["5h"].Source != "429" {
		t.Fatalf("after 429: %+v", q)
	}
	if other, _ := h.store.LatestQuota(context.Background(), "sample-b"); len(other) != 0 {
		t.Fatal("readings written to another account")
	}
}
