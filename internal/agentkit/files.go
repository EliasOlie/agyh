package agentkit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Agent files are what makes an agent yours besides skills: global
// instructions (CLAUDE.md, AGENTS.md), Claude Code's memories, subagents and
// slash commands, Codex's custom prompts. They are plain files under the
// agent's folder; the kit backs them up and `agyh kit apply` puts back the
// ones a machine lacks.

// Kinds of agent files.
const (
	FileInstructions = "instructions"
	FileMemory       = "memory"
	FileSubagent     = "subagent"
	FileCommand      = "command"
	FilePrompt       = "prompt"
)

// ScopeProject: a skill inside a workspace (<ws>/.claude/skills, <ws>/.agents/skills).
const ScopeProject = "project"

// maxAgentFile caps one file (memories and instructions are small).
const maxAgentFile = 2 << 20

// AgentFile is one file of an agent's own folder.
type AgentFile struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Rel    string `json:"path"` // relative to the agent's folder, slash-separated
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // SHA-256 of the content
}

type filePattern struct {
	kind string
	re   *regexp.Regexp // matched against the slash-separated relative path
}

var agentFilePatterns = map[string][]filePattern{
	"claude": {
		{FileInstructions, regexp.MustCompile(`^CLAUDE\.md$`)},
		{FileInstructions, regexp.MustCompile(`^output-styles/[^/]+\.md$`)},
		{FileSubagent, regexp.MustCompile(`^agents/.+\.md$`)},
		{FileCommand, regexp.MustCompile(`^commands/.+\.md$`)},
		{FileMemory, regexp.MustCompile(`^projects/[^/]+/memory/.+`)},
	},
	"agy": {
		{FileInstructions, regexp.MustCompile(`^config/AGENTS\.md$`)},
		{FileInstructions, regexp.MustCompile(`^GEMINI\.md$`)},
	},
	"codex": {
		{FileInstructions, regexp.MustCompile(`^AGENTS\.md$`)},
		{FilePrompt, regexp.MustCompile(`^prompts/.+\.md$`)},
	},
}

// walkRoots: the folders under each agent's root that may hold its files
// (so a backup never walks Claude Code's whole history).
var walkRoots = map[string][]string{
	"claude": {"CLAUDE.md", "output-styles", "agents", "commands", "projects"},
	"agy":    {"config/AGENTS.md", "GEMINI.md"},
	"codex":  {"AGENTS.md", "prompts"},
}

// FileRoot is an agent's own folder on this machine.
func FileRoot(agent string) string {
	home, _ := os.UserHomeDir()
	switch agent {
	case "claude":
		return envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	case "agy":
		return filepath.Join(home, ".gemini")
	case "codex":
		return envOr("CODEX_HOME", filepath.Join(home, ".codex"))
	}
	return ""
}

// FileAgents are the agents with files to back up.
func FileAgents() []string { return []string{"claude", "agy", "codex"} }

// AgentFiles lists an agent's files on this machine.
func AgentFiles(agent string) ([]AgentFile, []error) {
	root := FileRoot(agent)
	if root == "" {
		return nil, nil
	}
	var (
		out  []AgentFile
		errs []error
	)
	for _, start := range walkRoots[agent] {
		top := filepath.Join(root, filepath.FromSlash(start))
		if _, err := os.Stat(top); err != nil {
			continue
		}
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				// Under projects/, only memory folders: never the transcripts.
				if agent == "claude" && strings.HasPrefix(rel, "projects/") {
					parts := strings.Split(rel, "/")
					if len(parts) == 3 && parts[2] != "memory" {
						return fs.SkipDir
					}
				}
				return nil
			}
			kind := ""
			for _, pat := range agentFilePatterns[agent] {
				if pat.re.MatchString(rel) {
					kind = pat.kind
					break
				}
			}
			if kind == "" || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if info.Size() > maxAgentFile {
				errs = append(errs, fmt.Errorf("%s: over %d MB, not backed up", shortHome(p), maxAgentFile>>20))
				return nil
			}
			digest, err := FileDigest(p)
			if err != nil {
				errs = append(errs, err)
				return nil
			}
			out = append(out, AgentFile{Agent: agent, Kind: kind, Rel: rel, Size: info.Size(), Digest: digest})
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	slices.SortFunc(out, func(a, b AgentFile) int { return strings.Compare(a.Rel, b.Rel) })
	return out, errs
}

// FileDigest is the SHA-256 of a file's content.
func FileDigest(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SafeRel reports whether a relative path from a snapshot stays inside its
// root (no "..", not absolute): snapshots come from the vault.
func SafeRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return false
	}
	c := path.Clean(rel)
	return c == rel && c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// ProjectSkills lists the skills inside a workspace: Claude Code's
// <ws>/.claude/skills and the Agent Skills folder <ws>/.agents/skills
// (Codex). Each gets Scope project.
func ProjectSkills(workspace string) ([]Skill, []error) {
	var out []Skill
	var errs []error
	for agent, dir := range map[string]string{"claude": ".claude/skills", "codex": ".agents/skills"} {
		s, e := scanSkills(filepath.Join(workspace, filepath.FromSlash(dir)), agent, ScopeProject, "")
		out, errs = append(out, s...), append(errs, e...)
	}
	slices.SortFunc(out, func(a, b Skill) int { return strings.Compare(a.Agent+a.Name, b.Agent+b.Name) })
	return out, errs
}

// ProjectSkillDir is where a project skill lives inside its workspace.
func ProjectSkillDir(workspace, agent, name string) (string, error) {
	sub := map[string]string{"claude": ".claude/skills", "codex": ".agents/skills"}[agent]
	if sub == "" || !SafeRel(name) || strings.Contains(name, "/") {
		return "", errors.New("not a project skill")
	}
	return filepath.Join(workspace, filepath.FromSlash(sub), name), nil
}

// DirDigest fingerprints a skill folder the way the kit does.
func DirDigest(dir string) (string, error) { return hashDir(dir) }
