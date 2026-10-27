package gateway

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "gateway-key-012345678901234567890123456789"

func testGateway(t *testing.T, upstream http.Handler, logs io.Writer) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	if logs == nil {
		logs = io.Discard
	}
	h, err := New(Config{Key: testKey, CodexUpstream: up.URL + "/backend-api/codex", ClaudeUpstream: up.URL, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func authorizedRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(KeyHeader, testKey)
	req.Header.Set("Authorization", "Bearer personal-access-token")
	return req
}

func TestForwardingPreservesNativeRequestAndResponse(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/codex/responses", "/backend-api/codex/responses"},
		{"/codex/models", "/backend-api/codex/models"},
		{"/codex/responses/compact", "/backend-api/codex/responses/compact"},
		{"/anthropic/v1/messages", "/v1/messages"},
		{"/anthropic/v1/messages/count_tokens", "/v1/messages/count_tokens"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var log bytes.Buffer
			payload := `{"model":"native-model","secret":"source-code"}`
			srv := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.want {
					t.Errorf("path = %s, want %s", r.URL.Path, tc.want)
				}
				if r.URL.RawQuery != "client_version=0.164&opaque=a%2Bb" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				for name, want := range map[string]string{"Authorization": "Bearer personal-access-token", "ChatGPT-Account-ID": "personal-account", "Anthropic-Beta": "oauth-2025-04-20", "Anthropic-Version": "2023-06-01", "Content-Type": "application/json"} {
					if r.Header.Get(name) != want {
						t.Errorf("%s not preserved", name)
					}
				}
				for _, name := range []string{KeyHeader, "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "Proxy-Authorization"} {
					if r.Header.Get(name) != "" {
						t.Errorf("leaked %s", name)
					}
				}
				if r.Host == "attacker.invalid" {
					t.Error("client Host forwarded")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != payload {
					t.Errorf("body changed: %s", body)
				}
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Request-ID", "upstream-id")
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"error":"subscription limit"}`)
			}), &log)
			req := authorizedRequest(t, "POST", srv.URL+tc.path+"?client_version=0.164&opaque=a%2Bb", strings.NewReader(payload))
			req.Host = "attacker.invalid"
			for name, value := range map[string]string{"ChatGPT-Account-ID": "personal-account", "Anthropic-Beta": "oauth-2025-04-20", "Anthropic-Version": "2023-06-01", "Content-Type": "application/json", "Forwarded": "for=private", "X-Forwarded-For": "private", "X-Forwarded-Host": "private", "Proxy-Authorization": "private"} {
				req.Header.Set(name, value)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			// Client EOF may precede the handler's deferred access-log write.
			// Close waits for that handler before inspecting the test log buffer.
			srv.Close()
			if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" || resp.Header.Get("X-Request-ID") != "upstream-id" || string(body) != `{"error":"subscription limit"}` {
				t.Fatalf("upstream response changed: %d %s", resp.StatusCode, body)
			}
			for _, secret := range []string{testKey, "personal-access-token", "personal-account", "source-code", "opaque", "/responses", "private"} {
				if strings.Contains(log.String(), secret) {
					t.Errorf("log leaked request data: %s", secret)
				}
			}
		})
	}
}

func TestRejectsUnauthorizedAndAmbiguousRequests(t *testing.T) {
	srv := testGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rejected request reached upstream") }), nil)
	for _, tc := range []struct {
		path, method, key, auth, apiKey string
		status                          int
	}{
		{"/codex/responses", "POST", "", "Bearer token", "", 401},
		{"/codex/responses", "POST", "wrong", "Bearer token", "", 401},
		{"/codex/responses", "POST", testKey, "", "", 401},
		{"/anthropic/v1/messages", "POST", testKey, "Bearer token", "api-key", 400},
		{"/codexevil/responses", "POST", testKey, "Bearer token", "", 404},
		{"/codex/../anthropic/v1/messages", "POST", testKey, "Bearer token", "", 400},
		{"/codex/%2e%2e/responses", "POST", testKey, "Bearer token", "", 400},
		{"/codex//responses", "POST", testKey, "Bearer token", "", 400},
		{"/codex/%2Fresponses", "POST", testKey, "Bearer token", "", 400},
		{"/anthropic/v1/messages", "CONNECT", testKey, "Bearer token", "", 405},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		req.Header.Set(KeyHeader, tc.key)
		req.Header.Set("Authorization", tc.auth)
		req.Header.Set("X-Api-Key", tc.apiKey)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, resp.StatusCode, tc.status)
		}
	}
}

func TestSSEArrivesBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, "data: done\n\n")
		case <-r.Context().Done():
		}
	}), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := authorizedRequest(t, "POST", srv.URL+"/codex/responses", nil).WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first SSE event buffered or changed: %q %v", line, err)
	}
}

func TestCancellationReachesUpstream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	srv := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := authorizedRequest(t, "POST", srv.URL+"/codex/responses", nil).WithContext(ctx)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive cancellation")
	}
}

type failedTransport struct{}

func (failedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestHealthAndUpstreamFailure(t *testing.T) {
	h, err := New(Config{Key: testKey, CodexUpstream: "https://example.com/backend", ClaudeUpstream: "https://example.com", Transport: failedTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		key    bool
		status int
	}{{"/healthz", false, 200}, {"/readyz", false, 401}, {"/readyz", true, 200}, {"/codex/responses", true, 502}} {
		req := authorizedRequest(t, "GET", "http://gateway"+tc.path, nil)
		if !tc.key {
			req.Header.Del(KeyHeader)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Errorf("%s: %d, want %d", tc.path, w.Code, tc.status)
		}
	}
}
