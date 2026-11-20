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

type Sync struct {
	Enabled    bool   `json:"enabled"`
	Endpoint   string `json:"endpoint,omitempty"` // S3 endpoint URL (e.g. https://api-vpsql.../s3)
	Bucket     string `json:"bucket,omitempty"`
	Region     string `json:"region,omitempty"` // default: us-east-1
	AccessKey  string `json:"access_key,omitempty"`
	SecretKey  string `json:"secret_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"` // for client-side zero-knowledge AES-256-GCM
	Prefix     string `json:"prefix,omitempty"`     // default: "vault"
	// AutoSync lets the bridge sync on its own: every Interval, and a few
	// minutes after an agent session ends. Default true; false turns it off.
	AutoSync *bool `json:"auto_sync,omitempty"`
	// Interval between automatic syncs, e.g. "6h" (default 6h, at least 15m).
	Interval string `json:"interval,omitempty"`
	// Machine names this machine's kit snapshot (default: hostname).
	Machine string `json:"machine,omitempty"`
	// Kit backs up skills, plugin references and MCP servers with every
	// sync. Default true; set false to sync conversations only.
	Kit *bool `json:"kit,omitempty"`
	// Transcripts stores each Claude Code / Codex session's own file (tool
	// calls included) so `agyh resume` works on another machine. Default true.
	Transcripts *bool `json:"transcripts,omitempty"`
}

// Files configures sending files to and from the phone.
type Files struct {
	// MaxMB caps a file in either direction (default 50, Telegram's limit).
	MaxMB int `json:"max_mb,omitempty"`
	// Allow lists extra directories whose files agents may send without
	// asking (workspaces and the temp dir always are).
	Allow []string `json:"allow,omitempty"`
}

// MaxBytes is MaxMB in bytes.
func (f Files) MaxBytes() int64 {
	if f.MaxMB <= 0 {
		return 50 << 20
	}
	return int64(f.MaxMB) << 20
}

// AutoSyncEnabled reports whether the bridge syncs on its own (default: yes).
func (s Sync) AutoSyncEnabled() bool { return s.Enabled && (s.AutoSync == nil || *s.AutoSync) }

// SyncInterval is the time between automatic syncs.
func (s Sync) SyncInterval() time.Duration {
	d, err := time.ParseDuration(s.Interval)
	if err != nil || d <= 0 {
		return 6 * time.Hour
	}
	return max(d, 15*time.Minute)
}

// TranscriptsEnabled reports whether syncs store each Claude Code / Codex
// session's own file, so it can be reopened on another machine (default: yes).
func (s Sync) TranscriptsEnabled() bool { return s.Transcripts == nil || *s.Transcripts }

// KitEnabled reports whether syncs include the agent kit (default: yes).
func (s Sync) KitEnabled() bool { return s.Kit == nil || *s.Kit }

type Config struct {
	Bridge   Bridge   `json:"bridge"`
	Telegram Telegram `json:"telegram"`
	WhatsApp WhatsApp `json:"whatsapp"`
	Policy   Policy   `json:"policy"`
	Sync     Sync     `json:"sync"`
	Files    Files    `json:"files,omitzero"`
	// ApprovalTimeoutSec is how long a hook waits for a remote decision.
	ApprovalTimeoutSec int `json:"approval_timeout_sec"`
	// Runners start a new headless session from chat (/new). "{prompt}" is replaced.
	Runners map[string][]string `json:"runners"`
	// Workspaces offered by /ws and accepted by /new (absolute paths).
	Workspaces []string `json:"workspaces,omitempty"`
	// ManageAgents allows agyh to install skills into agents' folders
	// (agyh skills link/unlink). Off by default: agyh only reads agent files.
	ManageAgents bool `json:"manage_agents,omitempty"`
	// AutoPropagate lets the bridge offer every new or changed skill to the
	// agents that lack it, asking on Telegram/WhatsApp first. Needs ManageAgents.
	AutoPropagate bool `json:"auto_propagate,omitempty"`
	// Catalogs are extra plugin marketplaces searched by `agyh skills search`:
	// github:owner/repo (its .claude-plugin/marketplace.json) or an https URL.
	Catalogs []string `json:"catalogs,omitempty"`

	path string
}

func Default() *Config {
	return &Config{
		Bridge:             Bridge{Addr: "127.0.0.1:9123"},
		ApprovalTimeoutSec: 120,
		Runners: map[string][]string{
			"claude": {"claude", "-p", "{prompt}"},
			"agy":    {"agy", "-p", "{prompt}"},
			// OpenAI's documented headless mode; untested against a real Codex install.
			"codex": {"codex", "exec", "{prompt}"},
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
		c.Runners = map[string][]string{}
	}
	// Agents added in later versions get their default runner unless the
	// config already defines one (or disables it with an empty list).
	for agent, argv := range Default().Runners {
		if _, ok := c.Runners[agent]; !ok {
			c.Runners[agent] = argv
		}
	}
	if v := os.Getenv("AGYH_SYNC_ENDPOINT"); v != "" {
		c.Sync.Endpoint = v
	}
	if v := os.Getenv("AGYH_SYNC_BUCKET"); v != "" {
		c.Sync.Bucket = v
	}
	if v := os.Getenv("AGYH_SYNC_REGION"); v != "" {
		c.Sync.Region = v
	}
	if v := os.Getenv("AGYH_SYNC_ACCESS_KEY"); v != "" {
		c.Sync.AccessKey = v
	}
	if v := os.Getenv("AGYH_SYNC_SECRET_KEY"); v != "" {
		c.Sync.SecretKey = v
	}
	if v := os.Getenv("AGYH_SYNC_PASSPHRASE"); v != "" {
		c.Sync.Passphrase = v
	}
	if v := os.Getenv("AGYH_SYNC_MACHINE"); v != "" {
		c.Sync.Machine = v
	}
	if v := os.Getenv("AGYH_SYNC_PREFIX"); v != "" {
		c.Sync.Prefix = v
	}
	if c.Sync.Region == "" {
		c.Sync.Region = "us-east-1"
	}
	if c.Sync.Prefix == "" {
		c.Sync.Prefix = "vault"
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
