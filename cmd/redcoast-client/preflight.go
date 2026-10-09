package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// overridingVariables outrank CLAUDE_CODE_OAUTH_TOKEN in claude's credential
// precedence; present in the environment or in a settings env map, the session
// credential is not used.
var overridingVariables = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"}

// forbiddenSettingsEnv lists the variables a settings env map must not set: the
// overriding ones, the ones the launcher sets, and every proxy variable it clears.
func forbiddenSettingsEnv() []string {
	seen := make(map[string]bool)
	var keys []string
	for _, group := range [][]string{overridingVariables, gatewayVariables, clearedProxyVariables} {
		for _, key := range group {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return keys
}

// overridingSettings are settings keys that select another credential or login.
var overridingSettings = []string{"apiKeyHelper", "forceLoginMethod", "forceLoginGatewayUrl"}

// preflight returns everything that would stop the session credential from being
// used: a configuration directory that has not completed onboarding, an overriding
// variable in the environment, or an overriding key or variable in any settings layer
// (managed, user, project, local, and files or JSON given with --settings). A settings
// env map may also not set any proxy variable: claude would apply it over the
// launcher's HTTPS_PROXY. Only the presence of keys is examined; no value is read, and
// nothing is reported verbatim except key names and file paths.
func preflight(environ []string, cwd, home string, args []string) []string {
	env := environment(environ)
	var problems []string
	config := configDir(env, home)
	if err := onboardingCompleted(stateFile(env, home)); err != nil {
		problems = append(problems, err.Error())
	}
	for _, key := range overridingVariables {
		if _, present := env[key]; present {
			problems = append(problems, key+" in the environment")
		}
	}
	for _, layer := range settingsLayers(config, cwd, args) {
		problems = append(problems, layer.problems()...)
	}
	return problems
}

// stateFile is claude's own state file with the onboarding flag: ~/.claude.json, or
// .claude.json inside CLAUDE_CONFIG_DIR when that is set.
func stateFile(env map[string]string, home string) string {
	if dir := env["CLAUDE_CONFIG_DIR"]; dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// onboardingCompleted checks that path has hasCompletedOnboarding set to true; the
// interactive UI otherwise stops at the login choice and ignores the credential.
func onboardingCompleted(path string) error {
	var state struct {
		HasCompletedOnboarding bool `json:"hasCompletedOnboarding"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &state) != nil || !state.HasCompletedOnboarding {
		return errors.New("hasCompletedOnboarding is not true in " + path)
	}
	return nil
}

// settingsLayer is one settings source: a file, or inline JSON from --settings.
type settingsLayer struct {
	name   string
	path   string // read when inline is empty
	inline string
}

// settingsLayers lists the layers claude would read for a session started in cwd.
func settingsLayers(config, cwd string, args []string) []settingsLayer {
	managedDir := "/etc/claude-code"
	if runtime.GOOS == "darwin" {
		managedDir = "/Library/Application Support/ClaudeCode"
	}
	layers := []settingsLayer{{name: "managed settings", path: filepath.Join(managedDir, "managed-settings.json")}}
	dropIns, _ := filepath.Glob(filepath.Join(managedDir, "managed-settings.d", "*.json"))
	sort.Strings(dropIns)
	for _, path := range dropIns {
		layers = append(layers, settingsLayer{name: "managed settings", path: path})
	}
	layers = append(layers,
		settingsLayer{name: "user settings", path: filepath.Join(config, "settings.json")},
		settingsLayer{name: "project settings", path: filepath.Join(cwd, ".claude", "settings.json")},
	)
	for _, root := range localSettingsRoots(cwd) {
		layers = append(layers, settingsLayer{name: "local settings", path: filepath.Join(root, ".claude", "settings.local.json")})
	}
	for i := 0; i < len(args); i++ {
		value, found := strings.CutPrefix(args[i], "--settings=")
		if !found && args[i] == "--settings" && i+1 < len(args) {
			i++
			value, found = args[i], true
		}
		if !found {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(value), "{") {
			layers = append(layers, settingsLayer{name: "--settings", inline: value})
		} else {
			layers = append(layers, settingsLayer{name: "--settings", path: value})
		}
	}
	return layers
}

// localSettingsRoots lists the directories whose .claude/settings.local.json claude may
// read for a session in cwd: cwd itself; in a git repository its root, where claude
// keeps the local file when started from a subdirectory; and for a worktree the main
// checkout's root. All of them are checked, since which one claude picks depends on
// conditions (ownership, home directory) the launcher does not reproduce.
func localSettingsRoots(cwd string) []string {
	roots := []string{cwd}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			if dir != cwd {
				roots = append(roots, dir)
			}
			if !info.IsDir() {
				if main := mainCheckout(filepath.Join(dir, ".git")); main != "" && main != dir {
					roots = append(roots, main)
				}
			}
			return roots
		}
		if filepath.Dir(dir) == dir {
			return roots
		}
	}
}

// mainCheckout returns the main checkout's root for a worktree's .git file, which
// holds "gitdir: <main>/.git/worktrees/<name>", or "" when it holds something else.
func mainCheckout(gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir, found := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !found {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	worktrees := filepath.Dir(gitdir) // <main>/.git/worktrees
	if filepath.Base(worktrees) != "worktrees" || filepath.Base(filepath.Dir(worktrees)) != ".git" {
		return ""
	}
	return filepath.Dir(filepath.Dir(worktrees))
}

// problems reports the overriding keys and env variables in the layer. A missing file
// is fine; a file that exists but is not a JSON object is reported, since claude's
// reading of it cannot be predicted.
func (l settingsLayer) problems() []string {
	data := []byte(l.inline)
	where := l.name
	if l.inline == "" {
		var err error
		data, err = os.ReadFile(l.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		where = l.name + " " + l.path
		if err != nil {
			return []string{where + " cannot be read"}
		}
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return []string{where + " is not a JSON object"}
	}
	var problems []string
	for _, key := range overridingSettings {
		if _, present := keys[key]; present {
			problems = append(problems, key+" in "+where)
		}
	}
	var env map[string]json.RawMessage
	if raw, present := keys["env"]; present && json.Unmarshal(raw, &env) == nil {
		for _, key := range forbiddenSettingsEnv() {
			if _, present := env[key]; present {
				problems = append(problems, "env."+key+" in "+where)
			}
		}
	}
	return problems
}
