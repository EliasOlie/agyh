package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/explore"
	"github.com/EliasOlie/agyh/internal/explore/exploretest"
)

func TestSkillsSearchAndInstall(t *testing.T) {
	home, _ := env(t)
	gh := exploretest.NewFakeGitHub(t)
	old := newExplorer
	t.Cleanup(func() { newExplorer = old })
	newExplorer = func() *explore.Client { return gh.Client("") }
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	writeConfig(t, map[string]any{"catalogs": []string{"github:o/cat"}})

	out := mustRun(t, "skills", "search", "pdf")
	for _, want := range []string{"pdfplug", "cat", "agyh skills install pdfplug@cat", "z/big", "99", "agyh skills install github:o/r", "npmthing", "not on GitHub"} {
		if !strings.Contains(out, want) {
			t.Fatalf("search is missing %q:\n%s", want, out)
		}
	}
	var results []explore.Result
	if err := json.Unmarshal([]byte(mustRun(t, "skills", "search", "pdf", "--offline", "--json")), &results); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Catalog == "github" {
			t.Fatal("--offline must not query GitHub")
		}
	}

	// Dry run: fetch and preview, install nothing (allowed without opt-in).
	out = mustRun(t, "skills", "install", "github:o/r/skills", "--dry-run")
	for _, want := range []string{"pinned to commit " + exploretest.ShaHead[:12], "/tree/" + exploretest.ShaHead + "/skills",
		"• pdf: Work with PDFs.", "scripts your agent could run: scripts/run.sh", "• web: Browse.", "nothing was installed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview is missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "pdf")); !os.IsNotExist(err) {
		t.Fatal("dry run installed")
	}

	if _, err := run(t, "", "skills", "install", "github:o/r/skills", "--to", "claude"); err == nil || !strings.Contains(err.Error(), "manage_agents") {
		t.Fatalf("opt-in: %v", err)
	}
	writeConfig(t, map[string]any{"catalogs": []string{"github:o/cat"}, "manage_agents": true})

	// Asked to confirm; "n" installs nothing.
	out, err := run(t, "n\n", "skills", "install", "github:o/r/skills", "--to", "claude")
	if err != nil || !strings.Contains(out, "Install 2 skill(s) into claude? [y/N]") || !strings.Contains(out, "Cancelled") {
		t.Fatalf("decline: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "pdf")); !os.IsNotExist(err) {
		t.Fatal("installed after a no")
	}

	out, err = run(t, "y\n", "skills", "install", "github:o/r/skills", "--to", "claude,codex")
	if err != nil || !strings.Contains(out, "+ claude: pdf installed") || !strings.Contains(out, "+ codex: web installed") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if st, err := os.Stat(filepath.Join(home, ".claude", "skills", "pdf", "scripts", "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("script not installed executable: %v", err)
	}
	var links []struct{ Agent, Name, Source string }
	json.Unmarshal([]byte(mustRun(t, "skills", "links", "--json")), &links)
	if len(links) != 4 || links[0].Source != "github:o/r@"+exploretest.ShaHead[:12] {
		t.Fatalf("pinned source not recorded: %+v", links)
	}

	// From a catalog entry (relative path in the catalog's repo); same content: up to date.
	if out := mustRun(t, "skills", "install", "remote@cat", "--to", "claude", "--yes"); !strings.Contains(out, "= claude: web already up to date") {
		t.Fatalf("catalog install:\n%s", out)
	}
	if _, err := run(t, "", "skills", "install", "npmthing@cat", "--to", "claude", "-y"); err == nil || !strings.Contains(err.Error(), "not on GitHub") {
		t.Fatalf("non-GitHub plugin: %v", err)
	}
	if _, err := run(t, "", "skills", "install", "x@nocat", "--to", "claude", "-y"); err == nil || !strings.Contains(err.Error(), "unknown catalog") {
		t.Fatalf("unknown catalog: %v", err)
	}
	if _, err := run(t, "", "skills", "install", "github:o/evil", "--to", "claude", "-y"); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("hostile repo: %v", err)
	}
	if _, err := run(t, "", "skills", "install", "github:o/r", "-y"); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("missing --to: %v", err)
	}
}
