package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luolc/redcoast/session"
)

// piLogins are the auth.json entries that pi would use instead of the session
// credential, or that would send the person's own login elsewhere.
var piLogins = []string{"openai-codex", "openai", "radius"}

// piDir is the agent directory the launcher gives pi, holding no login.
func piDir(home string) string {
	return filepath.Join(home, ".config", "pi")
}

// piPreflight returns as overrides a login in the agent directory's auth.json, which
// pi prefers to models.json, and --api-key, which it prefers to both. Only key names
// and the file's path are reported.
func piPreflight(_ []string, _, home string, args []string) (overrides, conflicts []string) {
	path := filepath.Join(piDir(home), "auth.json")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		overrides = append(overrides, path+" cannot be read")
	default:
		var logins map[string]json.RawMessage
		if json.Unmarshal(data, &logins) != nil {
			overrides = append(overrides, path+" is not a JSON object")
		}
		for _, key := range piLogins {
			if _, present := logins[key]; present {
				overrides = append(overrides, key+" login in "+path)
			}
		}
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--api-key" || strings.HasPrefix(arg, "--api-key=") {
			overrides = append(overrides, "--api-key on the command line")
		}
	}
	return overrides, nil
}

// preparePi writes the agent directory's models.json, which points pi's built-in
// openai-codex provider at the gateway and names the variable holding the session
// credential, and returns pi's environment: the agent directory, PI_OFFLINE, the
// credential and the forward proxy. The credential itself stays out of the file, so
// that pi sessions started at once from the same directory each keep their own.
func preparePi(l *launcher, grant session.Grant) ([]string, []string, error) {
	origin, err := gatewayOrigin(grant)
	if err != nil {
		return nil, nil, err
	}
	dir, err := dedicatedDir(piDir(l.home))
	if err != nil {
		return nil, nil, err
	}
	models := map[string]any{"providers": map[string]any{"openai-codex": map[string]string{
		"baseUrl": origin + "/backend-api",
		"apiKey":  "${" + gatewayTokenVariable + "}",
	}}}
	// Encoding maps of strings cannot fail.
	data, _ := json.MarshalIndent(models, "", "  ")
	if err := replaceFile(filepath.Join(dir, "models.json"), append(data, '\n')); err != nil {
		return nil, nil, err
	}
	env := codexEnvironment(l.environ, grant, origin, []string{
		"PI_CODING_AGENT_DIR=" + dir,
		"PI_OFFLINE=1",
		gatewayTokenVariable + "=" + grant.Token,
	})
	return env, l.args, nil
}

// replaceFile writes data to path through a temporary file in the same directory, so
// that a pi starting at the same moment reads either the old file or the new one.
func replaceFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
