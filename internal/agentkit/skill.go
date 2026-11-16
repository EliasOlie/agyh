package agentkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits keep a huge or hostile skill folder from stalling a listing.
const (
	maxSkillFiles = 2000
	maxSkillBytes = 64 << 20
	maxSkillMD    = 1 << 20
)

// scanSkills reads every <root>/<dir>/SKILL.md, one level deep.
func scanSkills(root, agent, scope, plugin string) ([]Skill, []error) {
	entries, err := os.ReadDir(root)
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
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		// Skill folders may be symlinks (Codex documents this); follow them.
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		if !fileExists(filepath.Join(dir, "SKILL.md")) {
			continue
		}
		s, err := readSkill(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", shortHome(dir), err))
			if s.Name == "" {
				continue // SKILL.md unreadable
			}
		}
		s.Agent, s.Scope, s.Plugin = agent, scope, plugin
		out = append(out, s)
	}
	return out, errs
}

// readSkill parses SKILL.md's frontmatter and fingerprints the folder. When
// only fingerprinting fails (folder too large), the skill is still returned,
// with an empty Hash, alongside the error.
func readSkill(dir string) (Skill, error) {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSkillMD))
	f.Close()
	if err != nil {
		return Skill{}, err
	}
	fm := frontmatter(raw)
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = filepath.Base(dir)
	}
	s := Skill{Name: name, Description: strings.Join(strings.Fields(fm.Description), " "), Path: dir}
	s.Hash, err = hashDir(dir)
	if err != nil {
		return s, fmt.Errorf("not fingerprinted: %w", err)
	}
	return s, nil
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// frontmatter reads the YAML block between the leading "---" lines. A missing
// or malformed block yields an empty result: the folder name stands in.
func frontmatter(md []byte) skillFrontmatter {
	md = bytes.TrimPrefix(md, []byte("\xef\xbb\xbf"))
	md = bytes.ReplaceAll(md, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(md, []byte("---\n")) {
		return skillFrontmatter{}
	}
	body := md[4:]
	end := bytes.Index(body, []byte("\n---"))
	if end < 0 {
		return skillFrontmatter{}
	}
	var fm skillFrontmatter
	if yaml.Unmarshal(body[:end], &fm) != nil {
		return skillFrontmatter{}
	}
	return fm
}

// skipDirs are never part of a skill's identity: VCS metadata, and
// dependencies or caches a skill's scripts install next to it (a skill with
// a Python venv and Playwright browsers weighs gigabytes, its authored files
// a few kilobytes). A clone and a fresh copy of the same skill hash alike.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true, ".cache": true,
	"ms-playwright": true,
}

// hashDir fingerprints a skill folder: relative paths, executable bits and
// contents of every regular file outside skipDirs, in a stable order.
func hashDir(root string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	var files []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			files = append(files, p)
			if len(files) > maxSkillFiles {
				return fmt.Errorf("more than %d files", maxSkillFiles)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.Sort(files)
	h := sha256.New()
	var total int64
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		total += st.Size()
		if total > maxSkillBytes {
			return "", fmt.Errorf("larger than %d MiB", maxSkillBytes>>20)
		}
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", filepath.ToSlash(rel), st.Mode().Perm()&0o111, st.Size())
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
