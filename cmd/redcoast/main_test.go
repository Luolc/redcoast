package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Luolc/redcoast/session"
)

// basePaths are the keys a configuration must set; baseConfig adds the stdin test entry.
const (
	basePaths  = "session_db: gateway.sqlite\nsession_socket: gateway.sock\nadmin_socket: admin.sock\ninventory: inventory\n"
	baseConfig = basePaths + "credentials: {stdin_test_entry: true}\n"
)

// TestConfiguration checks which upstreams the gateway accepts: the API origin and
// loopback origins for synthetic experiments, nothing else; which listen addresses it
// accepts; and which files it refuses, naming the key.
func TestConfiguration(t *testing.T) {
	// The first address of 100.64.0.0/10, a synthetic tailnet address.
	const shared = "100.64.0.1"
	const backup = "backup:\n  endpoint: https://s3.us-east-1.amazonaws.com\n  bucket: example-backup-bucket\n  region: us-east-1\n  prefix: redcoast\n" +
		"  access_key_id: op://example-vault/backup-writer/access key id\n  secret_access_key: op://example-vault/backup-writer/secret access key\n"
	for _, test := range []struct {
		name     string
		yaml     string
		upstream string // expected when accepted
		problem  string // part of the error when refused
	}{
		{"onepassword_token", basePaths + "credentials: {onepassword_token: 'file:///run/credentials/redcoast.service/op-sa-token'}\n", "https://api.anthropic.com", ""},
		{"stdin_test_entry", baseConfig, "https://api.anthropic.com", ""},
		// Without 1Password, env:// and file:// references still resolve.
		{"no_onepassword", basePaths, "https://api.anthropic.com", ""},
		{"loopback_upstream", baseConfig + "upstream: http://127.0.0.1:8790\n", "http://127.0.0.1:8790", ""},
		{"loopback_https_upstream", baseConfig + "upstream: https://127.0.0.1:8790\n", "https://127.0.0.1:8790", ""},
		{"forward_address", baseConfig + "listen: {forward: '127.0.0.2:8791'}\n", "https://api.anthropic.com", ""},
		// Control arm for the address guards: a specific non-loopback private IP (a tailnet address) is accepted for every listener.
		{"tailnet_addresses", baseConfig + "listen: {reverse: '" + shared + ":7802', forward: '" + shared + ":7803', session: '" + shared + ":7801', health: '10.0.0.7:7804', dashboard: '[fd00::7]:7805'}\n", "https://api.anthropic.com", ""},
		{"public_address", baseConfig + "listen: {session: '203.0.113.7:7801'}\n", "", "listen.session: a public IP needs listen.allow_public: true"},
		{"public_address_allowed", baseConfig + "listen: {session: '203.0.113.7:7801', allow_public: true}\n", "https://api.anthropic.com", ""},
		{"public_wildcard_allowed", baseConfig + "listen: {session: '0.0.0.0:7801', allow_public: true}\n", "", "listen.session: must be a specific IP"},
		{"public_mapped_wildcard_allowed", baseConfig + "listen: {session: '[::ffff:0.0.0.0]:7801', allow_public: true}\n", "", "listen.session: must be a specific IP"},
		{"public_ipv6_allowed", baseConfig + "listen: {session: '[2001:db8::7]:7801', allow_public: true}\n", "https://api.anthropic.com", ""},
		{"both_credential_sources", basePaths + "credentials: {onepassword_token: 'env://OP_TOKEN', stdin_test_entry: true}\n", "", "credentials: the stdin test entry"},
		{"token_from_vault", basePaths + "credentials: {onepassword_token: 'op://example-vault/sa/token'}\n", "", "credentials.onepassword_token: must be an env:// or file://"},
		{"missing_inventory", "session_db: gateway.sqlite\nsession_socket: gateway.sock\nadmin_socket: admin.sock\n", "", "inventory: missing"},
		{"missing_admin_socket", "session_db: gateway.sqlite\nsession_socket: gateway.sock\ninventory: inventory\n", "", "admin_socket: missing"},
		{"missing_session_db", "session_socket: gateway.sock\nadmin_socket: admin.sock\ninventory: inventory\n", "", "session_db: missing"},
		{"admin_socket_is_session_socket", "session_db: gateway.sqlite\nsession_socket: gateway.sock\nadmin_socket: gateway.sock\ninventory: inventory\n", "", "admin_socket: must differ"},
		{"unknown_key", baseConfig + "op_token_file: op-sa-token\n", "", "field op_token_file not found"},
		{"unknown_nested_key", baseConfig + "listen: {reverse_proxy: '127.0.0.1:8789'}\n", "", "field reverse_proxy not found"},
		// A type error names the line, not the key.
		{"wrong_type", baseConfig + "limits: {max_tunnels: many}\n", "", "line 6: cannot unmarshal"},
		{"two_documents", baseConfig + "---\n" + baseConfig, "", "more than one YAML document"},
		{"session_listen_every_interface", baseConfig + "listen: {session: '0.0.0.0:7801'}\n", "", "listen.session: must be a specific IP"},
		{"session_listen_host_name", baseConfig + "listen: {session: 'gateway.tailnet:7801'}\n", "", "listen.session: must be one IP literal"},
		{"session_listen_is_mapped_proxy", baseConfig + "listen: {session: '[::ffff:127.0.0.1]:8789'}\n", "", "listen.session: same address as listen.reverse"},
		{"session_listen_is_proxy", baseConfig + "listen: {session: '127.0.0.1:8789'}\n", "", "listen.session: same address as listen.reverse"},
		// Control arm for the upstream guard: a host that is neither the API nor loopback.
		{"other_host", baseConfig + "upstream: https://api.example.test\n", "", "upstream: must be the API origin"},
		{"localhost_name", baseConfig + "upstream: http://localhost:8790\n", "", "upstream:"},
		{"api_with_path", baseConfig + "upstream: https://api.anthropic.com/v1\n", "", "upstream: not an origin"},
		{"api_with_port", baseConfig + "upstream: https://api.anthropic.com:8443\n", "", "upstream:"},
		{"listen_wildcard", baseConfig + "listen: {reverse: '0.0.0.0:8789'}\n", "", "listen.reverse: must be a specific IP"},
		{"forward_on_reverse_address", baseConfig + "listen: {forward: '127.0.0.1:8789'}\n", "", "listen.forward: same address as listen.reverse"},
		{"retention_days", baseConfig + "retention: {traffic_days: 90, session_days: 200}\n", "https://api.anthropic.com", ""},
		{"zero_retention", baseConfig + "retention: {traffic_days: 0}\n", "", "retention.traffic_days: must be between 1 and 36500"},
		{"longest_retention", baseConfig + "retention: {traffic_days: 36500, session_days: 36500}\n", "https://api.anthropic.com", ""},
		{"traffic_retention_overflow", baseConfig + "retention: {traffic_days: 9223372036854775807}\n", "", "retention.traffic_days"},
		{"session_retention_overflow", baseConfig + "retention: {session_days: 36501}\n", "", "retention.session_days"},
		{"backup", baseConfig + backup, "https://api.anthropic.com", ""},
		{"backup_minio", baseConfig + strings.Replace(backup, "https://s3.us-east-1.amazonaws.com", "http://10.0.0.5:9000", 1), "https://api.anthropic.com", ""},
		{"backup_missing_bucket", baseConfig + strings.Replace(backup, "  bucket: example-backup-bucket\n", "", 1), "", "backup.bucket: not an S3 bucket name"},
		{"backup_endpoint_with_path", baseConfig + strings.Replace(backup, "amazonaws.com", "amazonaws.com/other", 1), "", "backup.endpoint: must be an origin"},
		{"backup_public_http", baseConfig + strings.Replace(backup, "https://s3.us-east-1.amazonaws.com", "http://s3.example.test", 1), "", "backup.endpoint: http only"},
		{"backup_bucket_with_slash", baseConfig + strings.Replace(backup, "example-backup-bucket", "b/x", 1), "", "backup.bucket"},
		{"backup_region_with_slash", baseConfig + strings.Replace(backup, "region: us-east-1", "region: example.test/", 1), "", "backup.region"},
		{"backup_key_not_a_reference", baseConfig + strings.Replace(backup, "op://example-vault/backup-writer/access key id", "AKIDEXAMPLE", 1), "", "backup.access_key_id: not a credential reference"},
		{"backup_unknown_key", baseConfig + backup + "  path_style: true\n", "", "field path_style not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(test.yaml))
			if test.upstream == "" {
				if err == nil || !strings.Contains(err.Error(), test.problem) {
					t.Fatalf("got %v, want an error with %q", err, test.problem)
				}
				return
			}
			if err != nil || cfg.upstream.String() != test.upstream || cfg.sessionDB != "gateway.sqlite" || cfg.socket != "gateway.sock" || cfg.adminSocket != "admin.sock" || cfg.inventory != "inventory" || cfg.forwardListen == cfg.listen {
				t.Fatalf("configuration refused or changed: %+v %v", cfg, err)
			}
			wantRetention := session.Retention{Traffic: session.DefaultTrafficRetention, Sessions: session.DefaultSessionRetention}
			switch test.name {
			case "retention_days":
				wantRetention = session.Retention{Traffic: 90 * 24 * time.Hour, Sessions: 200 * 24 * time.Hour}
			case "longest_retention":
				wantRetention = session.Retention{Traffic: 36500 * 24 * time.Hour, Sessions: 36500 * 24 * time.Hour}
			}
			if cfg.retention != wantRetention {
				t.Fatalf("retention = %+v, want %+v", cfg.retention, wantRetention)
			}
			if (cfg.backup != nil) != strings.HasPrefix(test.name, "backup") {
				t.Fatalf("backup %+v", cfg.backup)
			}
		})
	}
}

// TestLoadConfig checks that --config is the only argument and that the file is read.
func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(baseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig([]string{"--config", path}); err != nil || cfg.sessionDB != "gateway.sqlite" {
		t.Fatalf("got %+v %v", cfg, err)
	}
	for _, arguments := range [][]string{nil, {"--config", path, "--listen", "127.0.0.1:8789"}, {"--config", path, "extra"}, {"--config", path + ".missing"}} {
		if _, err := loadConfig(arguments); err == nil {
			t.Errorf("%q accepted", arguments)
		}
	}
}

// TestOpenStoreRefusesCorruptFile starts on an intact file and refuses one whose data
// page was overwritten. Opening alone reads only the header, so the refusal comes from
// quick_check.
func TestOpenStoreRefusesCorruptFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.sqlite")
	store, err := openStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if err := store.RecordTraffic(ctx, session.Traffic{At: time.Now(), Kind: "connect", Host: "example.invalid", Port: 443, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Control arm: the same file, intact, opens.
	store, err = openStore(ctx, path)
	if err != nil {
		t.Fatalf("intact file refused: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("the WAL should be checkpointed into the file on close: %v", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	const pageSize = 4096
	if _, err := file.WriteAt([]byte(strings.Repeat("\xa5", pageSize)), 4*pageSize); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := openStore(ctx, path); err == nil {
		_ = store.Close()
		t.Fatal("a corrupt file was accepted")
	} else if !strings.Contains(err.Error(), "quick_check") {
		t.Fatalf("refused for another reason: %v", err)
	}
}

// TestExampleConfig checks that the example file parses and that the keys it says show
// their default do.
func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../redcoast.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := parseConfig([]byte(basePaths))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.limits != defaults.limits || cfg.accountConcurrency != defaults.accountConcurrency || cfg.retention != defaults.retention ||
		cfg.egressInterval != defaults.egressInterval || cfg.listen != defaults.listen || cfg.forwardListen != defaults.forwardListen ||
		cfg.healthListen != defaults.healthListen || cfg.dashboardListen != defaults.dashboardListen || cfg.upstream.String() != defaults.upstream.String() {
		t.Fatalf("example %+v differs from the defaults %+v", cfg, defaults)
	}
	if cfg.backup == nil || cfg.tokenRef == "" {
		t.Fatal("the example's backup or token is missing")
	}
}

// TestNewBackup takes no backups without a backup section and resolves the writer's key
// from the two references with one.
func TestNewBackup(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, filepath.Join(t.TempDir(), "gateway.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var asked []string
	resolve := func(_ context.Context, reference string) (string, error) {
		asked = append(asked, reference)
		return "value", nil
	}
	if b, err := newBackup(ctx, config{}, store, resolve); err != nil || b != nil || len(asked) != 0 {
		t.Fatalf("without a backup: backup=%v err=%v asked=%v", b, err, asked)
	}
	section := &backupConfig{Endpoint: "https://s3.us-east-1.amazonaws.com", Bucket: "bucket", Region: "us-east-1", Prefix: "redcoast", AccessKeyID: "env://KEY_ID", SecretAccessKey: "file:///run/secret"}
	b, err := newBackup(ctx, config{backup: section, sessionDB: "gateway.sqlite"}, store, resolve)
	if err != nil || b == nil || len(asked) != 2 || asked[0] != "env://KEY_ID" || asked[1] != "file:///run/secret" {
		t.Fatalf("with a backup: backup=%v err=%v asked=%v", b, err, asked)
	}
	failing := func(context.Context, string) (string, error) { return "", errors.New("not in the vault") }
	if _, err := newBackup(ctx, config{backup: section}, store, failing); err == nil {
		t.Fatal("an unresolved key was accepted")
	}
}

// TestBucketURL checks the path-style bucket URL for AWS and for S3-compatible services.
func TestBucketURL(t *testing.T) {
	for _, test := range []struct{ endpoint, bucket, want string }{
		{"https://s3.us-east-1.amazonaws.com", "example-backup-bucket", "https://s3.us-east-1.amazonaws.com/example-backup-bucket"},
		{"https://0123456789abcdef.r2.cloudflarestorage.com/", "gateway", "https://0123456789abcdef.r2.cloudflarestorage.com/gateway"},
		{"http://10.0.0.5:9000", "gateway.backups", "http://10.0.0.5:9000/gateway.backups"},
	} {
		if got := bucketURL(test.endpoint, test.bucket); got != test.want {
			t.Errorf("%s %s: got %s, want %s", test.endpoint, test.bucket, got, test.want)
		}
	}
}

// TestClaudeCommand checks that "claude" and a known command lead every command line.
func TestClaudeCommand(t *testing.T) {
	for _, arguments := range [][]string{{"claude", "serve", "--config", "x"}, {"claude", "plan"}, {"claude", "machine", "list"}} {
		if command, err := claudeCommand(arguments); err != nil || !slices.Equal(command, arguments[1:]) {
			t.Errorf("%q: got %q %v", arguments, command, err)
		}
	}
	for _, arguments := range [][]string{nil, {"claude"}, {"serve", "--config", "x"}, {"--config", "x"}, {"claude", "--config", "x"}, {"codex", "serve"}} {
		if _, err := claudeCommand(arguments); !errors.Is(err, errUsage) {
			t.Errorf("%q: got %v, want the usage", arguments, err)
		}
	}
}
