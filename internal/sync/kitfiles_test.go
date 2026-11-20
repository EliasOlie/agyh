package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/source"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

// A reinstall under another user name: instructions, memories, subagents
// and a project skill come back where the agents look, nothing local is
// overwritten, and the project skill never becomes a global one.
func TestKitCarriesAgentFilesAndProjectSkills(t *testing.T) {
	_, cfg := newVault(t)
	ctx := context.Background()
	oldHome := os.Getenv("HOME")
	ws := filepath.Join(oldHome, "work", "conecta-me")

	write(t, filepath.Join(oldHome, ".claude", "CLAUDE.md"), "Responda em PT-BR.")
	write(t, filepath.Join(oldHome, ".claude", "agents", "revisor.md"), "---\nname: revisor\n---\nRevise.")
	mem := filepath.Join(oldHome, ".claude", "projects", source.ClaudeProjectDir(ws), "memory")
	write(t, filepath.Join(mem, "MEMORY.md"), "- [deploy](deploy.md)")
	write(t, filepath.Join(mem, "deploy.md"), "deploy via coolify")
	write(t, filepath.Join(oldHome, ".claude", "projects", source.ClaudeProjectDir(ws), "abc.jsonl"), "transcript: not a kit file")
	write(t, filepath.Join(oldHome, ".gemini", "config", "AGENTS.md"), "agy rules")
	write(t, filepath.Join(ws, ".claude", "skills", "relatorio", "SKILL.md"), "---\nname: relatorio\ndescription: relatório\n---\nfaça")
	write(t, filepath.Join(ws, ".claude", "skills", "relatorio", "assets", "logo.svg"), "<svg/>")

	a := newMachine(t, cfg)
	a.e.kitHomes = func() []agentkit.Home { return nil } // global skills are covered elsewhere
	a.add(t, model.Session{Agent: "claude", ID: "s1", Title: "relatório", Workspace: ws, StartedAt: base, UpdatedAt: base, Path: "synced:"})
	res, err := a.e.PushKit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 5 || res.ProjectSkills != 1 {
		t.Fatalf("backup: %d files, %d project skills (%v)", res.Files, res.ProjectSkills, res.Warnings)
	}

	// The new machine: another home, the repository cloned, a CLAUDE.md of its own.
	newHome := t.TempDir()
	t.Setenv("HOME", newHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(newHome, "data"))
	newWS := filepath.Join(newHome, "work", "conecta-me")
	os.MkdirAll(newWS, 0o755)
	write(t, filepath.Join(newHome, ".claude", "CLAUDE.md"), "my new rules")

	b := newMachine(t, cfg)
	kit, err := b.e.RestoreKit(ctx, a.e.machine())
	if err != nil || kit.Files != 5 {
		t.Fatalf("restore: %+v %v", kit, err)
	}
	if skills, _ := agentkit.ScanKit(kit.Dir, kit.Machine); len(skills) != 0 {
		t.Fatalf("a project skill would be linked as global: %+v", skills)
	}
	snap, err := ReadKit(kit.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if steps := ApplyKit(kit.Dir, snap, true); len(steps) != 6 {
		t.Fatalf("dry run: %+v", steps)
	}
	if _, err := os.Stat(filepath.Join(newHome, ".gemini", "config", "AGENTS.md")); err == nil {
		t.Fatal("dry run wrote a file")
	}
	status := map[string]string{}
	for _, s := range ApplyKit(kit.Dir, snap, false) {
		status[s.Name] = s.Status
	}
	newMem := filepath.Join(newHome, ".claude", "projects", source.ClaudeProjectDir(newWS), "memory")
	if read(t, filepath.Join(newMem, "deploy.md")) != "deploy via coolify" {
		t.Fatal("memory not moved to the new home's folder")
	}
	if read(t, filepath.Join(newHome, ".gemini", "config", "AGENTS.md")) != "agy rules" ||
		read(t, filepath.Join(newHome, ".claude", "agents", "revisor.md")) == "" {
		t.Fatal("instructions or subagent missing")
	}
	if read(t, filepath.Join(newHome, ".claude", "CLAUDE.md")) != "my new rules" || status["CLAUDE.md"] != KitKept {
		t.Fatalf("a local file was overwritten: %v", status)
	}
	if read(t, filepath.Join(newWS, ".claude", "skills", "relatorio", "assets", "logo.svg")) != "<svg/>" || status["relatorio"] != KitRestored {
		t.Fatalf("project skill: %v", status)
	}
	// Again: everything is already here (or kept).
	for _, s := range ApplyKit(kit.Dir, snap, false) {
		if s.Status != KitPresent && s.Status != KitKept {
			t.Fatalf("second apply: %+v", s)
		}
	}
	if _, err := os.Stat(filepath.Join(newHome, ".claude", "projects", source.ClaudeProjectDir(newWS), "abc.jsonl")); err == nil {
		t.Fatal("a transcript travelled as a kit file")
	}
}

// Paths come from the vault: one escaping its folder is refused.
func TestKitApplyRefusesUnsafePaths(t *testing.T) {
	isolateAgents(t)
	steps := ApplyKit(t.TempDir(), KitSnapshot{Files: []agentkit.AgentFile{
		{Agent: "claude", Kind: agentkit.FileInstructions, Rel: "../.bashrc", Digest: "x"},
		{Agent: "claude", Kind: agentkit.FileInstructions, Rel: "/etc/passwd", Digest: "x"},
	}, Skills: []KitSkill{{Agent: "claude", Scope: agentkit.ScopeProject, Name: "../../evil", Workspace: t.TempDir()}}}, false)
	for _, s := range steps {
		if s.Status != KitFailed {
			t.Fatalf("accepted: %+v", s)
		}
	}
	if !strings.Contains(steps[0].Detail, "unsafe") {
		t.Fatalf("%+v", steps[0])
	}
}
