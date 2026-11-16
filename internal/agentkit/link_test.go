package agentkit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type linkEnv struct {
	t      *testing.T
	ledger *Ledger
	claude *Claude
	src    Skill
}

func newLinkEnv(t *testing.T) *linkEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_PLUGIN_CACHE_DIR", "")
	srcDir := filepath.Join(home, ".gemini", "config", "plugins", "ponytail", "skills", "ponytail")
	write(t, filepath.Join(srcDir, "SKILL.md"), skillMD("ponytail", "Lazy mode."))
	write(t, filepath.Join(srcDir, "scripts", "run.sh"), "echo v1\n")
	src, err := readSkill(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	src.Agent, src.Scope, src.Plugin = "agy", ScopePlugin, "ponytail"
	ledger, err := LoadLedger(filepath.Join(home, "data", "links.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	return &linkEnv{t: t, ledger: ledger, claude: NewClaude(home), src: src}
}

func (e *linkEnv) plan() Step {
	e.t.Helper()
	st, err := e.ledger.PlanLink(e.src, e.claude, "agy plugin ponytail")
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *linkEnv) apply(st Step) {
	e.t.Helper()
	if err := e.ledger.Apply(st); err != nil {
		e.t.Fatal(err)
	}
}

func (e *linkEnv) target() string { return filepath.Join(e.claude.UserSkillsDir(), "ponytail") }

func (e *linkEnv) editSource(body string) {
	e.t.Helper()
	write(e.t, filepath.Join(e.src.Path, "scripts", "run.sh"), body)
	s, err := readSkill(e.src.Path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.src.Digest, e.src.Hash = s.Digest, s.Hash
}

func TestLinkInstallUpdateUnlink(t *testing.T) {
	e := newLinkEnv(t)

	st := e.plan()
	if st.Kind != StepInstall || st.Path != e.target() {
		t.Fatalf("plan: %+v", st)
	}
	e.apply(st)
	if got, _ := hashDir(e.target()); got != e.src.Digest {
		t.Fatal("installed copy differs from the source")
	}
	if l, ok := e.ledger.Find(e.target()); !ok || l.Digest != e.src.Digest || l.Source != "agy plugin ponytail" {
		t.Fatalf("not recorded: %+v", l)
	}
	// The ledger persists, owner-only.
	if st, err := os.Stat(e.ledger.path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("ledger file: %v %v", st, err)
	}
	reloaded, _ := LoadLedger(e.ledger.path)
	if len(reloaded.Links) != 1 {
		t.Fatalf("ledger not saved: %+v", reloaded)
	}

	if st := e.plan(); st.Kind != StepUpToDate {
		t.Fatalf("second link: %+v", st)
	}

	// The source changed upstream: agyh's untouched copy is updated.
	e.editSource("echo v2\n")
	st = e.plan()
	if st.Kind != StepUpdate {
		t.Fatalf("after source change: %+v", st)
	}
	e.apply(st)
	if b, _ := os.ReadFile(filepath.Join(e.target(), "scripts", "run.sh")); string(b) != "echo v2\n" {
		t.Fatalf("not updated: %q", b)
	}

	link, _ := e.ledger.Find(e.target())
	if st := e.ledger.PlanUnlink(link); st.Kind != StepRemove {
		t.Fatalf("unlink plan: %+v", st)
	} else {
		e.apply(st)
	}
	if _, err := os.Stat(e.target()); !os.IsNotExist(err) {
		t.Fatal("unlink did not remove agyh's copy")
	}
	if len(e.ledger.Links) != 0 {
		t.Fatal("unlink did not drop the record")
	}
}

// A folder agyh did not create is never touched, even with the same name.
func TestLinkNeverTouchesUnmanagedFolders(t *testing.T) {
	e := newLinkEnv(t)
	write(t, filepath.Join(e.target(), "SKILL.md"), "the user's own ponytail\n")
	st := e.plan()
	if st.Kind != StepUnmanaged || !st.Refused() {
		t.Fatalf("plan: %+v", st)
	}
	e.apply(st) // a refused step is a no-op
	if b, _ := os.ReadFile(filepath.Join(e.target(), "SKILL.md")); string(b) != "the user's own ponytail\n" {
		t.Fatal("user's folder was modified")
	}
	if len(e.ledger.Links) != 0 {
		t.Fatal("refused step was recorded")
	}

	// Identical content that agyh didn't create: reported, left alone, not adopted.
	os.RemoveAll(e.target())
	archive, digest, _ := Pack(e.src.Path)
	Unpack(archive, e.target(), digest)
	if st := e.plan(); st.Kind != StepPresent {
		t.Fatalf("identical unmanaged: %+v", st)
	}
	if len(e.ledger.Links) != 0 {
		t.Fatal("agyh must not take ownership of a folder it didn't create")
	}
}

// A copy the user edited after agyh installed it is theirs now.
func TestLinkAndUnlinkRespectUserEdits(t *testing.T) {
	e := newLinkEnv(t)
	e.apply(e.plan())
	write(t, filepath.Join(e.target(), "notes.md"), "my tweak\n")

	e.editSource("echo v2\n")
	if st := e.plan(); st.Kind != StepModified || !st.Refused() {
		t.Fatalf("update over user edits: %+v", st)
	}
	link, _ := e.ledger.Find(e.target())
	st := e.ledger.PlanUnlink(link)
	if st.Kind != StepModified {
		t.Fatalf("unlink over user edits: %+v", st)
	}
	e.apply(st)
	if b, _ := os.ReadFile(filepath.Join(e.target(), "notes.md")); string(b) != "my tweak\n" {
		t.Fatal("user's edit was lost")
	}
}

// If the folder changes between plan and apply, apply re-checks and refuses.
func TestApplyRechecksBeforeWriting(t *testing.T) {
	e := newLinkEnv(t)
	st := e.plan() // install
	write(t, filepath.Join(e.target(), "SKILL.md"), "created by the user meanwhile\n")
	if err := e.ledger.Apply(st); err == nil {
		t.Fatal("apply clobbered a folder created after planning")
	}
	if b, _ := os.ReadFile(filepath.Join(e.target(), "SKILL.md")); string(b) != "created by the user meanwhile\n" {
		t.Fatal("user's folder was replaced")
	}

	// Source edited after planning: refused instead of installing the wrong digest.
	os.RemoveAll(e.target())
	st = e.plan()
	write(t, filepath.Join(e.src.Path, "SKILL.md"), "changed after planning\n")
	if err := e.ledger.Apply(st); err == nil {
		t.Fatal("installed a source that changed after planning")
	}
	if _, err := os.Stat(e.target()); !os.IsNotExist(err) {
		t.Fatal("partial install left behind")
	}
}

func TestUnlinkForgetsMissingFolders(t *testing.T) {
	e := newLinkEnv(t)
	e.apply(e.plan())
	os.RemoveAll(e.target())
	link, _ := e.ledger.Find(e.target())
	st := e.ledger.PlanUnlink(link)
	if st.Kind != StepForget {
		t.Fatalf("missing folder: %+v", st)
	}
	e.apply(st)
	if len(e.ledger.Links) != 0 {
		t.Fatal("record not dropped")
	}
}

func TestLinkTargets(t *testing.T) {
	if _, err := Target(NewAgy(t.TempDir())); !errors.Is(err, ErrNotATarget) {
		t.Fatalf("agy must not be a target: %v", err)
	}
	for _, h := range []Home{NewClaude(t.TempDir()), NewCodex(t.TempDir())} {
		if _, err := Target(h); err != nil {
			t.Fatalf("%s: %v", h.Name(), err)
		}
	}
	e := newLinkEnv(t)
	bad := e.src
	bad.Path = filepath.Join(filepath.Dir(bad.Path), "Has Spaces")
	if _, err := e.ledger.PlanLink(bad, e.claude, "x"); err == nil {
		t.Fatal("unsafe folder name accepted")
	}
	bad = e.src
	bad.Digest = ""
	if _, err := e.ledger.PlanLink(bad, e.claude, "x"); err == nil {
		t.Fatal("unfingerprinted skill accepted")
	}
}

func TestScanKit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "claude", "skills", "seo", "SKILL.md"), skillMD("seo", "x"))
	write(t, filepath.Join(dir, "codex", "skills", "lint", "SKILL.md"), skillMD("lint", "y"))
	write(t, filepath.Join(dir, "plugins.json"), "{}")
	s, errs := ScanKit(dir, "laptop")
	if len(errs) != 0 || len(s) != 2 {
		t.Fatalf("%+v %v", s, errs)
	}
	for _, sk := range s {
		if sk.Scope != "kit" || sk.Plugin != "kit:laptop" || (sk.Agent != "claude" && sk.Agent != "codex") {
			t.Fatalf("%+v", sk)
		}
	}
	if s, errs := ScanKit(filepath.Join(dir, "missing"), "x"); s != nil || errs != nil {
		t.Fatal("missing kit dir must be empty, not an error")
	}
}
