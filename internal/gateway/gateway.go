// Package gateway forwards model requests without replacing provider credentials.
package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const KeyHeader = "X-Teo-Gateway-Key"

type Config struct {
	Key            string
	CodexUpstream  string
	ClaudeUpstream string
	Transport      http.RoundTripper
	Logger         *slog.Logger
}

// New uses fixed upstreams. Neither a URL parameter nor a client Host can select
// an upstream. OAuth login and token renewal are deliberately outside this proxy.
func New(cfg Config) (http.Handler, error) {
	if len(cfg.Key) < 32 || len(cfg.Key) > 4096 {
		return nil, errors.New("gateway key must contain 32 to 4096 characters")
	}
	for _, c := range cfg.Key {
		if c < 33 || c > 126 {
			return nil, errors.New("gateway key must contain printable ASCII without whitespace")
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 5 * time.Minute
		transport.MaxIdleConnsPerHost = 32
		transport.DisableCompression = true
		cfg.Transport = transport
	}
	proxies := make(map[string]*httputil.ReverseProxy)
	for prefix, upstream := range map[string]string{
		"/codex": cfg.CodexUpstream, "/anthropic": cfg.ClaudeUpstream,
	} {
		target, err := url.Parse(upstream)
		if err != nil || target == nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") {
			return nil, errors.New("invalid upstream URL")
		}
		proxies[prefix] = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				// Strip the public prefix before joining the fixed upstream base path.
				pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
				pr.Out.URL.RawPath = ""
				pr.SetURL(target)
				// SetURL joins target.Path with Out.URL.Path; keep the original query.
				pr.Out.URL.RawQuery = pr.In.URL.RawQuery
				pr.Out.Host = target.Host
				pr.Out.Header.Del(KeyHeader)
				pr.Out.Header.Del("Forwarded")
				pr.Out.Header.Del("X-Forwarded-For")
				pr.Out.Header.Del("X-Forwarded-Host")
				pr.Out.Header.Del("X-Forwarded-Proto")
				pr.Out.Header.Del("Proxy-Authorization")
			},
			Transport:     cfg.Transport,
			FlushInterval: -1, // Flush each write, including incremental SSE events.
			ErrorLog:      log.New(io.Discard, "", 0),
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				// Transport errors can contain URLs. Never log their raw text.
				writeJSON(w, http.StatusBadGateway, "upstream unavailable")
			},
		}
	}
	wantKey := sha256.Sum256([]byte(cfg.Key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, "ok")
			return
		}
		gotKey := sha256.Sum256([]byte(r.Header.Get(KeyHeader)))
		if subtle.ConstantTimeCompare(wantKey[:], gotKey[:]) != 1 {
			writeJSON(w, http.StatusUnauthorized, "invalid gateway credential")
			return
		}
		if r.URL.Path == "/readyz" && r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, "ready")
			return
		}
		// Reject ambiguous paths rather than letting an upstream normalize them.
		if r.URL.RawPath != "" || strings.Contains(r.URL.Path, "\\") || strings.Contains(r.URL.Path, "//") {
			writeJSON(w, http.StatusBadRequest, "invalid path")
			return
		}
		for _, part := range strings.Split(r.URL.Path, "/") {
			if part == "." || part == ".." {
				writeJSON(w, http.StatusBadRequest, "invalid path")
				return
			}
		}
		var prefix string
		for candidate := range proxies {
			if strings.HasPrefix(r.URL.Path, candidate+"/") {
				prefix = candidate
				break
			}
		}
		if prefix == "" {
			writeJSON(w, http.StatusNotFound, "unknown route")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == "" {
			writeJSON(w, http.StatusUnauthorized, "provider bearer credential required")
			return
		}
		// API-key auth should not accidentally incur provider API billing.
		if r.Header.Get("X-Api-Key") != "" {
			writeJSON(w, http.StatusBadRequest, "API-key authentication is not supported")
			return
		}
		start := time.Now()
		response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			cfg.Logger.Info("request", "route", prefix, "method", r.Method,
				"status", response.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		proxies[prefix].ServeHTTP(response, r)
	}), nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush/Hijack for SSE and upgrades.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
