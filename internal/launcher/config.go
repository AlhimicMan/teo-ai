package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	GatewayURL            string `json:"gateway_url"`
	KeyFile               string `json:"gateway_key_file"`
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify,omitempty"`
}

func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "teo-ai", "config.json"), nil
}

func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, errors.New("cannot open teo-ai config")
	}
	defer f.Close()
	var cfg Config
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid teo-ai config JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("invalid trailing config data")
	}
	return cfg, nil
}

func (c Config) Validate() error {
	u, err := url.Parse(c.GatewayURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("gateway must be an HTTPS origin, e.g. https://ai.example.com")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return errors.New("gateway requires HTTPS; HTTP is allowed only on loopback for local testing")
	}
	if c.KeyFile == "" {
		return errors.New("gateway key file is required (--key-file or TEO_AI_GATEWAY_KEY_FILE)")
	}
	return nil
}

func SaveConfig(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(cfg); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func ReadKey(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open gateway key file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", errors.New("cannot read gateway key file")
	}
	key := strings.TrimSpace(string(data))
	if len(key) < 32 || len(key) > 4096 {
		return "", errors.New("gateway key must have 32 to 4096 characters")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return "", errors.New("gateway key must contain printable ASCII without whitespace")
		}
	}
	return key, nil
}

func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if path == "" {
		return "", fmt.Errorf("empty file path")
	}
	return filepath.Abs(path)
}
