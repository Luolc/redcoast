package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHealthAndEgressConfiguration checks the health endpoint's and the dashboard's
// listen addresses, one IP and port each, never a wildcard and never the same, and the
// lower bound of the egress interval.
func TestHealthAndEgressConfiguration(t *testing.T) {
	// The first address of 100.64.0.0/10, a synthetic tailnet address.
	shared := "100.64.0.1:7804"
	for _, test := range []struct {
		name     string
		yaml     string
		health   string // expected when accepted; empty when refused
		interval time.Duration
	}{
		{"defaults", "", "127.0.0.1:7804", 15 * time.Minute},
		{"tailnet_address", "listen: {health: '" + shared + "'}\negress_interval: 1m\n", shared, time.Minute},
		{"ipv4_wildcard", "listen: {health: '0.0.0.0:7804'}\n", "", 0},
		{"ipv6_wildcard", "listen: {health: '[::]:7804'}\n", "", 0},
		{"no_host", "listen: {health: ':7804'}\n", "", 0},
		{"host_name", "listen: {health: 'gateway.example.test:7804'}\n", "", 0},
		{"on_reverse_address", "listen: {health: '127.0.0.1:8789'}\n", "", 0},
		{"interval_below_a_minute", "egress_interval: 59s\n", "", 0},
		{"dashboard_on_health_address", "listen: {dashboard: '127.0.0.1:7804'}\n", "", 0},
		{"dashboard_wildcard", "listen: {dashboard: '0.0.0.0:7805'}\n", "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(baseConfig + test.yaml))
			if test.health == "" {
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil || cfg.healthListen != test.health || cfg.egressInterval != test.interval || cfg.dashboardListen != "127.0.0.1:7805" {
				t.Fatalf("configuration refused or changed: %+v %v", cfg, err)
			}
		})
	}
}

// TestNotifyReady checks that the gateway sends READY=1 to the socket NOTIFY_SOCKET
// names, at a path and in the abstract namespace; the control arm without the variable
// sends nothing to the same socket.
func TestNotifyReady(t *testing.T) {
	for _, arm := range []string{"path", "abstract", "unset"} {
		t.Run(arm, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "notify.sock")
			variable := name
			if arm == "abstract" {
				variable = "@" + name
				name = "\x00" + name
			}
			listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			if arm == "unset" {
				variable = ""
			}
			t.Setenv("NOTIFY_SOCKET", variable)
			if err := notifyReady(); err != nil {
				t.Fatal(err)
			}
			if err := listener.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, 64)
			n, err := listener.Read(buffer)
			if arm == "unset" {
				if err == nil {
					t.Fatalf("received %q without NOTIFY_SOCKET", buffer[:n])
				}
				return
			}
			if want := fmt.Sprintf("MAINPID=%d\nREADY=1", os.Getpid()); err != nil || string(buffer[:n]) != want {
				t.Fatalf("received %q, %v; want %q", buffer[:n], err, want)
			}
		})
	}
}
