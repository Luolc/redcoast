package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luolc/redcoast/session"
)

// gatewayTokenVariable carries the session credential to pi and codex, whose
// configuration names it instead of holding the credential.
const gatewayTokenVariable = "CODEX_GATEWAY_TOKEN"

// codexProvider is the id of the model provider the launcher defines for codex.
const codexProvider = "gateway"

// codexSettings are the switches the launcher passes to codex with -c, each at its
// value as TOML. -c outranks every configuration file but the managed ones, so a
// trusted project's .codex/config.toml cannot turn one back on. Shell snapshots would
// write the environment, the session credential included, into the CODEX_HOME.
// Storing logins in a file keeps a login made through the launcher where
// codexPreflight sees it.
var codexSettings = []struct{ key, value string }{
	{"features.plugins", "false"},
	{"features.shell_snapshot", "false"},
	{"analytics.enabled", "false"},
	{"otel.metrics_exporter", `"none"`},
	{"check_for_update_on_startup", "false"},
	{"feedback.enabled", "false"},
	{"cli_auth_credentials_store", `"file"`},
}

// codexManagedFiles are configuration layers that outrank -c; the launcher refuses
// to start codex while one exists rather than predict what it sets.
func codexManagedFiles(home string) []string {
	return []string{"/etc/codex/managed_config.toml", "/etc/codex/requirements.toml", filepath.Join(codexDir(home), "managed_config.toml")}
}

// codexDir is the CODEX_HOME the launcher gives codex, holding no login.
func codexDir(home string) string {
	return filepath.Join(home, ".config", "codex")
}

// codexPreflight returns as overrides a login in the CODEX_HOME, a managed
// configuration file, and the options that choose another model provider or a remote
// app server; as conflicts, each -c or --enable that sets one of codexSettings to
// another value. Values are shown only for those switches.
func codexPreflight(_ []string, _, home string, args []string) (overrides, conflicts []string) {
	login := filepath.Join(codexDir(home), "auth.json")
	if _, err := os.Lstat(login); !errors.Is(err, os.ErrNotExist) {
		overrides = append(overrides, "a login in "+login)
	}
	for _, path := range codexManagedFiles(home) {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			overrides = append(overrides, "managed configuration "+path)
		}
	}
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		name, value, found := splitOption(args[i])
		if !found && codexValueOptions[name] && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch name {
		case "-c", "--config":
			o, c := codexOverrideProblems(value)
			overrides, conflicts = append(overrides, o...), append(conflicts, c...)
		case "--enable":
			// --enable X is -c features.X=true.
			feature := strings.TrimSpace(value)
			for _, setting := range codexSettings {
				if setting.key == "features."+feature {
					conflicts = append(conflicts, "--enable "+feature+" on the command line; the launcher sets "+setting.key+"="+setting.value)
				}
			}
		case "--oss", "--local-provider", "--remote", "--remote-auth-token-env":
			overrides = append(overrides, name+" on the command line")
		}
	}
	return overrides, conflicts
}

// codexValueOptions are the options codexPreflight reads that take a value, which may
// be the next argument.
var codexValueOptions = map[string]bool{"-c": true, "--config": true, "--enable": true, "--local-provider": true, "--remote": true, "--remote-auth-token-env": true}

// splitOption splits "--name=value", "-cvalue" and "-c=value" into the option's name
// and its value; any other argument is its own name, without a value.
func splitOption(arg string) (name, value string, found bool) {
	switch {
	case strings.HasPrefix(arg, "--"):
		return strings.Cut(arg, "=")
	case strings.HasPrefix(arg, "-c") && len(arg) > 2:
		return "-c", strings.TrimPrefix(arg[2:], "="), true
	}
	return arg, "", false
}

// codexOverrideProblems checks one -c key=value: a key under model_provider or
// model_providers is an override; one of codexSettings at another value, or a table
// that contains one, is a conflict. Quotes and spaces in the key are ignored, as in
// TOML's quoted keys.
func codexOverrideProblems(override string) (overrides, conflicts []string) {
	rawKey, value, _ := strings.Cut(override, "=")
	key := strings.NewReplacer(`"`, "", "'", "", " ", "").Replace(rawKey)
	if key == "model_provider" || key == "model_providers" || strings.HasPrefix(key, "model_providers.") {
		return []string{"-c " + key + " on the command line"}, nil
	}
	for _, setting := range codexSettings {
		if key == setting.key && unquote(value) == unquote(setting.value) {
			continue
		}
		if key == setting.key || strings.HasPrefix(setting.key, key+".") {
			// Encoding a string cannot fail.
			shown, _ := json.Marshal(strings.TrimSpace(value))
			conflicts = append(conflicts, "-c "+key+"="+settingsValue(shown)+" on the command line; the launcher sets "+setting.key+"="+setting.value)
		}
	}
	return nil, conflicts
}

// unquote trims the spaces and quotes that codex itself trims from a -c value it
// cannot parse as TOML.
func unquote(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"'`)
}

// prepareCodex returns codex's environment, with its CODEX_HOME, the session credential
// and the forward proxy, and its arguments: --no-daemon, so that no shared background
// server outlives this session with its credential, the provider and codexSettings
// as -c, then the arguments the launcher got.
func prepareCodex(l *launcher, grant session.Grant) ([]string, []string, error) {
	origin, err := gatewayOrigin(grant)
	if err != nil {
		return nil, nil, err
	}
	dir, err := dedicatedDir(codexDir(l.home))
	if err != nil {
		return nil, nil, err
	}
	provider := "model_providers." + codexProvider + "."
	settings := []string{
		`model_provider="` + codexProvider + `"`,
		provider + `name="` + codexProvider + `"`,
		provider + `base_url="` + origin + `/backend-api/codex"`,
		provider + `wire_api="responses"`,
		provider + `env_key="` + gatewayTokenVariable + `"`,
		provider + "supports_websockets=false",
	}
	for _, setting := range codexSettings {
		settings = append(settings, setting.key+"="+setting.value)
	}
	args := []string{"--no-daemon"}
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	env := codexEnvironment(l.environ, grant, origin, []string{"CODEX_HOME=" + dir, gatewayTokenVariable + "=" + grant.Token})
	return env, append(args, l.args...), nil
}

// gatewayOrigin returns the grant's reverse proxy as an http or https origin of
// printable ASCII without quotes or backslashes, which can be written into a JSON or
// TOML string as it is.
func gatewayOrigin(grant session.Grant) (string, error) {
	reverse, err := url.Parse(grant.ReverseProxy)
	if err != nil || (reverse.Scheme != "http" && reverse.Scheme != "https") || reverse.Host == "" || reverse.User != nil || strings.Trim(reverse.Path, "/") != "" || reverse.RawQuery != "" {
		return "", errors.New("the gateway's reverse proxy is not an http or https origin")
	}
	origin := reverse.Scheme + "://" + reverse.Host
	for _, r := range origin {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
			return "", errors.New("the gateway's reverse proxy is not an http or https origin")
		}
	}
	return origin, nil
}

// codexEnvironment is the environment for pi and codex: the parent's, minus the proxy
// variables and those set assigns, plus the forward proxy, set, and NO_PROXY for the
// gateway's host and loopback, which the clients reach for their own local servers.
func codexEnvironment(parent []string, grant session.Grant, origin string, set []string) []string {
	// gatewayOrigin has parsed origin already.
	reverse, _ := url.Parse(origin)
	hosts := []string{reverse.Hostname()}
	for _, host := range []string{"127.0.0.1", "localhost"} {
		if hosts[0] != host {
			hosts = append(hosts, host)
		}
	}
	return proxiedEnvironment(parent, grant, strings.Join(hosts, ","), set)
}

// dedicatedDir creates dir, private to its owner, when it is missing and returns it. An
// existing dir must be private too: it holds the clients' session logs and history.
func dedicatedDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("configuration directory: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("configuration directory: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("configuration directory %s is accessible by others; make it mode 0700", dir)
	}
	return dir, nil
}
