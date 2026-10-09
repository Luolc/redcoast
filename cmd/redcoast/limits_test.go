package main

import (
	"testing"
	"time"

	"github.com/Luolc/redcoast/claude"
)

// TestLimitsConfiguration checks the idle, total, tunnel and account concurrency limits:
// the defaults, accepted values, and the refusal of an idle limit under a second or not
// shorter than a total, and of a tunnel or concurrency limit under 1.
func TestLimitsConfiguration(t *testing.T) {
	for _, test := range []struct {
		name        string
		yaml        string
		limits      claude.Limits // zero when refused
		concurrency int
	}{
		{"defaults", "", claude.Limits{Idle: 5 * time.Minute, MaxRequest: time.Hour, MaxTunnel: 6 * time.Hour, Tunnels: 256}, 32},
		{"set", "limits: {idle_timeout: 2m, max_request_duration: 90m, max_tunnel_duration: 12h, max_tunnels: 2, account_concurrency: 1}\n", claude.Limits{Idle: 2 * time.Minute, MaxRequest: 90 * time.Minute, MaxTunnel: 12 * time.Hour, Tunnels: 2}, 1},
		{"idle_under_a_second", "limits: {idle_timeout: 500ms}\n", claude.Limits{}, 0},
		{"request_not_longer_than_idle", "limits: {idle_timeout: 1h}\n", claude.Limits{}, 0},
		{"tunnel_not_longer_than_idle", "limits: {max_tunnel_duration: 5m}\n", claude.Limits{}, 0},
		{"no_tunnels", "limits: {max_tunnels: 0}\n", claude.Limits{}, 0},
		{"no_account_concurrency", "limits: {account_concurrency: 0}\n", claude.Limits{}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(baseConfig + test.yaml))
			if test.limits == (claude.Limits{}) {
				if err == nil {
					t.Fatal("invalid limits accepted")
				}
				return
			}
			if err != nil || cfg.limits != test.limits || cfg.accountConcurrency != test.concurrency {
				t.Fatalf("limits %+v, concurrency %d, %v; want %+v, %d", cfg.limits, cfg.accountConcurrency, err, test.limits, test.concurrency)
			}
		})
	}
}
