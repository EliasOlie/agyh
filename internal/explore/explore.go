// Package explore finds skills and plugins to install: in plugin
// marketplace catalogs (the .claude-plugin/marketplace.json format many
// skill repos publish) and on GitHub, and fetches a skill repository at an
// exact commit so it can be reviewed before anything is installed.
package explore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Client talks to GitHub. Base URLs are fields so tests can point them at
// a fake server.
type Client struct {
	API      string // https://api.github.com
	Codeload string // https://codeload.github.com
	Raw      string // https://raw.githubusercontent.com
	Token    string // optional; sent only as an Authorization header, never printed
	HTTP     *http.Client
}

// NewClient uses GITHUB_TOKEN or GH_TOKEN when set (higher rate limits).
func NewClient() *Client {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		tok = os.Getenv("GH_TOKEN")
	}
	return &Client{API: "https://api.github.com", Codeload: "https://codeload.github.com",
		Raw: "https://raw.githubusercontent.com", Token: tok, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// maxDownload caps a repository tarball.
const maxDownload = 64 << 20

func (c *Client) get(ctx context.Context, rawURL, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "agyh")
	if c.Token != "" && (strings.HasPrefix(rawURL, c.API) || strings.HasPrefix(rawURL, c.Codeload)) {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, ue.Err // never echo URLs with tokens (there are none, but stay safe)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found: %s", redactURL(rawURL))
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("GitHub refused the request (HTTP %d, likely the rate limit): set GITHUB_TOKEN to raise it", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, redactURL(rawURL))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response larger than %d MiB", limit>>20)
	}
	return body, nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(url)"
	}
	u.RawQuery, u.User = "", nil
	return u.String()
}

// Ref names a skill repository (or a folder in it) on GitHub.
type Ref struct {
	Owner, Repo string
	Path        string // folder inside the repo, "" for the root
	Rev         string // branch, tag or commit; "" for the default branch
}

func (r Ref) String() string {
	s := "github:" + r.Owner + "/" + r.Repo
	if r.Path != "" {
		s += "/" + r.Path
	}
	if r.Rev != "" {
		s += "@" + r.Rev
	}
	return s
}

var ghName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRef accepts github:owner/repo[/path][@rev], owner/repo[...] and
// https://github.com/owner/repo[/tree/<rev>/<path>].
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	var r Ref
	if after, ok := strings.CutPrefix(s, "https://github.com/"); ok {
		parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(after, "/"), ".git"), "/")
		if len(parts) < 2 {
			return Ref{}, fmt.Errorf("not a repository URL: %s", s)
		}
		r.Owner, r.Repo = parts[0], parts[1]
		if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
			r.Rev, r.Path = parts[3], strings.Join(parts[4:], "/")
		}
	} else {
		s = strings.TrimPrefix(s, "github:")
		if i := strings.LastIndex(s, "@"); i > 0 {
			s, r.Rev = s[:i], s[i+1:]
		}
		parts := strings.Split(strings.Trim(s, "/"), "/")
		if len(parts) < 2 {
			return Ref{}, fmt.Errorf("expected github:owner/repo[/path][@rev], got %q", s)
		}
		r.Owner, r.Repo, r.Path = parts[0], parts[1], strings.Join(parts[2:], "/")
	}
	if !ghName.MatchString(r.Owner) || !ghName.MatchString(r.Repo) {
		return Ref{}, fmt.Errorf("invalid repository name %s/%s", r.Owner, r.Repo)
	}
	if r.Path != "" {
		if _, ok := cleanRel(r.Path); !ok {
			return Ref{}, fmt.Errorf("invalid path %q", r.Path)
		}
	}
	if r.Rev != "" && !regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`).MatchString(r.Rev) {
		return Ref{}, fmt.Errorf("invalid revision %q", r.Rev)
	}
	return r, nil
}

var sha1Hex = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Resolve pins a ref to a commit SHA.
func (c *Client) Resolve(ctx context.Context, r Ref) (string, error) {
	rev := r.Rev
	if rev == "" {
		rev = "HEAD"
	}
	body, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/%s/commits/%s", c.API, r.Owner, r.Repo, url.PathEscape(rev)), "application/vnd.github.sha", 1024)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", r, err)
	}
	sha := strings.TrimSpace(string(body))
	if !sha1Hex.MatchString(sha) {
		return "", fmt.Errorf("resolve %s: unexpected answer from GitHub", r)
	}
	return sha, nil
}

// Limits for an extracted repository subtree.
const (
	maxFiles = 20000
	maxBytes = 256 << 20
)

// Fetch downloads r at commit sha and extracts r.Path into dest (which must
// not exist). The archive is untrusted: only regular files with clean
// relative paths, within limits; links and special files are skipped.
func (c *Client) Fetch(ctx context.Context, r Ref, sha, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	body, err := c.get(ctx, fmt.Sprintf("%s/%s/%s/tar.gz/%s", c.Codeload, r.Owner, r.Repo, sha), "", maxDownload)
	if err != nil {
		return fmt.Errorf("download %s: %w", r, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("download %s: not a tar.gz: %w", r, err)
	}
	tr := tar.NewReader(gz)
	prefix := ""
	if r.Path != "" {
		prefix = r.Path + "/"
	}
	var (
		files int
		total int64
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("download %s: %w", r, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // directories are implied; links and devices are never extracted
		}
		// GitHub archives have one top folder ("<repo>-<sha>/"): drop it.
		_, rel, ok := strings.Cut(hdr.Name, "/")
		if !ok {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(rel, prefix) {
				continue
			}
			rel = strings.TrimPrefix(rel, prefix)
		}
		name, ok := cleanRel(rel)
		if !ok {
			return fmt.Errorf("download %s: unsafe path %q in archive", r, hdr.Name)
		}
		if files++; files > maxFiles {
			return fmt.Errorf("download %s: more than %d files", r, maxFiles)
		}
		if total += hdr.Size; total > maxBytes {
			return fmt.Errorf("download %s: larger than %d MiB", r, maxBytes>>20)
		}
		target := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if hdr.Mode&0o111 != 0 {
			mode = 0o700
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("download %s: %w", r, err)
		}
		_, err = io.CopyN(out, tr, hdr.Size)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if files == 0 {
		where := "the repository"
		if r.Path != "" {
			where = r.Path
		}
		return fmt.Errorf("%s: nothing found in %s", r, where)
	}
	return os.Rename(tmp, dest)
}

func cleanRel(name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return name, true
}

// SkillDirs finds the skills in a fetched tree: the root itself if it has a
// SKILL.md, else folders with a SKILL.md up to three levels down
// (skills/<name>/, plugins/<p>/skills/<name>/, …).
func SkillDirs(root string) []string {
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
		return []string{root}
	}
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if depth := strings.Count(filepath.ToSlash(rel), "/"); rel != "." && depth > 3 {
			return filepath.SkipDir
		}
		if strings.HasPrefix(d.Name(), ".") && p != root {
			return filepath.SkipDir
		}
		if p != root {
			if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err == nil {
				out = append(out, p)
				return filepath.SkipDir
			}
		}
		return nil
	})
	slices.Sort(out)
	return out
}

// Result is one thing to install, from a catalog or GitHub.
type Result struct {
	Kind        string `json:"kind"` // "plugin" (catalog entry) or "repo" (GitHub)
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Catalog     string `json:"catalog"`          // marketplace name, or "github"
	Source      string `json:"source,omitempty"` // github:owner/repo[/path] when known
	Stars       int    `json:"stars,omitempty"`
	Install     string `json:"install,omitempty"` // command that installs it
}

// matches reports whether every word appears in the text (case-insensitive).
func matches(words []string, text string) bool {
	text = strings.ToLower(text)
	for _, w := range words {
		if !strings.Contains(text, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// Marketplace is the .claude-plugin/marketplace.json format.
type Marketplace struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Source      json.RawMessage `json:"source"`
	} `json:"plugins"`
}

// SearchCatalog filters a marketplace. home is the catalog's own location
// (github:owner/repo) used to resolve plugins given by relative path.
func SearchCatalog(m Marketplace, home string, words []string) []Result {
	var out []Result
	for _, p := range m.Plugins {
		if !matches(words, p.Name+" "+p.Description) {
			continue
		}
		r := Result{Kind: "plugin", Name: p.Name, Description: p.Description, Catalog: m.Name, Source: pluginSource(p.Source, home)}
		if r.Source != "" {
			r.Install = "agyh skills install " + p.Name + "@" + m.Name
		}
		out = append(out, r)
	}
	return out
}

// pluginSource turns a marketplace "source" into github:owner/repo[/path].
func pluginSource(raw json.RawMessage, home string) string {
	var rel string
	if json.Unmarshal(raw, &rel) == nil {
		rel = strings.TrimPrefix(strings.TrimPrefix(rel, "./"), "/")
		if home == "" || rel == "" {
			return ""
		}
		if _, ok := cleanRel(rel); !ok {
			return ""
		}
		return home + "/" + rel
	}
	var obj struct {
		Source string `json:"source"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	repo := obj.Repo
	if repo == "" && strings.HasPrefix(obj.URL, "https://github.com/") {
		repo = strings.TrimSuffix(strings.TrimPrefix(obj.URL, "https://github.com/"), ".git")
	}
	if repo == "" {
		return ""
	}
	s := "github:" + strings.Trim(repo, "/")
	if obj.Path != "" {
		s += "/" + strings.Trim(obj.Path, "/")
	}
	if obj.Ref != "" {
		s += "@" + obj.Ref
	}
	if _, err := ParseRef(s); err != nil {
		return ""
	}
	return s
}

// FetchCatalog downloads a marketplace.json from github:owner/repo (its
// .claude-plugin/marketplace.json) or a direct https URL.
func (c *Client) FetchCatalog(ctx context.Context, where string) (Marketplace, string, error) {
	var (
		rawURL string
		home   string
	)
	if strings.HasPrefix(where, "https://") {
		rawURL = where
	} else {
		r, err := ParseRef(where)
		if err != nil {
			return Marketplace{}, "", err
		}
		rev := r.Rev
		if rev == "" {
			rev = "HEAD"
		}
		rawURL = fmt.Sprintf("%s/%s/%s/%s/.claude-plugin/marketplace.json", c.Raw, r.Owner, r.Repo, rev)
		home = "github:" + r.Owner + "/" + r.Repo
	}
	body, err := c.get(ctx, rawURL, "", 8<<20)
	if err != nil {
		return Marketplace{}, "", fmt.Errorf("catalog %s: %w", where, err)
	}
	var m Marketplace
	if err := json.Unmarshal(body, &m); err != nil {
		return Marketplace{}, "", fmt.Errorf("catalog %s: %w", where, err)
	}
	return m, home, nil
}

// SearchGitHub looks for repositories tagged as agent skills.
func (c *Client) SearchGitHub(ctx context.Context, words []string, limit int) ([]Result, error) {
	seen := map[string]bool{}
	var out []Result
	for _, topic := range []string{"agent-skills", "claude-skills"} {
		q := strings.TrimSpace(strings.Join(words, " ") + " topic:" + topic)
		body, err := c.get(ctx, fmt.Sprintf("%s/search/repositories?q=%s&sort=stars&order=desc&per_page=%d", c.API, url.QueryEscape(q), limit),
			"application/vnd.github+json", 4<<20)
		if err != nil {
			return out, err
		}
		var res struct {
			Items []struct {
				FullName    string `json:"full_name"`
				Description string `json:"description"`
				Stars       int    `json:"stargazers_count"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return out, err
		}
		for _, it := range res.Items {
			if seen[it.FullName] {
				continue
			}
			seen[it.FullName] = true
			out = append(out, Result{Kind: "repo", Name: it.FullName, Description: it.Description, Catalog: "github",
				Source: "github:" + it.FullName, Stars: it.Stars, Install: "agyh skills install github:" + it.FullName})
		}
	}
	slices.SortStableFunc(out, func(a, b Result) int { return b.Stars - a.Stars })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
