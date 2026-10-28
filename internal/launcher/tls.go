package launcher

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// optionalBool distinguishes an omitted setting from an explicit false.
type optionalBool struct{ value, set bool }

func (b *optionalBool) String() string   { return strconv.FormatBool(b.value) }
func (b *optionalBool) IsBoolFlag() bool { return true }
func (b *optionalBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return errors.New("insecure-skip-tls-verify must be true or false")
	}
	b.value, b.set = v, true
	return nil
}

func gatewayTransport(insecure bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The user explicitly opts in for a self-signed MVP gateway. This
		// transport is scoped to that gateway; native login TLS is unaffected.
		InsecureSkipVerify: insecure,
	}
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 5 * time.Minute
	return transport
}

// startInsecureRelay gives both native clients the same explicit per-gateway
// TLS behavior without global NODE_TLS_REJECT_UNAUTHORIZED or undocumented
// Codex environment switches. The ephemeral listener lives only with the CLI.
func startInsecureRelay(cfg Config, key string) (string, func(), error) {
	if err := cfg.Validate(); err != nil {
		return "", nil, err
	}
	target, _ := url.Parse(cfg.GatewayURL)
	if !cfg.InsecureSkipTLSVerify || target.Scheme != "https" {
		return "", nil, errors.New("insecure relay requires an HTTPS gateway and explicit TLS opt-in")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", nil, errors.New("cannot open local gateway relay")
	}
	transport := gatewayTransport(true)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
		},
		Transport: transport, FlushInterval: -1,
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "gateway unavailable", http.StatusBadGateway)
		},
	}
	want := sha256.Sum256([]byte(key))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get(keyHeader)))
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			http.Error(w, "gateway credential required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodConnect || (r.URL.Path != "/readyz" && !strings.HasPrefix(r.URL.Path, "/codex/") && !strings.HasPrefix(r.URL.Path, "/anthropic/")) {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	cleanup := func() {
		_ = server.Close()
		transport.CloseIdleConnections()
		<-done
	}
	return "http://" + listener.Addr().String(), cleanup, nil
}
