package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func launchDesktop(args []string, cfg Config, key string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "Codex Desktop .app bundle (macOS)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("usage: teo-ai desktop [--app /path/to/Codex.app]")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("desktop currently supports macOS only; use teo-ai codex on Linux")
	}
	executable, helper, err := findDesktop(*app)
	if err != nil {
		return err
	}
	// An existing GUI process would retain its old environment and routing.
	check := exec.Command("/usr/bin/pgrep", "-u", fmt.Sprint(os.Getuid()), "-x", regexp.QuoteMeta(filepath.Base(executable)))
	if err := check.Run(); err == nil {
		return errors.New("quit Codex Desktop completely (Cmd+Q), then run teo-ai desktop again")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		return errors.New("cannot check whether Codex Desktop is running")
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(userHome, ".codex")
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return err
	}
	return desktopSession(executable, helper, filepath.Join(home, "config.toml"), cfg, key, stdin, stdout, stderr)
}

func findDesktop(app string) (string, string, error) {
	var candidates []string
	if app != "" {
		path, err := expandPath(app)
		if err != nil {
			return "", "", err
		}
		candidates = []string{path}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		for _, name := range []string{"Codex.app", "ChatGPT.app"} {
			candidates = append(candidates, filepath.Join(home, "Applications", name), filepath.Join("/Applications", name))
		}
	}
	for _, bundle := range candidates {
		// plutil handles both XML and binary Info.plist files.
		name, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleExecutable", "raw", "-o", "-", filepath.Join(bundle, "Contents", "Info.plist")).Output()
		if err != nil {
			continue
		}
		base := strings.TrimSpace(string(name))
		if base == "" || base == "." || base == ".." || filepath.Base(base) != base {
			continue
		}
		executable := filepath.Join(bundle, "Contents", "MacOS", base)
		if info, err := os.Stat(executable); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		// Prefer the backend shipped with the GUI for matching config semantics.
		for _, name := range []string{"codex", "codex-cli"} {
			helper := filepath.Join(bundle, "Contents", "Resources", name)
			if info, err := os.Stat(helper); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				return executable, helper, nil
			}
		}
		if helper, err := exec.LookPath("codex"); err == nil {
			return executable, helper, nil
		}
		return "", "", errors.New("Codex config backend not found in app bundle or PATH; install Codex CLI")
	}
	return "", "", errors.New("Codex Desktop not found; install the app or pass --app /path/to/Codex.app")
}

func desktopSession(executable, helper, configPath string, cfg Config, key string, stdin io.Reader, stdout, stderr io.Writer) (result error) {
	if cfg.InsecureSkipTLSVerify {
		endpoint, stop, err := startInsecureRelay(cfg, key)
		if err != nil {
			return err
		}
		defer stop()
		cfg.GatewayURL = endpoint
	}
	plan, err := BuildPlan("codex", nil, cfg, key, os.Environ())
	if err != nil {
		return err
	}
	restore, err := configureDesktop(helper, configPath, cfg, plan.Env)
	if err != nil {
		return err
	}
	defer func() {
		if err := restore(); err != nil {
			if result != nil {
				fmt.Fprintln(stderr, "teo-ai:", err)
			} else {
				result = err
			}
		}
	}()
	fmt.Fprintln(stdout, "Starting Codex Desktop through teo-ai. Keep this terminal open until you quit the app (Cmd+Q).")
	// Launch the bundle executable directly: LaunchServices can reuse an existing
	// process, discard our environment, or return before the relay can be stopped.
	return execute(Plan{Executable: executable, Env: plan.Env}, stdin, stdout, stderr)
}

// Use Codex's own TOML editor rather than rewriting arbitrary user TOML. The
// editor runs in an isolated home with no native credentials, and modifies only
// a staged copy. The gateway key is never written to either config file.
func configureDesktop(helper, configPath string, cfg Config, environ []string) (func() error, error) {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("cannot create Codex config directory")
	}
	lock := filepath.Join(dir, ".teo-ai-desktop")
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, fmt.Errorf("cannot acquire desktop config lock; check for another teo-ai desktop session or recover the backup in %s", lock)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(lock)
		}
	}()
	original, mode, existed, err := readDesktopConfig(configPath)
	if err != nil {
		return nil, err
	}
	backup := filepath.Join(lock, "config.original.toml")
	if existed {
		if err := os.WriteFile(backup, original, 0600); err != nil {
			return nil, errors.New("cannot back up Codex config")
		}
	} else if err := os.WriteFile(filepath.Join(lock, "config.was-absent"), nil, 0600); err != nil {
		return nil, err
	}
	staged := filepath.Join(lock, "config.toml")
	if err := os.WriteFile(staged, original, 0600); err != nil {
		return nil, err
	}
	if err := editDesktopConfig(helper, staged, cfg, environ); err != nil {
		return nil, err
	}
	updated, err := os.ReadFile(staged)
	if err != nil {
		return nil, errors.New("cannot read staged Codex config")
	}
	current, _, present, err := readDesktopConfig(configPath)
	if err != nil || present != existed || !bytes.Equal(current, original) {
		return nil, errors.New("Codex config changed while preparing Desktop; retry after closing other clients")
	}
	if err := replaceDesktopConfig(configPath, updated, 0600); err != nil {
		return nil, err
	}
	keep = true
	return func() error {
		current, _, present, err := readDesktopConfig(configPath)
		if err != nil || !present || !bytes.Equal(current, updated) {
			return fmt.Errorf("Codex config changed during the Desktop session; preserved your changes and backup in %s; restore provider settings manually before removing this lock directory", lock)
		}
		if existed {
			err = replaceDesktopConfig(configPath, original, mode)
		} else {
			err = os.Remove(configPath)
		}
		if err != nil {
			return fmt.Errorf("cannot restore Codex config; backup retained in %s", lock)
		}
		return os.RemoveAll(lock)
	}, nil
}

func readDesktopConfig(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0600, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, 0, false, errors.New("Codex config must be a regular file of at most 4 MiB (symlinks are not supported)")
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), true, err
}

func replaceDesktopConfig(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".teo-ai-config-*")
	if err != nil {
		return errors.New("cannot stage Codex config")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("cannot write Codex config")
	}
	return os.Rename(f.Name(), path)
}

func editDesktopConfig(helper, path string, cfg Config, environ []string) error {
	editorHome := filepath.Join(filepath.Dir(path), "editor")
	if err := os.Mkdir(editorHome, 0700); err != nil {
		return err
	}
	env := envMap(environ)
	delete(env, "TEO_AI_GATEWAY_KEY")
	env["CODEX_HOME"] = editorHome
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "app-server", "--listen", "stdio://")
	cmd.Env, cmd.Dir = flattenEnv(env), editorHome
	// PATH may contain the npm launcher, which spawns a native backend. Stop
	// the whole editor process group on timeout/cleanup so it cannot outlive us
	// with an open stdout pipe or keep writing the staged config.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Backend errors may contain user config or credentials; never echo them.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return errors.New("cannot start Codex config editor")
	}
	defer func() {
		_ = in.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	encoder, decoder := json.NewEncoder(in), json.NewDecoder(out)
	call := func(id int, method string, params any) error {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return errors.New("cannot send Codex config request")
		}
		for {
			var response struct {
				ID     *int            `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := decoder.Decode(&response); err != nil {
				return errors.New("Codex config editor disconnected or timed out; check the installed Codex version")
			}
			if response.ID == nil || *response.ID != id {
				continue
			}
			if len(response.Error) != 0 && string(response.Error) != "null" {
				return errors.New("Codex rejected the Desktop config; original config was not changed")
			}
			if len(response.Result) == 0 || string(response.Result) == "null" {
				return errors.New("Codex config editor returned an invalid response")
			}
			return nil
		}
	}
	if err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "teo-ai", "version": "1"}}); err != nil {
		return err
	}
	if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return errors.New("cannot initialize Codex config editor")
	}
	// The RPC only permits edits to its own user config. Initialize against an
	// empty home first, then move the staged TOML into that home: the editor must
	// not load user plugins, relative profiles or telemetry during startup.
	editorConfig := filepath.Join(editorHome, "config.toml")
	if err := os.Rename(path, editorConfig); err != nil {
		return errors.New("cannot prepare Codex editor input")
	}
	var edits []map[string]any
	for key, value := range map[string]any{
		"forced_login_method": "chatgpt",
		"model_provider":      "teo_ai",
		"model_providers.teo_ai": map[string]any{
			"name": "teo-ai", "base_url": strings.TrimRight(cfg.GatewayURL, "/") + "/codex",
			"wire_api": "responses", "requires_openai_auth": true, "supports_websockets": false,
			"env_http_headers": map[string]string{keyHeader: "TEO_AI_GATEWAY_KEY"},
		},
	} {
		edits = append(edits, map[string]any{"keyPath": key, "value": value, "mergeStrategy": "replace"})
	}
	if err := call(2, "config/batchWrite", map[string]any{"filePath": editorConfig, "edits": edits}); err != nil {
		return err
	}
	return os.Rename(editorConfig, path)
}
