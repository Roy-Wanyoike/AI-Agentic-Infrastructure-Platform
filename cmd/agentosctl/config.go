package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the on-disk CLI state (~/.agentos/config.json) plus the env
// overrides applied on load (AGENTOS_URL / AGENTOS_TOKEN / AGENTOS_API_KEY).
// Environment values always win over the file so CI can drive the CLI
// without touching a developer's saved profile.
type Config struct {
	URL    string `json:"url"`
	Token  string `json:"token,omitempty"`
	APIKey string `json:"api_key,omitempty"`
}

// Environment variables recognised by the CLI.
const (
	EnvURL    = "AGENTOS_URL"
	EnvToken  = "AGENTOS_TOKEN"
	EnvAPIKey = "AGENTOS_API_KEY"
	// EnvConfig overrides the config file location (used by tests and
	// users who want a non-default profile path).
	EnvConfig = "AGENTOS_CONFIG"
)

// ConfigPath resolves the config file location: $AGENTOS_CONFIG, or
// $HOME/.agentos/config.json.
func ConfigPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvConfig)); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".agentos", "config.json"), nil
}

// LoadConfig reads the config file. A missing file yields the zero Config
// with no error (first-run experience: `agentosctl login` creates it).
func LoadConfig() (Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig atomically persists the config (mkdir 0700, file 0600 — the
// token is a credential and must not be world-readable).
func SaveConfig(cfg Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// applyEnvOverrides returns cfg with AGENTOS_URL/AGENTOS_TOKEN/AGENTOS_API_KEY
// taking precedence over the file values.
func applyEnvOverrides(cfg Config) Config {
	if v := strings.TrimSpace(os.Getenv(EnvURL)); v != "" {
		cfg.URL = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvToken)); v != "" {
		cfg.Token = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvAPIKey)); v != "" {
		cfg.APIKey = v
	}
	return cfg
}

// effectiveConfig loads the config file and applies env overrides.
func effectiveConfig() (Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return Config{}, err
	}
	return applyEnvOverrides(cfg), nil
}
