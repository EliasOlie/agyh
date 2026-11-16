package agentkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Installing skills into agents is the only place agyh writes to an agent's
// files, so it follows strict rules:
//
//   - it only happens when the user enabled it ("manage_agents") and asked
//     for it (agyh skills link);
//   - a folder agyh did not create is never touched, even if it has the same name;
//   - a folder agyh created but the user edited since is never touched either;
//   - every install is recorded in a ledger (agyh's data dir), so it can be
//     listed and undone, and only agyh's own copies are ever replaced or removed.
//
// Skills are copied, not symlinked: Claude Code doesn't document symlinked
// skills, and links are fragile across WSL/Windows. The copy is the source's
// exact files (Pack/Unpack), verified against the source's fingerprint.

// SkillTarget is an agent that skills can be installed into.
type SkillTarget interface {
	Home
	// UserSkillsDir is where the agent reads user-installed skills from.
	UserSkillsDir() string
}

func (c *Claude) UserSkillsDir() string { return filepath.Join(c.cfg, "skills") }
func (c *Codex) UserSkillsDir() string  { return c.agentsSkills }

// ErrNotATarget explains why an agent cannot receive skills.
var ErrNotATarget = errors.New("cannot install skills into this agent")

// Target returns h as a SkillTarget, or an error saying why it isn't one.
func Target(h Home) (SkillTarget, error) {
	if t, ok := h.(SkillTarget); ok {
		return t, nil
	}
	if h.Name() == "agy" {
		return nil, fmt.Errorf("%w: Antigravity has no documented folder for user skills (only plugins); agyh reads its skills but can't install into it yet. PRs welcome", ErrNotATarget)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotATarget, h.Name())
}

// Link is one skill folder agyh installed into an agent.
type Link struct {
	Agent       string    `json:"agent"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Digest      string    `json:"digest"` // content agyh wrote: anything else means the user edited it
	Source      string    `json:"source"` // where it was copied from, for humans
	InstalledAt time.Time `json:"installed_at"`
}

// Ledger records every skill folder agyh installed.
type Ledger struct {
	path  string
	Links []Link    `json:"links"`
	MCP   []MCPLink `json:"mcp,omitempty"`
}

// LoadLedger reads the ledger at path (empty if missing).
func LoadLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("%s: %w", shortHome(path), err)
	}
	return l, nil
}

// Save writes the ledger atomically, owner-only.
func (l *Ledger) Save() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	slices.SortFunc(l.Links, func(a, b Link) int { return strings.Compare(a.Agent+a.Path, b.Agent+b.Path) })
	slices.SortFunc(l.MCP, func(a, b MCPLink) int { return strings.Compare(a.Agent+a.Name, b.Agent+b.Name) })
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Find returns the link recorded for path.
func (l *Ledger) Find(path string) (Link, bool) {
	for _, k := range l.Links {
		if k.Path == path {
			return k, true
		}
	}
	return Link{}, false
}

func (l *Ledger) put(k Link) {
	l.Links = slices.DeleteFunc(l.Links, func(o Link) bool { return o.Path == k.Path })
	l.Links = append(l.Links, k)
}

func (l *Ledger) drop(path string) {
	l.Links = slices.DeleteFunc(l.Links, func(o Link) bool { return o.Path == path })
}

// Step outcomes.
const (
	StepInstall   = "install"   // will create the folder
	StepUpdate    = "update"    // agyh's own, unedited copy will be replaced
	StepRemove    = "remove"    // agyh's own, unedited copy will be deleted
	StepUpToDate  = "uptodate"  // agyh's copy already matches
	StepPresent   = "present"   // an identical folder agyh didn't create: left alone
	StepUnmanaged = "unmanaged" // refused: folder exists and agyh didn't create it
	StepModified  = "modified"  // refused: agyh created it but the user edited it
	StepForget    = "forget"    // recorded folder is gone: only the record is dropped
)

// Step is one planned change. Plan* never write; Apply does.
type Step struct {
	Kind   string `json:"kind"`
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
	src    Skill
}

// Refused reports whether the step was refused to protect the user's files.
func (s Step) Refused() bool { return s.Kind == StepUnmanaged || s.Kind == StepModified }

var safeDirName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// PlanLink decides what installing src into target would do.
func (l *Ledger) PlanLink(src Skill, target SkillTarget, source string) (Step, error) {
	// The folder name, not the display name: it already is a valid directory
	// name on the source machine ("Agentic UX Design - …" names exist).
	dir := filepath.Base(src.Path)
	if !safeDirName.MatchString(dir) {
		return Step{}, fmt.Errorf("skill folder name %q is not safe to install", dir)
	}
	if src.Digest == "" {
		return Step{}, fmt.Errorf("skill %s has no fingerprint (too large to copy)", src.Name)
	}
	path := filepath.Join(target.UserSkillsDir(), dir)
	st := Step{Agent: target.Name(), Name: src.Name, Path: path, Source: source, src: src}

	_, statErr := os.Lstat(path)
	link, managed := l.Find(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		st.Kind = StepInstall
	case statErr != nil:
		return Step{}, statErr
	default:
		current, err := hashDir(path)
		switch {
		case !managed && err == nil && current == src.Digest:
			st.Kind = StepPresent
		case !managed:
			st.Kind = StepUnmanaged
		case err != nil || current != link.Digest:
			st.Kind = StepModified
		case current == src.Digest:
			st.Kind = StepUpToDate
		default:
			st.Kind = StepUpdate
		}
	}
	return st, nil
}

// PlanUnlink decides what removing agyh's copy at path would do.
func (l *Ledger) PlanUnlink(link Link) Step {
	st := Step{Agent: link.Agent, Name: link.Name, Path: link.Path, Source: link.Source}
	if _, err := os.Lstat(link.Path); errors.Is(err, fs.ErrNotExist) {
		st.Kind = StepForget
		return st
	}
	if current, err := hashDir(link.Path); err != nil || current != link.Digest {
		st.Kind = StepModified
		return st
	}
	st.Kind = StepRemove
	return st
}

// Apply performs a planned step and records it. Refused and no-op steps do
// nothing. Every write re-checks the plan's premise first, so a folder that
// changed between plan and apply is not clobbered.
func (l *Ledger) Apply(st Step) error {
	switch st.Kind {
	case StepInstall, StepUpdate:
		again, err := l.PlanLink(st.src, fakeTarget{agent: st.Agent, dir: filepath.Dir(st.Path)}, st.Source)
		if err != nil {
			return err
		}
		if again.Kind != st.Kind {
			return fmt.Errorf("%s changed while installing: left untouched", shortHome(st.Path))
		}
		archive, digest, err := Pack(st.src.Path)
		if err != nil {
			return err
		}
		if digest != st.src.Digest {
			return fmt.Errorf("source of %s changed since it was listed: run the command again", st.Name)
		}
		if err := os.MkdirAll(filepath.Dir(st.Path), 0o755); err != nil {
			return err
		}
		if err := Unpack(archive, st.Path, digest); err != nil {
			return err
		}
		l.put(Link{Agent: st.Agent, Name: st.Name, Path: st.Path, Digest: digest, Source: st.Source, InstalledAt: time.Now().UTC()})
		return l.Save()
	case StepRemove:
		link, ok := l.Find(st.Path)
		if !ok {
			return fmt.Errorf("%s is not managed by agyh", shortHome(st.Path))
		}
		if again := l.PlanUnlink(link); again.Kind != StepRemove {
			return fmt.Errorf("%s changed: left untouched", shortHome(st.Path))
		}
		if err := os.RemoveAll(st.Path); err != nil {
			return err
		}
		l.drop(st.Path)
		return l.Save()
	case StepForget:
		l.drop(st.Path)
		return l.Save()
	}
	return nil
}

// fakeTarget re-plans against a known directory without a full Home.
type fakeTarget struct{ agent, dir string }

func (f fakeTarget) Name() string                       { return f.agent }
func (f fakeTarget) Available() bool                    { return true }
func (f fakeTarget) Skills() ([]Skill, []error)         { return nil, nil }
func (f fakeTarget) MCPServers() ([]MCPServer, []error) { return nil, nil }
func (f fakeTarget) MCPConfig() ([]MCPEntry, []error)   { return nil, nil }
func (f fakeTarget) Plugins() ([]PluginRef, []error)    { return nil, nil }
func (f fakeTarget) UserSkillsDir() string              { return f.dir }

// ScanKit lists the skills restored into a kit directory (see sync.RestoreKit):
// <dir>/<agent>/skills/<name>/SKILL.md.
func ScanKit(dir, machine string) ([]Skill, []error) {
	agents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var (
		out  []Skill
		errs []error
	)
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		s, e := scanSkills(filepath.Join(dir, a.Name(), "skills"), a.Name(), "kit", "kit:"+machine)
		out, errs = append(out, s...), append(errs, e...)
	}
	return out, errs
}

// ReadSkillDir reads one skill folder (anywhere, e.g. a fetched repository).
func ReadSkillDir(dir, agent, scope, source string) (Skill, error) {
	s, err := readSkill(dir)
	s.Agent, s.Scope, s.Plugin = agent, scope, source
	return s, err
}

// LocalCatalog is a plugin marketplace catalog Claude Code has downloaded.
type LocalCatalog struct {
	Name   string // marketplace name
	File   string // its marketplace.json
	Source string // where the marketplace comes from (github:owner/repo), if known
}

// Catalogs lists the marketplace catalogs under plugins/marketplaces/.
func (c *Claude) Catalogs() []LocalCatalog {
	sources := c.marketplaceSources()
	dirs, _ := os.ReadDir(filepath.Join(c.plugins, "marketplaces"))
	var out []LocalCatalog
	for _, d := range dirs {
		f := filepath.Join(c.plugins, "marketplaces", d.Name(), ".claude-plugin", "marketplace.json")
		if fileExists(f) {
			out = append(out, LocalCatalog{Name: d.Name(), File: f, Source: sources[d.Name()]})
		}
	}
	return out
}
