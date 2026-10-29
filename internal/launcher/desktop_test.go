package launcher

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Subprocess fixtures exercise the real JSON-RPC pipes and GUI lifetime, without
// touching a developer's config or starting a real app/subscription request.
func TestDesktopBackendProcess(t *testing.T) {
	if os.Getenv("TEO_TEST_BACKEND") != "1" {
		return
	}
	if os.Getenv("TEO_AI_GATEWAY_KEY") != "" {
		os.Exit(8)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				FilePath string `json:"filePath"`
				Edits    []struct {
					KeyPath string          `json:"keyPath"`
					Value   json.RawMessage `json:"value"`
				} `json:"edits"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(9)
		}
		switch request.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{}})
		case "config/batchWrite":
			if os.Getenv("TEO_TEST_BACKEND_FAIL") == "1" {
				_ = encoder.Encode(map[string]any{"id": request.ID, "error": map[string]string{"message": "private config must not be printed"}})
				continue
			}
			if filepath.Dir(request.Params.FilePath) != os.Getenv("CODEX_HOME") || len(request.Params.Edits) != 3 {
				os.Exit(10)
			}
			var staged map[string]any = make(map[string]any)
			for _, edit := range request.Params.Edits {
				var value any
				_ = json.Unmarshal(edit.Value, &value)
				staged[edit.KeyPath] = value
			}
			data, _ := json.Marshal(staged)
			if err := os.WriteFile(request.Params.FilePath, data, 0600); err != nil {
				os.Exit(11)
			}
			_ = encoder.Encode(map[string]any{"method": "config/notification"})
			_ = encoder.Encode(map[string]any{"id": request.ID, "result": map[string]string{"status": "ok"}})
		}
	}
	os.Exit(0)
}

func TestDesktopAppProcess(t *testing.T) {
	if os.Getenv("TEO_TEST_APP") != "1" {
		return
	}
	data, err := os.ReadFile(os.Getenv("TEO_TEST_CONFIG"))
	if err != nil || bytes.Contains(data, []byte(testKey)) || os.Getenv("TEO_AI_GATEWAY_KEY") != testKey || os.Getenv("OPENAI_API_KEY") != "" {
		os.Exit(12)
	}
	var staged map[string]json.RawMessage
	_ = json.Unmarshal(data, &staged)
	var provider struct {
		BaseURL string `json:"base_url"`
	}
	_ = json.Unmarshal(staged["model_providers.teo_ai"], &provider)
	req, _ := http.NewRequest("POST", provider.BaseURL+"/responses", strings.NewReader("tool result"))
	req.Header.Set(keyHeader, os.Getenv("TEO_AI_GATEWAY_KEY"))
	req.Header.Set("Authorization", "Bearer personal-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		os.Exit(13)
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(data) != "gateway-ok" {
		os.Exit(14)
	}
	os.Exit(23)
}

func desktopFixture(t *testing.T, testName, marker string) string {
	t.Helper()
	t.Setenv(marker, "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEO_TEST_BINARY", binary)
	path := filepath.Join(t.TempDir(), "native-client")
	script := "#!/bin/sh\nexec \"$TEO_TEST_BINARY\" -test.run=^" + testName + "$ -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDesktopConfigRestorationAndLock(t *testing.T) {
	helper := desktopFixture(t, "TestDesktopBackendProcess", "TEO_TEST_BACKEND")
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-config", true: "existing-config"}[exists], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			original := []byte("# preserve exactly\nmodel = \"my-model\"\n[projects.\"/project with spaces\"]\ntrust_level = \"trusted\"\n")
			if exists {
				if err := os.WriteFile(path, original, 0640); err != nil {
					t.Fatal(err)
				}
			}
			cfg := Config{GatewayURL: "https://ai.example.com", KeyFile: "/key"}
			restore, err := configureDesktop(helper, path, cfg, append(os.Environ(), "TEO_AI_GATEWAY_KEY="+testKey))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			for _, expected := range []string{"https://ai.example.com/codex", "requires_openai_auth", "supports_websockets", "TEO_AI_GATEWAY_KEY", "chatgpt"} {
				if !bytes.Contains(data, []byte(expected)) {
					t.Errorf("missing %s", expected)
				}
			}
			if bytes.Contains(data, []byte(testKey)) {
				t.Fatal("key stored in config")
			}
			if _, err := configureDesktop(helper, path, cfg, os.Environ()); err == nil {
				t.Fatal("second session acquired the lock")
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			if exists {
				data, err = os.ReadFile(path)
				info, _ := os.Stat(path)
				if err != nil || !bytes.Equal(data, original) || info.Mode().Perm() != 0640 {
					t.Fatal("original contents or permissions not restored")
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("new config not removed")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".teo-ai-desktop")); !os.IsNotExist(err) {
				t.Fatal("lock not removed")
			}
		})
	}
}

func TestDesktopPreservesConcurrentChangesAndRejectsSymlink(t *testing.T) {
	helper := desktopFixture(t, "TestDesktopBackendProcess", "TEO_TEST_BACKEND")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := []byte("# original\n")
	_ = os.WriteFile(path, original, 0600)
	cfg := Config{GatewayURL: "https://ai.example.com", KeyFile: "/key"}
	restore, err := configureDesktop(helper, path, cfg, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte("# edited by the user while Desktop was running\n")
	_ = os.WriteFile(path, changed, 0600)
	if err := restore(); err == nil {
		t.Fatal("concurrent changes silently overwritten")
	}
	data, _ := os.ReadFile(path)
	backup, _ := os.ReadFile(filepath.Join(dir, ".teo-ai-desktop", "config.original.toml"))
	if !bytes.Equal(data, changed) || !bytes.Equal(backup, original) {
		t.Fatal("user changes or recovery backup lost")
	}
	link := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := configureDesktop(helper, link, cfg, os.Environ()); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestDesktopBackendFailureLeavesConfigUntouched(t *testing.T) {
	helper := desktopFixture(t, "TestDesktopBackendProcess", "TEO_TEST_BACKEND")
	t.Setenv("TEO_TEST_BACKEND_FAIL", "1")
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("# original\n")
	_ = os.WriteFile(path, original, 0600)
	_, err := configureDesktop(helper, path, Config{GatewayURL: "https://example.com", KeyFile: "/key"}, os.Environ())
	if err == nil || strings.Contains(err.Error(), "private config") {
		t.Fatal("backend failure missing or leaked details")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("config changed on backend failure")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".teo-ai-desktop")); !os.IsNotExist(err) {
		t.Fatal("lock left after staging failure")
	}
}

func TestDesktopLaunchFailureRestoresConfig(t *testing.T) {
	helper := desktopFixture(t, "TestDesktopBackendProcess", "TEO_TEST_BACKEND")
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("# original\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := desktopSession(filepath.Join(t.TempDir(), "missing-app"), helper, path, Config{GatewayURL: "https://example.com", KeyFile: "/key"}, testKey, strings.NewReader(""), &output, &output)
	if err == nil {
		t.Fatal("missing application accepted")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("config not restored after launch failure")
	}
}

func TestDesktopHelpWithoutConfiguration(t *testing.T) {
	var output bytes.Buffer
	if code := Run([]string{"desktop", "--help"}, strings.NewReader(""), &output, &output); code != 0 || !strings.Contains(output.String(), "-app") {
		t.Fatalf("desktop help failed: %d %s", code, output.String())
	}
}

func TestDesktopRelaySurvivesUntilAppExitAndConfigIsRestored(t *testing.T) {
	helper := desktopFixture(t, "TestDesktopBackendProcess", "TEO_TEST_BACKEND")
	app := desktopFixture(t, "TestDesktopAppProcess", "TEO_TEST_APP")
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("# original\n")
	_ = os.WriteFile(path, original, 0600)
	t.Setenv("TEO_TEST_CONFIG", path)
	t.Setenv("OPENAI_API_KEY", "must-not-reach-desktop")
	requests := make(chan struct{}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/responses" || r.Header.Get(keyHeader) != testKey || r.Header.Get("Authorization") != "Bearer personal-token" {
			t.Error("incorrect route or credentials")
		}
		requests <- struct{}{}
		_, _ = io.WriteString(w, "gateway-ok")
	}))
	defer upstream.Close()
	var output bytes.Buffer
	err := desktopSession(app, helper, path, Config{GatewayURL: upstream.URL, KeyFile: "/key", InsecureSkipTLSVerify: true}, testKey, strings.NewReader(""), &output, &output)
	if exit, ok := err.(*childExit); !ok || exit.code != 23 {
		t.Fatalf("app exit not preserved: %v", err)
	}
	select {
	case <-requests:
	default:
		t.Fatal("app did not reach the gateway")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) || strings.Contains(output.String(), testKey) {
		t.Fatal("config not restored or output leaked key")
	}
}

func TestDesktopNativeConfigEditor(t *testing.T) {
	helper := os.Getenv("TEO_TEST_CODEX_BACKEND")
	if helper == "" {
		t.Skip("set TEO_TEST_CODEX_BACKEND to test the installed Codex TOML editor in isolation")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("# keep comment\nmodel_provider = \"openai\"\nmodel = \"custom-model\"\n[model_providers.teo_ai]\nname = \"old\"\nenv_key = \"WRONG_KEY\"\n")
	_ = os.WriteFile(path, original, 0600)
	restore, err := configureDesktop(helper, path, Config{GatewayURL: "https://example.com", KeyFile: "/key"}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	}()
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("# keep comment")) || !bytes.Contains(data, []byte(`model = "custom-model"`)) || bytes.Contains(data, []byte("WRONG_KEY")) {
		t.Fatal("native editor damaged user settings or retained conflicting provider credentials")
	}
	cmd := exec.Command(helper, "app-server", "--print-startup-config")
	env := envMap(os.Environ())
	env["CODEX_HOME"] = filepath.Dir(path)
	env["TEO_AI_GATEWAY_KEY"] = testKey
	cmd.Env, cmd.Dir = flattenEnv(env), filepath.Dir(path)
	if err := cmd.Run(); err != nil {
		t.Fatalf("native Codex rejected the generated config: %v", err)
	}
}
