package hookinstall

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed skill/agyh-send/SKILL.md
var sendSkill []byte

// SkillName is the skill that teaches agents `agyh send`.
const SkillName = "agyh-send"

// skillDir is where an agent reads user skills from. Antigravity has no
// documented folder for user skills (agyh can't install skills there at
// all), so it gets none: README suggests a line in its AGENTS.md instead.
func (p Paths) skillDir(agent string) string {
	switch agent {
	case "claude":
		return filepath.Join(filepath.Dir(p.Claude), "skills", SkillName)
	case "codex":
		return filepath.Join(p.Home, ".agents", "skills", SkillName)
	}
	return ""
}

// InstallSkill writes the agyh-send skill for an agent. agyh remembers a
// digest of what it wrote (.agyh-digest next to SKILL.md): a copy changed
// since then is the user's and is left alone; agyh's own copy is updated.
func InstallSkill(p Paths, agent string) (path, status string, err error) {
	dir := p.skillDir(agent)
	if dir == "" {
		return "", "", nil
	}
	path = filepath.Join(dir, "SKILL.md")
	stamp := filepath.Join(dir, ".agyh-digest")
	cur, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		status = "installed"
	case err != nil:
		return path, "", err
	case bytes.Equal(cur, sendSkill):
		return path, "up to date", nil
	default:
		written, _ := os.ReadFile(stamp)
		if string(bytes.TrimSpace(written)) != digest(cur) {
			return path, "kept (not written by agyh, or edited since)", nil
		}
		status = "updated"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return path, "", err
	}
	tmp := path + ".agyh-tmp"
	if err := os.WriteFile(tmp, sendSkill, 0o644); err != nil {
		return path, "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return path, "", err
	}
	return path, status, os.WriteFile(stamp, []byte(digest(sendSkill)+"\n"), 0o644)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
