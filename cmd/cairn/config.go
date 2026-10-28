package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aloisdeniel/cairn/internal/client"
)

// cliConfig is stored at ~/.config/cairn/config.json after `cairn login`.
type cliConfig struct {
	Host  string `json:"host"`
	Token string `json:"token"`
	// Pushed remembers which artifact each directory was pushed to, keyed
	// by "<host> <absolute dir>", so `cairn push <dir>` needs no --artifact
	// the next time.
	Pushed map[string]string `json:"pushed,omitempty"`
}

// saveLogin stores new credentials, keeping the rest of the configuration.
func saveLogin(host, token string) error {
	cfg := loadConfig()
	cfg.Host, cfg.Token = host, token
	return saveConfig(cfg)
}

func pushKey(host, dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return host + " " + filepath.Clean(dir)
}

// pushedArtifact is the artifact dir was last pushed to on host, if any.
func pushedArtifact(host, dir string) string {
	return loadConfig().Pushed[pushKey(host, dir)]
}

// rememberPush records where dir was pushed. Failing to save is not worth
// failing the push for.
func rememberPush(host, dir, artifactID string) {
	cfg := loadConfig()
	if cfg.Pushed == nil {
		cfg.Pushed = map[string]string{}
	}
	cfg.Pushed[pushKey(host, dir)] = artifactID
	saveConfig(cfg)
}

func configPath() (string, error) {
	if p := os.Getenv("CAIRN_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cairn", "config.json"), nil
}

func loadConfig() cliConfig {
	var cfg cliConfig
	path, err := configPath()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg cliConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

// apiClient builds a client from, in priority order: CAIRN_HOST/CAIRN_API_KEY
// environment (headless agents), then the stored login.
func apiClient() (*client.Client, error) {
	host := os.Getenv("CAIRN_HOST")
	token := os.Getenv("CAIRN_API_KEY")
	if host != "" && token != "" {
		return client.New(host, token), nil
	}
	cfg := loadConfig()
	if host == "" {
		host = cfg.Host
	}
	if token == "" {
		token = cfg.Token
	}
	if host == "" {
		return nil, errors.New("not logged in: run 'cairn login --host <url>' or set CAIRN_HOST and CAIRN_API_KEY")
	}
	if token == "" {
		return nil, fmt.Errorf("no credentials for %s: run 'cairn login' or set CAIRN_API_KEY", host)
	}
	return client.New(host, token), nil
}

func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
