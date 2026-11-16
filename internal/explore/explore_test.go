package explore_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/EliasOlie/agyh/internal/explore"
	. "github.com/EliasOlie/agyh/internal/explore/exploretest"
)

func TestParseRef(t *testing.T) {
	for in, want := range map[string]Ref{
		"github:o/r":                                  {Owner: "o", Repo: "r"},
		"o/r/skills/pdf@v1":                           {Owner: "o", Repo: "r", Path: "skills/pdf", Rev: "v1"},
		"https://github.com/o/r":                      {Owner: "o", Repo: "r"},
		"https://github.com/o/r.git":                  {Owner: "o", Repo: "r"},
		"https://github.com/o/r/tree/main/skills/pdf": {Owner: "o", Repo: "r", Path: "skills/pdf", Rev: "main"},
	} {
		if got, err := ParseRef(in); err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"o", "github:o", "o/r/../x", "o/r@bad rev", "github:o;rm/r", "o/r//x"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResolveAndFetch(t *testing.T) {
	f := NewFakeGitHub(t)
	c := f.Client("tok-SECRET")
	ctx := context.Background()
	r, _ := ParseRef("o/r/skills")
	sha, err := c.Resolve(ctx, r)
	if err != nil || sha != ShaHead {
		t.Fatalf("resolve: %s %v", sha, err)
	}
	if sha, _ := c.Resolve(ctx, Ref{Owner: "o", Repo: "r", Rev: "v1"}); sha != ShaV1 {
		t.Fatalf("resolve v1: %s", sha)
	}
	dest := filepath.Join(t.TempDir(), "src")
	if err := c.Fetch(ctx, r, sha, dest); err != nil {
		t.Fatal(err)
	}
	// Only the requested folder, top dir stripped, no links, exec bit kept.
	if _, err := os.Stat(filepath.Join(dest, "README.md")); !os.IsNotExist(err) {
		t.Fatal("files outside the path were extracted")
	}
	if _, err := os.Lstat(filepath.Join(dest, "pdf", "link")); !os.IsNotExist(err) {
		t.Fatal("a symlink was extracted")
	}
	if st, err := os.Stat(filepath.Join(dest, "pdf", "scripts", "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("script: %v", err)
	}
	if dirs := SkillDirs(dest); len(dirs) != 2 || filepath.Base(dirs[0]) != "pdf" {
		t.Fatalf("skill dirs: %v", dirs)
	}
	if err := c.Fetch(ctx, r, sha, dest); err == nil {
		t.Fatal("fetch over an existing folder")
	}
	if err := c.Fetch(ctx, Ref{Owner: "o", Repo: "r", Path: "nope"}, sha, filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("missing path accepted")
	}

	// The token goes to the API and codeload only, never to other hosts' paths.
	if f.AuthSeen["/api/repos/o/r/commits/HEAD"] != "Bearer tok-SECRET" {
		t.Fatal("token not sent to the API")
	}
	c.FetchCatalog(ctx, "github:o/cat")
	if f.AuthSeen["/raw/o/cat/HEAD/.claude-plugin/marketplace.json"] != "" {
		t.Fatal("token sent to raw content")
	}
}

func TestFetchRejectsHostileArchives(t *testing.T) {
	f := NewFakeGitHub(t)
	root := t.TempDir()
	dest := filepath.Join(root, "src")
	err := f.Client("").Fetch(context.Background(), Ref{Owner: "o", Repo: "evil"}, ShaHead, dest)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("hostile archive: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("something was written: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside")); !os.IsNotExist(err) {
		t.Fatal("wrote outside")
	}
}

func TestSearch(t *testing.T) {
	f := NewFakeGitHub(t)
	c := f.Client("")
	ctx := context.Background()
	m, home, err := c.FetchCatalog(ctx, "github:o/cat")
	if err != nil || home != "github:o/cat" {
		t.Fatal(err)
	}
	got := map[string]Result{}
	for _, r := range SearchCatalog(m, home, []string{"PDF"}) {
		got[r.Name] = r
	}
	if got["pdfplug"].Source != "github:o/cat/skills/pdf" || got["pdfplug"].Install != "agyh skills install pdfplug@cat" {
		t.Fatalf("relative source: %+v", got["pdfplug"])
	}
	if got["npmthing"].Source != "" || got["npmthing"].Install != "" {
		t.Fatalf("non-GitHub plugin must not be installable here: %+v", got["npmthing"])
	}
	if got["escape"].Source != "" {
		t.Fatalf("escaping relative path accepted: %+v", got["escape"])
	}
	if _, ok := got["remote"]; ok {
		t.Fatal("non-matching entry returned")
	}
	if r := SearchCatalog(m, home, []string{"web"}); len(r) != 1 || r[0].Source != "github:o/r/skills/web" {
		t.Fatalf("object source: %+v", r)
	}

	gh, err := c.SearchGitHub(ctx, []string{"pdf"}, 10)
	if err != nil || len(gh) != 2 || gh[0].Name != "z/big" || gh[1].Install != "agyh skills install github:o/r" {
		t.Fatalf("github: %+v %v", gh, err)
	}
}
