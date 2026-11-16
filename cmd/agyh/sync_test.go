package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"

	"github.com/EliasOlie/agyh/internal/store"
)

// fakeS3 is a path-style in-memory bucket, enough for the vault engine.
func fakeS3(t *testing.T) string {
	t.Helper()
	var mu gosync.Mutex
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case http.MethodGet:
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(b)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSyncCommands(t *testing.T) {
	home, _ := env(t)
	t.Setenv("AGYH_SYNC_MACHINE", "work-laptop")
	// One user skill and one MCP server with a secret: the kit to back up.
	put(t, filepath.Join(home, ".claude", "skills", "ponytail", "SKILL.md"), "---\nname: ponytail\ndescription: Lazy mode.\n---\nbody\n")
	put(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"github":{"command":"gh-mcp","env":{"GITHUB_TOKEN":"sk-KITSECRET"}}}}`)
	endpoint := fakeS3(t)
	writeConfig(t, map[string]any{"sync": map[string]any{"enabled": true, "endpoint": endpoint, "bucket": "vault-test",
		"access_key": "ak", "secret_key": "sk", "passphrase": "a long test passphrase"}})

	// Status before anything is uploaded, in both spellings.
	for _, args := range [][]string{{"sync", "status"}, {"sync", "--status"}} {
		out := mustRun(t, args...)
		for _, want := range []string{"Endpoint:", endpoint, "vault-test", "Local sessions:", "To upload:", "v2: Argon2id, per-vault salt"} {
			if !strings.Contains(out, want) {
				t.Fatalf("agyh %s is missing %q:\n%s", strings.Join(args, " "), want, out)
			}
		}
	}
	var st struct {
		Local    int `json:"local_sessions"`
		ToUpload int `json:"to_upload"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "sync", "status", "--json")), &st); err != nil || st.Local != 2 || st.ToUpload != 2 {
		t.Fatalf("status --json: %+v %v", st, err)
	}
	if out := mustRun(t, "sync", "status"); !strings.Contains(out, "This machine:") || !strings.Contains(out, "work-laptop") || !strings.Contains(out, "none yet") {
		t.Fatalf("status kit lines:\n%s", out)
	}

	out := mustRun(t, "sync")
	for _, want := range []string{"✓ Synced with cloud vault (" + endpoint + "/vault-test)", "Uploaded:   2 sessions", "Total:      2 sessions in vault"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sync is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `Kit:        1 skills (1 new), 1 MCP servers, 0 plugins, as "work-laptop"`) || strings.Contains(out, "sk-KITSECRET") {
		t.Fatalf("sync kit line:\n%s", out)
	}
	if out := mustRun(t, "sync", "push"); out != "✓ Pushed 2 sessions to vault (2 total)\n  Kit:        1 skills (0 new), 1 MCP servers, 0 plugins, as \"work-laptop\"\n" {
		t.Fatalf("sync push: %q", out)
	}
	if out := mustRun(t, "sync", "--push"); !strings.Contains(out, "✓ Pushed to cloud vault") {
		t.Fatalf("sync --push: %q", out)
	}
	if out := mustRun(t, "sync", "pull"); out != "✓ Pulled 0 sessions from vault (2 total)\n" {
		t.Fatalf("sync pull: %q", out)
	}
	if out := mustRun(t, "sync", "--pull"); !strings.Contains(out, "✓ Pulled from cloud vault") {
		t.Fatalf("sync --pull: %q", out)
	}
	var res struct {
		Uploaded int `json:"uploaded"`
		Total    int `json:"total"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "sync", "push", "--json")), &res); err != nil || res.Uploaded != 2 || res.Total != 2 {
		t.Fatalf("push --json: %+v %v", res, err)
	}

	// A vault created by this version has nothing to migrate.
	if out := mustRun(t, "sync", "migrate"); out != "✓ Re-encrypted 0 sessions with this vault's own salt (Argon2id); 2 in vault\n" {
		t.Fatalf("sync migrate: %q", out)
	}
	if out := mustRun(t, "sync"); strings.Contains(out, "legacy") {
		t.Fatalf("no legacy hint expected on a v2 vault:\n%s", out)
	}
	if out := mustRun(t, "sync", "status"); !strings.Contains(out, "work-laptop: 1 skills, 1 MCP servers, 0 plugins") {
		t.Fatalf("status after backup:\n%s", out)
	}

	// Fresh machine, reinstalled under a new name: no agent files, empty index.
	fresh := filepath.Join(home, "fresh-home")
	t.Setenv("HOME", fresh)
	t.Setenv("AGYH_SYNC_MACHINE", "new-laptop")
	t.Setenv("AGYH_DB", filepath.Join(home, "fresh", "index.db"))
	t.Setenv("AGYH_CLAUDE_DIR", filepath.Join(home, "no-claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "fresh-data"))
	// Its own name has no kit yet: sessions come back, the kit error names the machines.
	out, err := run(t, "", "sync", "restore")
	if err == nil || !strings.Contains(err.Error(), "machines in the vault: work-laptop") || !strings.Contains(out, "✓ Restored 2 sessions") {
		t.Fatalf("restore without a kit for this name: %v\n%s", err, out)
	}
	out = mustRun(t, "sync", "restore", "--machine", "work-laptop")
	kitDir := filepath.Join(home, "fresh-data", "agyh", "kit", "work-laptop")
	if !strings.Contains(out, "✓ Restored the kit of work-laptop: 1 skills, 1 MCP servers, 0 plugins") || !strings.Contains(out, "not modified") {
		t.Fatalf("sync restore --machine:\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(kitDir, "claude", "skills", "ponytail", "SKILL.md")); err != nil || !strings.Contains(string(b), "Lazy mode") {
		t.Fatalf("restored skill: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(kitDir, "claude", "mcp.json")); err != nil || !strings.Contains(string(b), "sk-KITSECRET") {
		t.Fatalf("restored MCP config must carry its secrets: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("restore must not touch the agents: %v", err)
	}
	var hits []store.Hit
	if err := json.Unmarshal([]byte(mustRun(t, "search", "idempotencia", "--json")), &hits); err != nil || len(hits) != 1 || hits[0].ID != pixID {
		t.Fatalf("restored sessions not searchable: %+v %v", hits, err)
	}
	if out := mustRun(t, "show", "aaaa1111", "--last", "1"); !strings.Contains(out, "idempotência") {
		t.Fatalf("restored transcript not readable:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "fresh", "index.db")); err != nil {
		t.Fatal(err)
	}
}
