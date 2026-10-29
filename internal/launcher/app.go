package launcher

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"teo-ai/internal/buildinfo"
)

const usage = `teo-ai: native coding clients with personal subscriptions through a gateway

Usage:
  teo-ai init --gateway https://ai.example.com --key-file /path/to/gateway-key
  teo-ai login codex [--device-auth]
  teo-ai login claude
  teo-ai codex [--] [native arguments...]
  teo-ai claude [--] [native arguments...]
  teo-ai desktop [--app /path/to/Codex.app]
  teo-ai doctor
  teo-ai version

Global flags (before the command): --config, --gateway, --key-file, --insecure-skip-tls-verify
Environment: TEO_AI_CONFIG, TEO_AI_GATEWAY_URL, TEO_AI_GATEWAY_KEY_FILE,
             TEO_AI_INSECURE_SKIP_TLS_VERIFY
Login and token refresh are performed only by the native clients.
`

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	err := run(args, stdin, stdout, stderr)
	if err == nil {
		return 0
	}
	var exit *childExit
	if errors.As(err, &exit) {
		return exit.code
	}
	fmt.Fprintln(stderr, "teo-ai:", err)
	return 1
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	defaultPath, err := DefaultConfigPath()
	if err != nil {
		return err
	}
	if value := os.Getenv("TEO_AI_CONFIG"); value != "" {
		defaultPath = value
	}
	fs := flag.NewFlagSet("teo-ai", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultPath, "configuration file")
	gateway := fs.String("gateway", os.Getenv("TEO_AI_GATEWAY_URL"), "gateway HTTPS origin")
	keyFile := fs.String("key-file", os.Getenv("TEO_AI_GATEWAY_KEY_FILE"), "gateway credential file")
	var insecure optionalBool
	if value, present := os.LookupEnv("TEO_AI_INSECURE_SKIP_TLS_VERIFY"); present {
		if err := insecure.Set(value); err != nil {
			return err
		}
	}
	fs.Var(&insecure, "insecure-skip-tls-verify", "skip certificate verification only for the MVP gateway")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] == "help" {
		fmt.Fprint(stdout, usage)
		return nil
	}
	command, nativeArgs := rest[0], rest[1:]
	if len(nativeArgs) > 0 && nativeArgs[0] == "--" {
		nativeArgs = nativeArgs[1:]
	}
	if command == "version" {
		fmt.Fprintf(stdout, "teo-ai %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	}
	if command == "desktop" && len(nativeArgs) == 1 && (nativeArgs[0] == "--help" || nativeArgs[0] == "-h") {
		return launchDesktop(nativeArgs, Config{}, "", stdin, stdout, stderr)
	}
	if command == "login" {
		plan, err := loginPlan(nativeArgs, os.Environ())
		if err != nil {
			return err
		}
		return execute(plan, stdin, stdout, stderr)
	}
	if command != "init" && command != "doctor" && command != "codex" && command != "claude" && command != "desktop" {
		return errors.New("unknown command; run teo-ai help")
	}
	path, err := expandPath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	if command == "init" {
		initFlags := flag.NewFlagSet("init", flag.ContinueOnError)
		initFlags.SetOutput(stderr)
		initFlags.StringVar(gateway, "gateway", *gateway, "gateway HTTPS origin")
		initFlags.StringVar(keyFile, "key-file", *keyFile, "gateway credential file")
		initFlags.Var(&insecure, "insecure-skip-tls-verify", "skip certificate verification only for the MVP gateway")
		if err := initFlags.Parse(nativeArgs); err != nil {
			return err
		}
		if len(initFlags.Args()) != 0 {
			return errors.New("unexpected init arguments")
		}
	}
	if *gateway != "" {
		cfg.GatewayURL = strings.TrimRight(*gateway, "/")
	}
	if *keyFile != "" {
		cfg.KeyFile = *keyFile
	}
	if insecure.set {
		cfg.InsecureSkipTLSVerify = insecure.value
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.KeyFile, err = expandPath(cfg.KeyFile)
	if err != nil {
		return err
	}
	key, err := ReadKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	switch command {
	case "init":
		if err := SaveConfig(path, cfg); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Saved configuration:", path)
		return nil
	case "doctor":
		if len(nativeArgs) != 0 {
			return errors.New("doctor takes no arguments")
		}
		return doctor(cfg, key, stdout)
	case "desktop":
		return launchDesktop(nativeArgs, cfg, key, stdin, stdout, stderr)
	default:
		launchConfig := cfg
		if cfg.InsecureSkipTLSVerify {
			endpoint, cleanup, err := startInsecureRelay(cfg, key)
			if err != nil {
				return err
			}
			defer cleanup()
			launchConfig.GatewayURL = endpoint
		}
		plan, err := BuildPlan(command, nativeArgs, launchConfig, key, os.Environ())
		if err != nil {
			return err
		}
		return execute(plan, stdin, stdout, stderr)
	}
}

func loginPlan(args, environ []string) (Plan, error) {
	if len(args) == 0 {
		return Plan{}, errors.New("usage: teo-ai login codex|claude")
	}
	tool, extra := args[0], args[1:]
	env := envMap(environ)
	for name := range env {
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_USE_") || name == "CLAUDE_CODE_OAUTH_TOKEN" || name == "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR" || name == "OPENAI_API_KEY" || name == "CODEX_API_KEY" || name == "OPENAI_BASE_URL" || name == "OPENAI_ACCESS_TOKEN" || name == "TEO_AI_GATEWAY_KEY" {
			delete(env, name)
		}
	}
	p := Plan{Executable: tool, Env: flattenEnv(env)}
	switch tool {
	case "codex":
		for _, arg := range extra {
			if arg != "--device-auth" {
				return Plan{}, errors.New("only --device-auth is accepted for Codex subscription login")
			}
		}
		p.Args = append([]string{"-c", `forced_login_method="chatgpt"`, "login"}, extra...)
	case "claude":
		if len(extra) != 0 {
			return Plan{}, errors.New("Claude subscription login takes no extra arguments")
		}
		p.Settings = map[string]any{"forceLoginMethod": "claudeai", "apiKeyHelper": "", "env": map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.anthropic.com", "ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "",
			"ANTHROPIC_CUSTOM_HEADERS": "", "CLAUDE_CODE_OAUTH_TOKEN": "",
			"CLAUDE_CODE_USE_BEDROCK": "0", "CLAUDE_CODE_USE_VERTEX": "0", "CLAUDE_CODE_USE_FOUNDRY": "0", "CLAUDE_CODE_USE_MANTLE": "0",
		}}
		p.Args = []string{"auth", "login"}
	default:
		return Plan{}, errors.New("login tool must be codex or claude")
	}
	return p, nil
}

func doctor(cfg Config, key string, out io.Writer) error {
	for _, tool := range []string{"codex", "claude"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Fprintln(out, tool+": missing from PATH")
		} else {
			fmt.Fprintln(out, tool+": installed")
		}
	}
	transport := gatewayTransport(cfg.InsecureSkipTLSVerify)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(cfg.GatewayURL, "/")+"/readyz", nil)
	req.Header.Set(keyHeader, key)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("gateway connection failed; check DNS, HTTPS certificate, and network access")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway returned HTTP %d; check gateway key and configuration", resp.StatusCode)
	}
	var ready struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ready); err != nil || ready.Status != "ready" {
		return errors.New("gateway returned an unexpected readiness response; check the configured address")
	}
	fmt.Fprintln(out, "gateway: reachable; gateway key accepted")
	if cfg.InsecureSkipTLSVerify {
		fmt.Fprintln(out, "gateway TLS certificate verification: disabled (MVP)")
	}
	fmt.Fprintln(out, "Provider access was not tested. Check native /status and send a test prompt.")
	return nil
}

func execute(plan Plan, stdin io.Reader, stdout, stderr io.Writer) error {
	if _, err := exec.LookPath(plan.Executable); err != nil {
		return fmt.Errorf("%s is not installed or is missing from PATH", plan.Executable)
	}
	args := append([]string(nil), plan.Args...)
	if plan.Settings != nil {
		dir, err := os.MkdirTemp("", "teo-ai-*")
		if err != nil {
			return errors.New("cannot create private temporary settings directory")
		}
		defer os.RemoveAll(dir)
		data, err := writeSettings(plan.Settings)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return errors.New("cannot create temporary Claude settings")
		}
		args = append([]string{"--settings", path}, args...)
	}
	cmd := exec.Command(plan.Executable, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = plan.Env, stdin, stdout, stderr
	return runChild(cmd)
}
