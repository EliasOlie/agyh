package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	home, ws, other string
	pol             Policy
}

func setup(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	f := fixture{home: home, ws: filepath.Join(home, "work", "app"), other: filepath.Join(home, "Documents")}
	for _, d := range []string{f.ws, f.other, filepath.Join(home, ".ssh"), filepath.Join(home, ".config", "agyh"),
		filepath.Join(home, "data", "inbox", "2026-11-19"), filepath.Join(f.ws, ".git")} {
		os.MkdirAll(d, 0o700)
	}
	f.pol = Policy{Home: home, Allow: []string{f.ws}, Protected: []string{filepath.Join(home, ".config", "agyh"), filepath.Join(home, "data")},
		Inbox: filepath.Join(home, "data", "inbox"), MaxBytes: 1 << 20}
	return f
}

func write(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkspaceFilesGoStraight(t *testing.T) {
	f := setup(t)
	write(t, filepath.Join(f.ws, "report.pdf"), "%PDF")
	c, err := f.pol.Check("report.pdf", f.ws)
	if err != nil || c.NeedsApproval || c.Size != 4 || c.Name != "report.pdf" {
		t.Fatalf("%+v %v", c, err)
	}
	// The current directory counts as a workspace too…
	p := write(t, filepath.Join(f.other, "brand.pdf"), "%PDF")
	if c, err := f.pol.Check("brand.pdf", f.other); err != nil || c.NeedsApproval {
		t.Fatalf("cwd: %+v %v", c, err)
	}
	// …but not when it is the home directory (that would allow everything).
	if c, err := f.pol.Check(p, f.home); err != nil || !c.NeedsApproval {
		t.Fatalf("home as cwd: %+v %v", c, err)
	}
	if c, err := f.pol.Check("~/Documents/brand.pdf", "/"); err != nil || !c.NeedsApproval {
		t.Fatalf("outside: %+v %v", c, err)
	}
}

func TestSecretsAreNeverSent(t *testing.T) {
	f := setup(t)
	secrets := []string{
		write(t, filepath.Join(f.ws, ".env"), "TOKEN=x"),
		write(t, filepath.Join(f.ws, ".env.production"), "TOKEN=x"),
		write(t, filepath.Join(f.ws, "server.pem"), "-----"),
		write(t, filepath.Join(f.ws, "service-account-prod.json"), "{}"),
		write(t, filepath.Join(f.ws, ".git", "config"), "[remote]"),
		write(t, filepath.Join(f.home, ".ssh", "id_ed25519"), "key"),
		write(t, filepath.Join(f.home, ".config", "agyh", "config.json"), "{}"),
		write(t, filepath.Join(f.home, "data", "index.db"), "db"),
	}
	for _, p := range secrets {
		if _, err := f.pol.Check(p, f.ws); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: want denied, got %v", p, err)
		}
	}
	// A harmless-looking link to a secret is judged by its target.
	link := filepath.Join(f.ws, "notes.txt")
	if err := os.Symlink(filepath.Join(f.home, ".ssh", "id_ed25519"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pol.Check(link, f.ws); !errors.Is(err, ErrDenied) {
		t.Errorf("symlink to a key: %v", err)
	}
	// Templates and the inbox are fine.
	for _, p := range []string{
		write(t, filepath.Join(f.ws, ".env.example"), "TOKEN="),
		write(t, filepath.Join(f.home, "data", "inbox", "2026-11-19", "foto.jpg"), "jpg"),
	} {
		if _, err := f.pol.Check(p, f.ws); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestSizeAndKind(t *testing.T) {
	f := setup(t)
	big := filepath.Join(f.ws, "big.bin")
	write(t, big, strings.Repeat("x", 2<<20))
	if _, err := f.pol.Check(big, f.ws); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := f.pol.Check(f.ws, f.ws); err == nil {
		t.Error("a directory was accepted")
	}
	if _, err := f.pol.Check("missing.pdf", f.ws); err == nil {
		t.Error("a missing file was accepted")
	}
	if _, err := f.pol.Check(write(t, filepath.Join(f.ws, "empty.txt"), ""), f.ws); err == nil {
		t.Error("an empty file was accepted")
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"../../.bashrc":       "bashrc",
		`C:\Users\x\nota.pdf`: "nota.pdf",
		"..":                  "file",
		"":                    "file",
		"a\x00b|c.txt":        "a_b_c.txt",
		"relatório final.pdf": "relatório final.pdf",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveNeverOverwritesAndStopsAtTheLimit(t *testing.T) {
	inbox := t.TempDir()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	p1, n, err := Save(inbox, "foto.jpg", strings.NewReader("one"), 100, now)
	if err != nil || n != 3 || filepath.Base(filepath.Dir(p1)) != "2026-11-19" {
		t.Fatalf("%s %d %v", p1, n, err)
	}
	p2, _, err := Save(inbox, "foto.jpg", strings.NewReader("two"), 100, now)
	if err != nil || filepath.Base(p2) != "foto (1).jpg" {
		t.Fatalf("second save: %s %v", p2, err)
	}
	if b, _ := os.ReadFile(p1); string(b) != "one" {
		t.Fatal("first file overwritten")
	}
	if fi, _ := os.Stat(p1); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if _, _, err := Save(inbox, "big.bin", strings.NewReader(strings.Repeat("x", 101)), 100, now); err == nil {
		t.Fatal("over the limit saved")
	}
	if _, err := os.Stat(filepath.Join(inbox, "2026-11-19", "big.bin")); err == nil {
		t.Fatal("partial file left behind")
	}
}
