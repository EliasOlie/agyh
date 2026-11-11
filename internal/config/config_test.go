package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agyh", "config.json")
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bridge.Addr != "127.0.0.1:9123" || c.ApprovalTimeoutSec != 120 || len(c.Runners["claude"]) == 0 {
		t.Fatalf("defaults: %+v", c)
	}
	created, err := c.EnsureSecret()
	if err != nil || !created || len(c.Bridge.Secret) != 64 {
		t.Fatalf("secret: %v %v %q", created, err, c.Bridge.Secret)
	}
	if again, _ := c.EnsureSecret(); again {
		t.Fatal("secret regenerated")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config must be 0600, got %v (%v)", st.Mode().Perm(), err)
	}
	c2, err := LoadFrom(path)
	if err != nil || c2.Bridge.Secret != c.Bridge.Secret {
		t.Fatalf("reload: %+v %v", c2, err)
	}
}
