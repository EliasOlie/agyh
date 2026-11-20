package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/source"
)

// projectSkills finds skills inside the workspaces agyh knows (from the
// sessions it indexed): <ws>/.claude/skills and <ws>/.agents/skills. Many
// live in git too, but not all, and a fresh machine has none of them.
func (e *Engine) projectSkills(ctx context.Context, home string, warnf func(string, ...any)) []KitSkill {
	if e.st == nil {
		return nil
	}
	sessions, err := e.st.AllSessions(ctx)
	if err != nil {
		warnf("project skills: %v", err)
		return nil
	}
	seen := map[string]bool{}
	var out []KitSkill
	for _, s := range sessions {
		ws := s.Workspace
		// The home folder's .claude/skills are the user's global skills.
		if ws == "" || seen[ws] || ws == home || !filepath.IsAbs(ws) || strings.HasPrefix(home+"/", ws+"/") {
			continue
		}
		seen[ws] = true
		if st, err := os.Stat(ws); err != nil || !st.IsDir() {
			continue
		}
		skills, errs := agentkit.ProjectSkills(ws)
		for _, err := range errs {
			warnf("project skills in %s: %v", ws, err)
		}
		for _, sk := range skills {
			if sk.Digest == "" {
				warnf("project skill %s in %s not backed up: too large", sk.Name, ws)
				continue
			}
			out = append(out, KitSkill{Name: sk.Name, Description: sk.Description, Agent: sk.Agent,
				Scope: agentkit.ScopeProject, Digest: sk.Digest, Path: sk.Path, Workspace: ws})
		}
	}
	return out
}

// uploadAgentFiles stores each new file once, by content.
func (e *Engine) uploadAgentFiles(ctx context.Context, files []agentkit.AgentFile, idx *kitIndex) error {
	var todo []agentkit.AgentFile
	seen := map[string]bool{}
	for _, f := range files {
		if !idx.Files[f.Digest] && !seen[f.Digest] {
			seen[f.Digest] = true
			todo = append(todo, f)
		}
	}
	done := make([]bool, len(todo))
	err := each(ctx, len(todo), func(ctx context.Context, i int) error {
		f := todo[i]
		data, err := os.ReadFile(filepath.Join(agentkit.FileRoot(f.Agent), filepath.FromSlash(f.Rel)))
		if err != nil {
			return fmt.Errorf("%s file %s: %w", f.Agent, f.Rel, err)
		}
		if digestOf(data) != f.Digest {
			return fmt.Errorf("%s file %s changed during backup: sync again", f.Agent, f.Rel)
		}
		keys, err := e.ready(ctx, true)
		if err != nil {
			return err
		}
		sealed, err := keys.seal(data)
		if err != nil {
			return err
		}
		if err := e.s3.Put(ctx, e.kitKey("files", f.Digest+".blob"), sealed); err != nil {
			return fmt.Errorf("upload %s file %s: %w", f.Agent, f.Rel, err)
		}
		done[i] = true
		return nil
	})
	for i, ok := range done {
		if ok {
			idx.Files[todo[i].Digest] = true
		}
	}
	return err
}

// restoreAgentFiles writes a snapshot's files into <dir>/<agent>/files/,
// each checked against its digest. Paths come from the vault: one that
// would leave its folder is refused.
func (e *Engine) restoreAgentFiles(ctx context.Context, dir string, files []agentkit.AgentFile) (int, error) {
	var n atomic.Int64
	err := each(ctx, len(files), func(ctx context.Context, i int) error {
		f := files[i]
		if !agentkit.SafeRel(f.Rel) {
			return fmt.Errorf("refusing kit file path %q", f.Rel)
		}
		_, data, err := e.getOpen(ctx, e.kitKey("files", f.Digest+".blob"))
		if err != nil {
			return fmt.Errorf("%s file %s: %w", f.Agent, f.Rel, err)
		}
		if digestOf(data) != f.Digest {
			return fmt.Errorf("%s file %s: content does not match its digest", f.Agent, f.Rel)
		}
		p := KitFilePath(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
		n.Add(1)
		return nil
	})
	return int(n.Load()), err
}

// KitFilePath is where a restored kit keeps one agent file.
func KitFilePath(dir string, f agentkit.AgentFile) string {
	return filepath.Join(dir, MachineName(f.Agent), "files", filepath.FromSlash(f.Rel))
}

// KitProjectSkillPath is where a restored kit keeps one project skill.
func KitProjectSkillPath(dir string, s KitSkill) string {
	return filepath.Join(dir, MachineName(s.Agent), "project-skills", sourceDirName(s.Workspace), s.Name)
}

func sourceDirName(ws string) string { return source.ClaudeProjectDir(ws) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
