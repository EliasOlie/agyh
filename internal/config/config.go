// Package config loads and saves ~/.config/agyh/config.json (mode 0600: it
// holds the Telegram bot token and the local bridge secret).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Telegram struct {
	Token  string `json:"token,omitempty"`
	ChatID int64  `json:"chat_id,omitempty"`
}

type WhatsApp struct {
	Enabled bool   `json:"enabled"`
	JID     string `json:"jid,omitempty"` // your own number, paired as a linked device
}

type Bridge struct {
	Addr   string `json:"addr"`
	Secret string `json:"secret,omitempty"`
}

type Policy struct {
	Safe  []string `json:"safe,omitempty"`
	Fatal []string `json:"fatal,omitempty"`
}

type Config struct {
	Bridge   Bridge   `json:"bridge"`
	Telegram Telegram `json:"telegram"`
	WhatsApp WhatsApp `json:"whatsapp"`
	Policy   Policy   `json:"policy"`
	// ApprovalTimeoutSec is how long a hook waits for a remote decision.
	ApprovalTimeoutSec int `json:"approval_timeout_sec"`
	// Runners start a new headless session from chat (/new). "{prompt}" is replaced.
	Runners map[string][]string `json:"runners"`
	// Workspaces offered by /ws and accepted by /new (absolute paths).
	Workspaces []string `json:"workspaces,omitempty"`

	path string
}

func Default() *Config {
	return &Config{
		Bridge:             Bridge{Addr: "127.0.0.1:9123"},
		ApprovalTimeoutSec: 120,
		Runners: map[string][]string{
			"claude": {"claude", "-p", "{prompt}"},
		},
	}
}

// Dir is $AGYH_CONFIG_DIR, else $XDG_CONFIG_HOME/agyh, else ~/.config/agyh.
func Dir() string {
	if v := os.Getenv("AGYH_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "agyh")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "agyh")
}

func Path() string { return filepath.Join(Dir(), "config.json") }

// Load reads the config, returning defaults when the file does not exist.
func Load() (*Config, error) { return LoadFrom(Path()) }

func LoadFrom(path string) (*Config, error) {
	c := Default()
	c.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Bridge.Addr == "" {
		c.Bridge.Addr = Default().Bridge.Addr
	}
	if c.ApprovalTimeoutSec <= 0 {
		c.ApprovalTimeoutSec = Default().ApprovalTimeoutSec
	}
	if c.Runners == nil {
		c.Runners = Default().Runners
	}
	return c, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save() error {
	if c.path == "" {
		c.path = Path()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// EnsureSecret generates the local bridge secret on first use.
func (c *Config) EnsureSecret() (bool, error) {
	if c.Bridge.Secret != "" {
		return false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return false, err
	}
	c.Bridge.Secret = hex.EncodeToString(buf)
	return true, c.Save()
}

func (c *Config) ApprovalTimeout() time.Duration {
	return time.Duration(c.ApprovalTimeoutSec) * time.Second
}
