package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"teo-ai/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "teo-ai-gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	healthcheck := flag.Bool("healthcheck", false, "check local /healthz and exit")
	flag.Parse()
	addr := envOr("TEO_LISTEN_ADDR", ":8080")
	if *healthcheck {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return errors.New("invalid TEO_LISTEN_ADDR")
		}
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		if host == "::" {
			host = "::1"
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
		if err != nil {
			return errors.New("health check failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New("health check failed")
		}
		return nil
	}
	keyPath := os.Getenv("TEO_GATEWAY_KEY_FILE")
	if keyPath == "" {
		return errors.New("TEO_GATEWAY_KEY_FILE is required")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return errors.New("cannot read TEO_GATEWAY_KEY_FILE")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	handler, err := gateway.New(gateway.Config{
		Key:            strings.TrimSpace(string(key)),
		CodexUpstream:  envOr("TEO_CODEX_UPSTREAM", "https://chatgpt.com/backend-api/codex"),
		ClaudeUpstream: envOr("TEO_CLAUDE_UPSTREAM", "https://api.anthropic.com"),
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
		// No WriteTimeout: a model response can stream for many minutes.
		ErrorLog: slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	logger.Info("gateway started", "listen", addr)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		return nil
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
