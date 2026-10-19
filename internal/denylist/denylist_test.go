package denylist

import "testing"

func TestDenied(t *testing.T) {
	for host, want := range map[string]bool{
		"chatgpt.com":                  true,
		"ChatGPT.com.":                 true,
		"auth.openai.com":              true,
		"o33249.ingest.us.sentry.io":   true,
		"ab.chatgpt.com":               false,
		"openai.com":                   false,
		"api.anthropic.com":            false,
		"o33249.ingest.us.sentry.io.x": false,
	} {
		if got := Denied(host); got != want {
			t.Errorf("Denied(%q) = %v, want %v", host, got, want)
		}
	}
}
