package sync

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/source"
)

// Outcomes of `agyh kit apply` for one item.
const (
	KitRestored     = "restored"
	KitWouldRestore = "would restore"
	KitPresent      = "already here"
	KitKept         = "kept yours"
	KitNoWorkspace  = "workspace not on this machine"
	KitFailed       = "failed"
)

// KitStep is what apply did (or would do) with one file or project skill.
type KitStep struct {
	Kind   string `json:"kind"` // an agentkit file kind, or "project skill"
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Status string `json:"status"`
	// Detail: for KitKept, where the vault's copy is, to compare by hand.
	Detail string `json:"detail,omitempty"`
}

// ReadKit loads a restored kit's snapshot (without MCP secrets).
func ReadKit(dir string) (KitSnapshot, error) {
	var snap KitSnapshot
	b, err := os.ReadFile(filepath.Join(dir, "kit.json"))
	if err != nil {
		return snap, err
	}
	return snap, json.Unmarshal(b, &snap)
}

// homeMap rewrites paths from the snapshot's home to this machine's, so a
// new user name (/home/elias → /Users/elias) still lands in the right place.
type homeMap struct{ from, to string }

func (h homeMap) path(p string) string {
	if h.from == "" || h.from == h.to {
		return p
	}
	if p == h.from || strings.HasPrefix(p, h.from+string(filepath.Separator)) || strings.HasPrefix(p, h.from+"/") {
		return h.to + p[len(h.from):]
	}
	return p
}

// rel rewrites Claude Code's per-folder names (projects/-home-elias-x/…).
func (h homeMap) rel(agent, rel string) string {
	if agent != "claude" || h.from == "" || h.from == h.to || !strings.HasPrefix(rel, "projects/") {
		return rel
	}
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) < 3 {
		return rel
	}
	old, cur := source.ClaudeProjectDir(h.from), source.ClaudeProjectDir(h.to)
	if parts[1] == old || strings.HasPrefix(parts[1], old+"-") {
		parts[1] = cur + parts[1][len(old):]
	}
	return strings.Join(parts, "/")
}

// ApplyKit puts back the agent files and project skills of a restored kit
// that this machine lacks. Nothing that exists is ever replaced: a
// different local copy is reported (KitKept) with the vault's copy beside it.
func ApplyKit(dir string, snap KitSnapshot, dry bool) []KitStep {
	home, _ := os.UserHomeDir()
	hm := homeMap{from: snap.Home, to: home}
	var steps []KitStep

	for _, f := range snap.Files {
		st := KitStep{Kind: f.Kind, Agent: f.Agent, Name: f.Rel}
		root := agentkit.FileRoot(f.Agent)
		rel := hm.rel(f.Agent, f.Rel)
		if root == "" || !agentkit.SafeRel(f.Rel) || !agentkit.SafeRel(rel) {
			st.Status, st.Detail = KitFailed, "unsafe path"
			steps = append(steps, st)
			continue
		}
		st.Target = filepath.Join(root, filepath.FromSlash(rel))
		src := KitFilePath(dir, f)
		switch d, err := agentkit.FileDigest(st.Target); {
		case err == nil && d == f.Digest:
			st.Status = KitPresent
		case err == nil:
			st.Status, st.Detail = KitKept, src
		case !errors.Is(err, fs.ErrNotExist):
			st.Status, st.Detail = KitFailed, err.Error()
		case dry:
			st.Status = KitWouldRestore
		default:
			st.Status = KitRestored
			if err := copyFile(src, st.Target, f); err != nil {
				st.Status, st.Detail = KitFailed, err.Error()
			}
		}
		steps = append(steps, st)
	}

	for _, s := range snap.Skills {
		if s.Scope != agentkit.ScopeProject {
			continue // global skills: `agyh skills link --from kit`
		}
		ws := hm.path(s.Workspace)
		st := KitStep{Kind: "project skill", Agent: s.Agent, Name: s.Name}
		target, err := agentkit.ProjectSkillDir(ws, s.Agent, s.Name)
		if err != nil {
			st.Status, st.Detail = KitFailed, err.Error()
			steps = append(steps, st)
			continue
		}
		st.Target = target
		if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
			st.Status, st.Detail = KitNoWorkspace, "clone it, then run this again"
			steps = append(steps, st)
			continue
		}
		src := KitProjectSkillPath(dir, s)
		switch _, err := os.Stat(target); {
		case err == nil:
			st.Status = KitKept
			if d, _ := agentkit.DirDigest(target); d == s.Digest {
				st.Status = KitPresent
			} else {
				st.Detail = src
			}
		case dry:
			st.Status = KitWouldRestore
		default:
			st.Status = KitRestored
			if err := copySkill(src, target, s.Digest); err != nil {
				st.Status, st.Detail = KitFailed, err.Error()
			}
		}
		steps = append(steps, st)
	}
	return steps
}

func copyFile(src, dst string, f agentkit.AgentFile) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if digestOf(data) != f.Digest {
		return errors.New("the restored copy does not match its digest: run agyh sync restore again")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if f.Kind == agentkit.FileMemory {
		mode = 0o600
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := out.Write(data); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// copySkill copies a skill folder through the kit's own archive format, so
// the result is checked against the digest the vault recorded.
func copySkill(src, dst, digest string) error {
	archive, _, err := agentkit.Pack(src)
	if err != nil {
		return err
	}
	return agentkit.Unpack(archive, dst, digest)
}
