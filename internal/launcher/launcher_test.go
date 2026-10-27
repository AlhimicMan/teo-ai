package launcher

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "gateway-key-012345678901234567890123456789"

func TestCodexPlanUsesNativeChatGPTAuth(t *testing.T) {
	cfg := Config{GatewayURL: "https://ai.example.com", KeyFile: "/key"}
	native := []string{"exec", "a prompt with spaces", "--model", "native-model"}
	p, err := BuildPlan("codex", native, cfg, testKey, []string{"PATH=/bin", "OPENAI_API_KEY=api-secret", "CODEX_API_KEY=api-secret", "OPENAI_BASE_URL=https://wrong"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(p.Args, "\n")
	for _, want := range []string{`forced_login_method="chatgpt"`, `model_providers.teo_ai.requires_openai_auth=true`, `https://ai.example.com/codex`, `supports_websockets=false`, `TEO_AI_GATEWAY_KEY`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(joined, testKey) {
		t.Fatal("key leaked into argv")
	}
	env := envMap(p.Env)
	if env["TEO_AI_GATEWAY_KEY"] != testKey || env["OPENAI_API_KEY"] != "" || env["CODEX_API_KEY"] != "" || env["OPENAI_BASE_URL"] != "" {
		t.Fatal("incorrect child credentials")
	}
	if p.Args[len(p.Args)-len(native)+1] != native[1] {
		t.Fatal("prompt argument split")
	}
}

func TestClaudePlanPreservesSubscriptionAndOverridesRoutingSettings(t *testing.T) {
	p, err := BuildPlan("claude", []string{"--resume"}, Config{GatewayURL: "https://ai.example.com", KeyFile: "/key"}, testKey, []string{
		"ANTHROPIC_API_KEY=api-secret", "ANTHROPIC_AUTH_TOKEN=other-token", "CLAUDE_CODE_OAUTH_TOKEN=external-token", "ANTHROPIC_CUSTOM_HEADERS=X-Trace: hello\nX-Teo-Gateway-Key: old", "CLAUDE_CODE_USE_BEDROCK=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := envMap(p.Env)
	if env["ANTHROPIC_API_KEY"] != "" || env["ANTHROPIC_AUTH_TOKEN"] != "" || env["CLAUDE_CODE_OAUTH_TOKEN"] != "" || env["CLAUDE_CODE_USE_BEDROCK"] != "0" {
		t.Fatal("auth conflict not cleared")
	}
	if env["ANTHROPIC_BASE_URL"] != "https://ai.example.com/anthropic" {
		t.Fatal("incorrect route")
	}
	if env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Trace: hello\nX-Teo-Gateway-Key: "+testKey {
		t.Fatal("incorrect custom headers")
	}
	if p.Settings["forceLoginMethod"] != "claudeai" || p.Settings["apiKeyHelper"] != "" {
		t.Fatal("subscription settings missing")
	}
	if strings.Contains(strings.Join(p.Args, " "), testKey) {
		t.Fatal("key leaked into argv")
	}
}

func TestRejectCredentialHeaders(t *testing.T) {
	for _, raw := range []string{"Authorization: Bearer stolen", "x-api-key: secret", "Host: wrong", "Cookie: secret", "Connection: Authorization", "Bad Header"} {
		if _, err := mergeHeaders(raw, testKey); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestConfigValidationAndRoundtrip(t *testing.T) {
	for _, url := range []string{"https://ai.example.com", "https://ai.example.com/", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if err := (Config{GatewayURL: url, KeyFile: "/key"}).Validate(); err != nil {
			t.Errorf("%s: %v", url, err)
		}
	}
	for _, url := range []string{"", "http://remote.example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?key=secret", "https://example.com#fragment", "file:///tmp/key"} {
		if err := (Config{GatewayURL: url, KeyFile: "/key"}).Validate(); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Config{GatewayURL: "https://example.com", KeyFile: "/path/key"}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || got != cfg {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config is not private")
	}
	if err := os.WriteFile(path, []byte(`{"gateway_url":"x","unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("unknown fields accepted")
	}
}

func TestPrivateClaudeSettingsRemovedAndExitStatusPreserved(t *testing.T) {
	dir := t.TempDir()
	// A fake native client records argv and copies its temporary settings so the
	// test can inspect exactly what a real spawned process would receive.
	script := "#!/bin/sh\nprintf '%s' \"$2\" > \"$TEST_RECORD/path\"\ncp \"$2\" \"$TEST_RECORD/settings.json\"\nprintf '%s' \"$4\"\nexit 23\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := BuildPlan("claude", []string{"-p", "prompt with spaces"}, Config{GatewayURL: "https://example.com", KeyFile: "/key"}, testKey, append(os.Environ(), "TEST_RECORD="+dir))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = execute(p, strings.NewReader(""), &out, &out)
	if exit, ok := err.(*childExit); !ok || exit.code != 23 {
		t.Fatalf("exit status: %v", err)
	}
	if out.String() != "prompt with spaces" {
		t.Fatalf("prompt not preserved: %q", out.String())
	}
	path, _ := os.ReadFile(filepath.Join(dir, "path"))
	if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
		t.Fatal("temporary settings not removed")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), testKey) || settings["forceLoginMethod"] != "claudeai" {
		t.Fatal("missing runtime settings")
	}
	info, _ := os.Stat(filepath.Join(dir, "settings.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("settings mode was not private")
	}
}

func TestCLIInitAndNativeArgumentForwarding(t *testing.T) {
	dir := t.TempDir()
	keyPath, configPath := filepath.Join(dir, "key"), filepath.Join(dir, "config.json")
	if err := os.WriteFile(keyPath, []byte(testKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Run([]string{"--config", configPath, "init", "--gateway", "https://example.com", "--key-file", keyPath}, strings.NewReader(""), &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	data, _ := os.ReadFile(configPath)
	if strings.Contains(string(data), testKey) {
		t.Fatal("credential stored in config")
	}
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done\nexit 17\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out.Reset()
	code := Run([]string{"--config", configPath, "codex", "--", "exec", "prompt with spaces"}, strings.NewReader(""), &out, &out)
	if code != 17 || !strings.HasSuffix(out.String(), "exec\nprompt with spaces\n") {
		t.Fatalf("code=%d output=%q", code, out.String())
	}
}

func TestLoginUsesNativeClientsWithoutGatewayCredential(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		p, err := loginPlan([]string{tool}, []string{"TEO_AI_GATEWAY_KEY=" + testKey, "ANTHROPIC_AUTH_TOKEN=secret", "OPENAI_API_KEY=secret"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(p.Env, "\n"), "secret") || strings.Contains(strings.Join(p.Env, "\n"), testKey) {
			t.Fatal("login retained gateway or API credentials")
		}
	}
	if _, err := loginPlan([]string{"codex", "--with-api-key"}, nil); err == nil {
		t.Fatal("API-key login accepted")
	}
	if _, err := loginPlan([]string{"claude", "--console"}, nil); err == nil {
		t.Fatal("Console login accepted")
	}
}
