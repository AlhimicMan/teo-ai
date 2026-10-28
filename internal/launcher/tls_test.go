package launcher

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorSelfSignedRequiresExplicitOptIn(t *testing.T) {
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" || r.Header.Get(keyHeader) != testKey {
			t.Error("incorrect gateway request")
		}
		io.WriteString(w, `{"status":"ready"}`)
	}))
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.StartTLS()
	defer up.Close()
	cfg := Config{GatewayURL: up.URL, KeyFile: "/key"}
	var output bytes.Buffer
	if err := doctor(cfg, testKey, &output); err == nil {
		t.Fatal("default transport accepted self-signed TLS")
	}
	cfg.InsecureSkipTLSVerify = true
	if err := doctor(cfg, testKey, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "verification: disabled") {
		t.Fatal("TLS mode not reported")
	}
}

func TestInsecureRelayPreservesCredentialsAndStops(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/codex/responses?opaque=a%2Bb" {
			t.Errorf("URI changed: %s", r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer personal-token" || r.Header.Get(keyHeader) != testKey {
			t.Error("credentials changed")
		}
		if r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Error("account header changed")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Error("body changed")
		}
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(429)
		io.WriteString(w, "limit")
	}))
	defer up.Close()
	cfg := Config{GatewayURL: up.URL, KeyFile: "/key", InsecureSkipTLSVerify: true}
	endpoint, stop, err := startInsecureRelay(cfg, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatal("relay is not restricted to loopback")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("POST", endpoint+"/codex/responses?opaque=a%2Bb", strings.NewReader("payload"))
	req.Header.Set(keyHeader, testKey)
	req.Header.Set("Authorization", "Bearer personal-token")
	req.Header.Set("ChatGPT-Account-ID", "account")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "9" || string(body) != "limit" {
		t.Fatal("upstream response changed")
	}
	for _, tc := range []struct {
		path, key string
		status    int
	}{{"/codex/models", "", 401}, {"/unrelated", testKey, 404}} {
		req, _ := http.NewRequest("GET", endpoint+tc.path, nil)
		req.Header.Set(keyHeader, tc.key)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Errorf("%s: %d", tc.path, response.StatusCode)
		}
	}
	stop()
	if response, err := client.Get(endpoint + "/readyz"); err == nil {
		response.Body.Close()
		t.Fatal("relay is still listening after cleanup")
	}
}

func TestInsecureRelayStreamsAndCancels(t *testing.T) {
	canceled := make(chan struct{})
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer up.Close()
	endpoint, stop, err := startInsecureRelay(Config{GatewayURL: up.URL, KeyFile: "/key", InsecureSkipTLSVerify: true}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint+"/anthropic/v1/messages", nil)
	req.Header.Set(keyHeader, testKey)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	response.Body.Close()
	cancel()
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream buffered or changed: %q %v", line, err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not canceled")
	}
}

func TestTLSFlagPersistsAndCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(key, []byte(testKey), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for _, value := range []string{"true", "false"} {
		code := Run([]string{"--config", config, "init", "--gateway", "https://192.0.2.10:8443", "--key-file", key, "--insecure-skip-tls-verify=" + value}, strings.NewReader(""), &output, &output)
		if code != 0 {
			t.Fatal(output.String())
		}
		cfg, err := LoadConfig(config)
		if err != nil || cfg.InsecureSkipTLSVerify != (value == "true") {
			t.Fatalf("TLS setting not persisted: %+v %v", cfg, err)
		}
	}
	t.Setenv("TEO_AI_INSECURE_SKIP_TLS_VERIFY", "invalid")
	if code := Run([]string{"--config", config, "doctor"}, strings.NewReader(""), &output, &output); code == 0 {
		t.Fatal("invalid TLS environment setting accepted")
	}
}
