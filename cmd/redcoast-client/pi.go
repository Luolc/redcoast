package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Luolc/redcoast/session"
)

// piProviderVariables are the variables from which Pi 1.2.0 takes a credential for a
// provider other than openai-codex (packages/ai/src/env-api-keys.ts and
// api/anthropic-messages.ts). Without them, and with no login in auth.json,
// openai-codex is the only provider with a credential, and pi falls back to it for a
// project's defaultProvider or a resumed session's model, which it uses only when
// their provider has one.
var piProviderVariables = []string{
	"AI_GATEWAY_API_KEY", "ANT_LING_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_FEDERATION_RULE_ID",
	"ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID",
	"ANTHROPIC_WORKSPACE_ID", "AWS_ACCESS_KEY_ID", "AWS_BEARER_TOKEN_BEDROCK", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_PROFILE", "AWS_SECRET_ACCESS_KEY", "AWS_WEB_IDENTITY_TOKEN_FILE",
	"AZURE_OPENAI_API_KEY", "BASETEN_API_KEY", "CEREBRAS_API_KEY", "CLOUDFLARE_API_KEY", "COPILOT_GITHUB_TOKEN",
	"DEEPSEEK_API_KEY", "FIREWORKS_API_KEY", "GCLOUD_PROJECT", "GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
	"GOOGLE_CLOUD_API_KEY", "GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_PROJECT", "GROQ_API_KEY", "HF_TOKEN", "KIMI_API_KEY",
	"META_API_KEY", "MINIMAX_API_KEY", "MINIMAX_CN_API_KEY", "MISTRAL_API_KEY", "MOONSHOT_API_KEY", "NVIDIA_API_KEY",
	"OPENAI_API_KEY", "OPENCODE_API_KEY", "OPENROUTER_API_KEY", "QWEN_TOKEN_PLAN_API_KEY", "QWEN_TOKEN_PLAN_CN_API_KEY",
	"RADIUS_API_KEY", "TOGETHER_API_KEY", "TYPESAFE_API_KEY", "XAI_API_KEY", "XIAOMI_API_KEY",
	"XIAOMI_TOKEN_PLAN_AMS_API_KEY", "XIAOMI_TOKEN_PLAN_CN_API_KEY", "XIAOMI_TOKEN_PLAN_SGP_API_KEY", "ZAI_API_KEY",
	"ZAI_CODING_CN_API_KEY",
}

// piProvider is the provider pi must use: its built-in one for ChatGPT subscriptions.
const piProvider = "openai-codex"

// piDir is the agent directory the launcher gives pi, holding no login.
func piDir(home string) string {
	return filepath.Join(home, ".config", "pi")
}

// piPreflight returns as overrides any login in the agent directory's auth.json,
// which pi prefers to models.json for openai-codex and which would give another
// provider a credential, --api-key, which pi prefers to both, and a --provider or a
// --model that names another provider. Of auth.json only key names and the file's
// path are reported.
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
		keys := slices.Sorted(maps.Keys(logins))
		for _, key := range keys {
			overrides = append(overrides, strconv.QuoteToASCII(key)+" login in "+path)
		}
	}
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		name, value, found := strings.Cut(args[i], "=")
		if !found && (name == "--provider" || name == "--model") && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch {
		case name == "--api-key":
			overrides = append(overrides, "--api-key on the command line")
		case name == "--provider" && !strings.EqualFold(value, piProvider):
			overrides = append(overrides, "--provider for another provider than "+piProvider+" on the command line")
		case name == "--model" && strings.Contains(value, "/") && !strings.HasPrefix(strings.ToLower(value), piProvider+"/"):
			overrides = append(overrides, "--model for another provider than "+piProvider+" on the command line")
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
	env := codexEnvironment(withoutVariables(l.environ, piProviderVariables), grant, origin, []string{
		"PI_CODING_AGENT_DIR=" + dir,
		"PI_OFFLINE=1",
		gatewayTokenVariable + "=" + grant.Token,
	})
	return env, l.args, nil
}

// withoutVariables returns environ without the variables named in keys.
func withoutVariables(environ, keys []string) []string {
	var kept []string
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(keys, key) {
			kept = append(kept, entry)
		}
	}
	return kept
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
