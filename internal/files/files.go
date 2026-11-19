// Package files decides which local files may go to your phone, and keeps
// the files that come from it.
//
// Sending is the risky direction: an agent misled by a web page or a
// document could try to send keys or tokens. The destination is always your
// own chat, but Telegram bot chats are not end-to-end encrypted, so:
//
//   - secrets are never sent, whoever asks (keys, .env files, credential
//     stores, agyh's own config and data);
//   - files inside a workspace (or the system temp dir) go straight away;
//   - anything else needs your approval on the phone first.
//
// Receiving: files are saved, never executed, in agyh's inbox
// (~/.local/share/agyh/inbox/<date>/), with a sanitized name.
package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// DefaultMaxBytes matches Telegram's upload limit for bots.
const DefaultMaxBytes = 50 << 20

// ErrDenied is returned for files that are never sent.
var ErrDenied = errors.New("never sent: looks like a secret")

// Policy says which files may leave the machine.
type Policy struct {
	Home string
	// Allow are directories whose files are sent without asking
	// (workspaces, the temp dir, files.allow from the config).
	Allow []string
	// Protected are directories never sent from, besides the built-in list
	// (agyh's config and data dirs); Inbox is the exception inside them.
	Protected []string
	Inbox     string
	MaxBytes  int64
}

// Check is the verdict for one file.
type Check struct {
	Path string // absolute, symlinks resolved
	Name string
	Size int64
	// NeedsApproval: outside every allowed directory.
	NeedsApproval bool
}

// Secret directories (any path segment) and file names.
var (
	secretDirs = map[string]bool{
		".ssh": true, ".gnupg": true, ".aws": true, ".azure": true, ".kube": true, ".docker": true,
		".password-store": true, ".git": true, "gcloud": true, ".terraform.d": true,
	}
	secretFiles = regexp.MustCompile(`(?i)^(` + strings.Join([]string{
		`\.env(\..+)?`, `.*\.(pem|key|p12|pfx|jks|keystore|kdbx|ovpn|asc|gpg)`,
		`id_(rsa|dsa|ecdsa|ed25519)(\.pub)?`, `\.netrc`, `_netrc`, `\.git-credentials`, `\.pgpass`, `\.npmrc`,
		`\.pypirc`, `\.htpasswd`, `credentials(\.json|\.toml|\.ya?ml)?`, `\.credentials\.json`, `auth\.json`,
		`oauth_creds\.json`, `client_secret.*\.json`, `service[-_]account.*\.json`, `.*\.tfstate(\.backup)?`,
		`known_hosts`, `authorized_keys`, `shadow`, `gshadow`, `master\.key`,
	}, "|") + `)$`)
	// Templates are meant to be shared.
	examples = regexp.MustCompile(`(?i)\.(example|sample|template|dist)$`)
)

// Check resolves path (relative to cwd) and applies the policy. cwd also
// counts as a workspace, unless it is the home directory or above it, where
// "inside the workspace" would mean everything.
func (p Policy) Check(path, cwd string) (Check, error) {
	if path == "" {
		return Check{}, errors.New("no file given")
	}
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(p.Home, path[2:])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Check{}, fmt.Errorf("%s: %w", path, errors.Unwrap(err))
	}
	fi, err := os.Stat(real)
	if err != nil {
		return Check{}, err
	}
	if !fi.Mode().IsRegular() {
		return Check{}, fmt.Errorf("%s is not a regular file", path)
	}
	c := Check{Path: real, Name: filepath.Base(path), Size: fi.Size()}
	// Judge both the name the caller used and where it really points.
	if secret(path, p) || secret(real, p) {
		return c, fmt.Errorf("%s: %w", path, ErrDenied)
	}
	max := p.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if c.Size > max {
		return c, fmt.Errorf("%s is %s, over the %s limit", path, Size(c.Size), Size(max))
	}
	if c.Size == 0 {
		return c, fmt.Errorf("%s is empty", path)
	}
	allow := append([]string{}, p.Allow...)
	if cwd != "" && !within(p.Home, cwd) && cwd != "/" {
		allow = append(allow, cwd)
	}
	c.NeedsApproval = true
	for _, dir := range allow {
		if dir == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		if within(real, dir) && !within(p.Home, dir) && dir != "/" {
			c.NeedsApproval = false
			break
		}
	}
	return c, nil
}

func secret(path string, p Policy) bool {
	if p.Inbox != "" && within(path, p.Inbox) {
		return false
	}
	for _, dir := range p.Protected {
		if dir != "" && within(path, dir) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
		if secretDirs[seg] {
			return true
		}
	}
	base := filepath.Base(path)
	return secretFiles.MatchString(base) && !examples.MatchString(base)
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Size formats a byte count for people.
func Size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// --- inbox

// SafeName keeps a received file's name readable but harmless: no
// directories, no leading dot (hidden files, "..") and no control characters.
func SafeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var sb strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsControl(r), strings.ContainsRune(`/\:*?"<>|`, r):
			sb.WriteRune('_')
		default:
			sb.WriteRune(r)
		}
	}
	name = strings.TrimLeft(strings.TrimSpace(sb.String()), ".")
	if name == "" || name == "_" {
		name = "file"
	}
	if len(name) > 120 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:120-len(ext)] + ext
	}
	return name
}

// Save writes r into inbox/<date>/<name>, never overwriting (a numbered name
// is used instead), readable only by you and not executable. It stops at
// max bytes.
func Save(inbox, name string, r io.Reader, max int64, now time.Time) (string, int64, error) {
	dir := filepath.Join(inbox, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	name = SafeName(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
			continue
		}
		if err != nil {
			return "", 0, err
		}
		n, err := io.Copy(f, io.LimitReader(r, max+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > max {
			err = fmt.Errorf("over the %s limit", Size(max))
		}
		if err != nil {
			os.Remove(path)
			return "", 0, err
		}
		return path, n, nil
	}
}

// Ext guesses an extension for a nameless file (a phone photo) from its type.
func Ext(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg", "audio/ogg; codecs=opus":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	return ""
}
