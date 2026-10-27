package launcher

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
)

const keyHeader = "X-Teo-Gateway-Key"

type Plan struct {
	Executable string
	Args       []string
	Env        []string
	// Claude settings are written to a private temporary file, never argv.
	Settings map[string]any
}

// BuildPlan never reads the native clients' credential stores. Their OAuth
// tokens and refresh logic stay entirely within the original clients.
func BuildPlan(tool string, args []string, cfg Config, key string, environ []string) (Plan, error) {
	if err := cfg.Validate(); err != nil {
		return Plan{}, err
	}
	base := strings.TrimRight(cfg.GatewayURL, "/")
	env := envMap(environ)
	// Credentials can be present in a developer's shell for unrelated API use.
	// Clear them only in the child; never alter the user's shell or native files.
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL", "OPENAI_ACCESS_TOKEN",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"} {
		delete(env, name)
	}
	p := Plan{Executable: tool}
	switch tool {
	case "codex":
		env["TEO_AI_GATEWAY_KEY"] = key
		p.Args = []string{
			"--no-daemon",
			"-c", `forced_login_method="chatgpt"`,
			"-c", `model_provider="teo_ai"`,
			"-c", `model_providers.teo_ai.name="teo-ai"`,
			"-c", `model_providers.teo_ai.base_url=` + strconv.Quote(base+"/codex"),
			"-c", `model_providers.teo_ai.wire_api="responses"`,
			"-c", `model_providers.teo_ai.requires_openai_auth=true`,
			"-c", `model_providers.teo_ai.supports_websockets=false`,
			"-c", `model_providers.teo_ai.env_http_headers={"` + keyHeader + `"="TEO_AI_GATEWAY_KEY"}`,
		}
	case "claude":
		headers, err := mergeHeaders(env["ANTHROPIC_CUSTOM_HEADERS"], key)
		if err != nil {
			return Plan{}, err
		}
		settingsEnv := map[string]string{
			"ANTHROPIC_BASE_URL":       base + "/anthropic",
			"ANTHROPIC_CUSTOM_HEADERS": headers,
			"ANTHROPIC_API_KEY":        "", "ANTHROPIC_AUTH_TOKEN": "", "CLAUDE_CODE_OAUTH_TOKEN": "",
			"CLAUDE_CODE_USE_BEDROCK": "0", "CLAUDE_CODE_USE_VERTEX": "0",
			"CLAUDE_CODE_USE_FOUNDRY": "0", "CLAUDE_CODE_USE_MANTLE": "0",
		}
		for name, value := range settingsEnv {
			env[name] = value
		}
		p.Settings = map[string]any{"forceLoginMethod": "claudeai", "apiKeyHelper": "", "env": settingsEnv}
	default:
		return Plan{}, errors.New("tool must be codex or claude")
	}
	p.Args = append(p.Args, args...)
	p.Env = flattenEnv(env)
	return p, nil
}

// Reject headers which could override provider credentials, the upstream host,
// or the gateway's own identity. Preserve harmless user tracing headers.
func mergeHeaders(raw, key string) (string, error) {
	var headers []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok || strings.ContainsAny(line, "\r") {
			return "", errors.New("invalid ANTHROPIC_CUSTOM_HEADERS")
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "authorization", "x-api-key", "host", "cookie", "connection", "proxy-authorization":
			return "", errors.New("ANTHROPIC_CUSTOM_HEADERS must not override credentials, host, or connection")
		case strings.ToLower(keyHeader):
			continue
		}
		headers = append(headers, line)
	}
	return strings.Join(append(headers, keyHeader+": "+key), "\n"), nil
}

func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, entry := range environ {
		if name, value, ok := strings.Cut(entry, "="); ok {
			m[name] = value
		}
	}
	return m
}

func flattenEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(m))
	for _, key := range keys {
		result = append(result, key+"="+m[key])
	}
	return result
}

func writeSettings(settings map[string]any) ([]byte, error) {
	return json.Marshal(settings)
}
