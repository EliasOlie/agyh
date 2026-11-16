package sync

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/config"
)

const (
	kitSecret     = "sk-KIT-SECRET-VALUE"
	skillBodyText = "UNIQUE-SKILL-BODY-MARKER"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agentHome builds one machine's agents: Claude user skills (ponytail with an
// executable script and a venv, seo), an Antigravity plugin, and MCP servers
// with secrets in Claude, Antigravity and Codex.
func agentHome(t *testing.T, withSEO bool) string {
	t.Helper()
	home := t.TempDir()
	md := "---\nname: ponytail\ndescription: Lazy senior dev mode.\n---\n" + skillBodyText + "\n"
	put(t, filepath.Join(home, ".claude", "skills", "ponytail", "SKILL.md"), md)
	put(t, filepath.Join(home, ".claude", "skills", "ponytail", "scripts", "run.sh"), "echo hi\n")
	os.Chmod(filepath.Join(home, ".claude", "skills", "ponytail", "scripts", "run.sh"), 0o755)
	put(t, filepath.Join(home, ".claude", "skills", "ponytail", ".venv", "lib", "dep.py"), "not part of the skill")
	if withSEO {
		put(t, filepath.Join(home, ".claude", "skills", "seo", "SKILL.md"), "---\nname: seo\n---\nSEO audits.\n")
	}
	// Plugin skills are not copied: the plugin reference is.
	put(t, filepath.Join(home, ".gemini", "config", "plugins", "ponytail", "skills", "ponytail", "SKILL.md"), md)
	put(t, filepath.Join(home, ".gemini", "config", "import_manifest.json"),
		`{"imports":[{"name":"ponytail","source":"https://github.com/example/ponytail","components":["skills"]}]}`)
	put(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"github":{"command":"gh-mcp","env":{"GITHUB_TOKEN":"`+kitSecret+`"}}}}`)
	put(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), `{"mcpServers":{"janus":{"command":"janus","args":["--key","`+kitSecret+`"]}}}`)
	put(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.figma]\nurl = \"https://mcp.figma.com/mcp\"\nhttp_headers = { \"X-Key\" = \""+kitSecret+"\" }\n")
	return home
}

func homesOf(home string) func() []agentkit.Home {
	return func() []agentkit.Home {
		return []agentkit.Home{agentkit.NewClaude(home), agentkit.NewAgy(home), agentkit.NewCodex(home)}
	}
}

func kitMachine(t *testing.T, cfg config.Sync, name, home string) machine {
	t.Helper()
	cfg.Machine = name
	m := newMachine(t, cfg)
	m.e.kitHomes = homesOf(home)
	return m
}

func TestKitBackupAndRestore(t *testing.T) {
	v, cfg := newVault(t)
	ctx := context.Background()
	laptop := kitMachine(t, cfg, "laptop", agentHome(t, true))
	laptop.add(t, testSession(1))

	res := laptop.sync(t)
	k := res.Kit
	if k == nil || k.Machine != "laptop" || k.Skills != 2 || k.NewSkills != 2 || k.MCP != 3 || k.Plugins != 1 || len(k.Warnings) != 0 {
		t.Fatalf("kit backup: %+v", k)
	}

	// Everything in the bucket is ciphertext: no secret, no skill text.
	v.mu.Lock()
	skillBlobs := 0
	for key, obj := range v.objects {
		if bytes.Contains(obj, []byte(kitSecret)) || bytes.Contains(obj, []byte(skillBodyText)) {
			t.Errorf("%s holds plaintext", key)
		}
		if strings.Contains(key, "/kit/") && strings.HasSuffix(key, ".blob") && !bytes.HasPrefix(obj, magicEncryptedV2) {
			t.Errorf("%s is not a v2 blob", key)
		}
		if strings.Contains(key, "/kit/skills/") {
			skillBlobs++
		}
	}
	v.mu.Unlock()
	if skillBlobs != 2 {
		t.Fatalf("want 2 skill blobs, got %d", skillBlobs)
	}

	// Unchanged skills are not uploaded again; another machine with the same
	// skill reuses the stored blob.
	if res := laptop.sync(t); res.Kit.NewSkills != 0 {
		t.Fatalf("second backup re-uploaded: %+v", res.Kit)
	}
	desktop := kitMachine(t, cfg, "desktop", agentHome(t, false))
	if res := desktop.sync(t); res.Kit.Skills != 1 || res.Kit.NewSkills != 0 {
		t.Fatalf("identical skill on another machine: %+v", res.Kit)
	}
	if st := laptop.status(t); len(st.KitMachines) != 2 || st.Machine != "laptop" {
		t.Fatalf("status kit machines: %+v", st)
	}

	// A reinstalled machine: empty agents, fresh data dir.
	freshHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fresh := kitMachine(t, cfg, "laptop-new", freshHome)
	if _, err := fresh.e.RestoreKit(ctx, ""); !errors.Is(err, ErrNoKit) {
		t.Fatalf("no kit under the new name: %v", err)
	}
	r, err := fresh.e.RestoreKit(ctx, "laptop")
	if err != nil || r.Skills != 2 || r.MCP != 3 || r.Plugins != 1 || r.Dir != KitDir("laptop") {
		t.Fatalf("restore: %+v %v", r, err)
	}
	dir := r.Dir
	if b, _ := os.ReadFile(filepath.Join(dir, "claude", "skills", "ponytail", "SKILL.md")); !strings.Contains(string(b), skillBodyText) {
		t.Fatal("skill content not restored")
	}
	if st, err := os.Stat(filepath.Join(dir, "claude", "skills", "ponytail", "scripts", "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("script not restored executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude", "skills", "ponytail", ".venv")); !os.IsNotExist(err) {
		t.Fatal("dependency folders must not be backed up")
	}

	// MCP servers come back with their secrets, owner-only.
	for agent, wantName := range map[string]string{"claude": "github", "agy": "janus", "codex": "figma"} {
		p := filepath.Join(dir, agent, "mcp.json")
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
		raw, _ := os.ReadFile(p)
		var doc struct{ Servers []agentkit.MCPEntry }
		json.Unmarshal(raw, &doc)
		if len(doc.Servers) != 1 || doc.Servers[0].Name != wantName || !strings.Contains(string(doc.Servers[0].Raw), kitSecret) {
			t.Fatalf("%s mcp: %s", agent, raw)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("kit dir must be private: %v", st.Mode())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "plugins.json"))
	if !strings.Contains(string(raw), "https://github.com/example/ponytail") {
		t.Fatalf("plugin reference: %s", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "kit.json")); bytes.Contains(raw, []byte(kitSecret)) {
		t.Fatal("kit.json must not carry MCP secrets")
	}

	// Agents are never touched by a restore.
	if entries, _ := os.ReadDir(freshHome); len(entries) != 0 {
		t.Fatalf("restore wrote into the agents' home: %v", entries)
	}
	// Restoring again is idempotent.
	if _, err := fresh.e.RestoreKit(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
}

func TestKitDisabledSyncsConversationsOnly(t *testing.T) {
	v, cfg := newVault(t)
	off := false
	cfg.Kit = &off
	m := kitMachine(t, cfg, "box", agentHome(t, true))
	m.add(t, testSession(1))
	if res := m.sync(t); res.Kit != nil || res.Uploaded != 1 {
		t.Fatalf("kit disabled: %+v", res)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for key := range v.objects {
		if strings.Contains(key, "/kit/") {
			t.Fatalf("kit object written while disabled: %s", key)
		}
	}
}

func TestKitFailureKeepsConversationsAndReportsIt(t *testing.T) {
	v, cfg := newVault(t)
	m := kitMachine(t, cfg, "box", agentHome(t, true))
	m.add(t, testSession(1))
	v.failPut = func(key string) bool { return strings.Contains(key, "/kit/skills/") }
	res, err := m.e.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conversations synced") || res.Uploaded != 1 || res.Kit == nil {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if mf, _ := m.e.fetchManifest(context.Background()); len(mf.Sessions) != 1 {
		t.Fatal("conversations must be saved even when the kit fails")
	}
	if v.get("vault/kit/machines/box.blob") != nil {
		t.Fatal("a snapshot pointing at missing skill blobs must not be saved")
	}
	v.failPut = nil
	if res := m.sync(t); res.Kit.NewSkills != 2 {
		t.Fatalf("retry: %+v", res.Kit)
	}
}

// A blob swapped for another validly encrypted skill (someone with the
// passphrase, or a bug) must not be restored under the wrong name.
func TestKitRestoreVerifiesSkillDigest(t *testing.T) {
	v, cfg := newVault(t)
	ctx := context.Background()
	m := kitMachine(t, cfg, "box", agentHome(t, false))
	m.sync(t)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	tw.Write([]byte("evil"))
	tw.Close()
	keys, _ := m.e.ready(ctx, true)
	sealed, _ := keys.seal(buf.Bytes())
	v.mu.Lock()
	for key := range v.objects {
		if strings.Contains(key, "/kit/skills/") {
			v.objects[key] = sealed
		}
	}
	v.mu.Unlock()

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := m.e.RestoreKit(ctx, "box"); !errors.Is(err, agentkit.ErrBadArchive) {
		t.Fatalf("swapped blob accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(KitDir("box"), "claude", "skills", "ponytail")); !os.IsNotExist(err) {
		t.Fatal("a skill that failed verification was written")
	}
}

func TestMachineName(t *testing.T) {
	for in, want := range map[string]string{"laptop": "laptop", "Elias's MacBook Pro": "Elias-s-MacBook-Pro", "../../etc": "etc", "...": "machine"} {
		if got := MachineName(in); got != want {
			t.Errorf("MachineName(%q) = %q, want %q", in, got, want)
		}
	}
	if MachineName("") == "" {
		t.Fatal("hostname fallback")
	}
}
