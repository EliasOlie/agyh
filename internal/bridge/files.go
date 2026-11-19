package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/EliasOlie/agyh/internal/files"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/policy"
)

// OutFile is a file on its way to the phone.
type OutFile struct {
	Path, Name, Mime string
	Size             int64
	Caption          string // plain text; each channel escapes it
}

// FileSender is a channel that can send files.
type FileSender interface {
	SendFile(ctx context.Context, f OutFile) error
	// MaxFileBytes is the largest file the platform takes.
	MaxFileBytes() int64
}

// Incoming is a file sent from the phone.
type Incoming struct {
	Name, Mime, Caption string
	Size                int64 // 0 when the platform doesn't say
	Open                func(ctx context.Context) (io.ReadCloser, error)
}

// attachWindow: a file received this long ago still goes along with /new.
const attachWindow = 15 * time.Minute

type attachments struct {
	mu    sync.Mutex
	paths []string
	at    time.Time
}

func (a *attachments) add(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.at) > attachWindow {
		a.paths = nil
	}
	a.paths = append(a.paths, path)
	a.at = time.Now()
}

// take returns the files received in the last attachWindow and forgets them.
func (a *attachments) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.paths
	a.paths = nil
	if time.Since(a.at) > attachWindow {
		return nil
	}
	return p
}

// filePolicy is the send policy from the current config.
func (b *Bridge) filePolicy() files.Policy {
	home, _ := os.UserHomeDir()
	allow := append([]string{os.TempDir()}, b.cfg.Workspaces...)
	allow = append(allow, b.cfg.Files.Allow...)
	return files.Policy{Home: home, Allow: allow, Protected: b.protected, Inbox: b.inbox,
		MaxBytes: b.cfg.Files.MaxBytes()}
}

func (b *Bridge) fileSenders() []Channel {
	var out []Channel
	for _, c := range b.Channels() {
		if _, ok := c.(FileSender); ok {
			out = append(out, c)
		}
	}
	return out
}

// FileRequest is the body of POST /v1/files (agyh send).
type FileRequest struct {
	Paths     []string `json:"paths"`
	Caption   string   `json:"caption,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
}

// FileResult is the outcome for one file.
type FileResult struct {
	Path  string   `json:"path"`
	Sent  []string `json:"sent,omitempty"` // channels that got it
	Error string   `json:"error,omitempty"`
}

func (b *Bridge) handleFiles(w http.ResponseWriter, r *http.Request) {
	var in FileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || len(in.Paths) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "paths required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": b.SendFiles(r.Context(), in)})
}

// SendFiles applies the policy to each file, asks your phone once for the
// ones outside every workspace, and sends what may go to every channel.
func (b *Bridge) SendFiles(ctx context.Context, in FileRequest) []FileResult {
	pol := b.filePolicy()
	results := make([]FileResult, len(in.Paths))
	checks := make([]files.Check, len(in.Paths))
	var ask []string
	for i, p := range in.Paths {
		results[i].Path = p
		c, err := pol.Check(p, in.Cwd)
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		checks[i] = c
		if c.NeedsApproval {
			ask = append(ask, fmt.Sprintf("%s (%s)", hook.ShortenHome(c.Path), files.Size(c.Size)))
		}
	}
	if len(b.fileSenders()) == 0 {
		for i := range results {
			if results[i].Error == "" {
				results[i].Error = "no chat channel connected"
			}
		}
		return results
	}
	approved := true
	reason := ""
	if len(ask) > 0 {
		agent := in.Agent
		if agent == "" {
			agent = "agyh send"
		}
		d, _ := b.AskHuman(ctx, "", hook.Request{Agent: agent, SessionID: in.SessionID, Tool: "send_file",
			Summary: "send to your phone, from outside the workspace: " + strings.Join(ask, ", "), Workspace: in.Cwd})
		approved, reason = d.Verdict == policy.Allow, d.Reason
	}
	for i, c := range checks {
		if results[i].Error != "" {
			continue
		}
		if c.NeedsApproval && !approved {
			results[i].Error = "not approved (" + reason + ")"
			continue
		}
		out := OutFile{Path: c.Path, Name: c.Name, Size: c.Size, Mime: mimeOf(c.Path), Caption: in.Caption}
		results[i].Sent, results[i].Error = b.sendFile(ctx, nil, out)
	}
	return results
}

// sendFile sends to one channel (only, when given) or to every channel that
// takes files of that size.
func (b *Bridge) sendFile(ctx context.Context, only Channel, f OutFile) ([]string, string) {
	var sent, errs []string
	for _, c := range b.fileSenders() {
		if only != nil && c != only {
			continue
		}
		fs := c.(FileSender)
		if f.Size > fs.MaxFileBytes() {
			errs = append(errs, fmt.Sprintf("%s takes files up to %s", c.Name(), files.Size(fs.MaxFileBytes())))
			continue
		}
		if err := fs.SendFile(ctx, f); err != nil {
			b.log.Warn("send file failed", "channel", c.Name(), "err", err)
			errs = append(errs, c.Name()+": "+err.Error())
			continue
		}
		sent = append(sent, c.Name())
	}
	if len(sent) == 0 && len(errs) == 0 {
		errs = append(errs, "no chat channel can send files")
	}
	if len(sent) > 0 {
		b.log.Info("file sent", "name", f.Name, "size", f.Size, "channels", strings.Join(sent, ","))
		return sent, ""
	}
	return nil, strings.Join(errs, "; ")
}

func mimeOf(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return http.DetectContentType(buf[:n])
}

// cmdGet: "/get <path>" sends a file from this machine to the chat that asked.
// You asked for it, so no approval; secrets and the size limit still apply.
func cmdGet(ctx context.Context, b *Bridge, m Markup, rest string) string {
	if rest == "" {
		return "Usage / Uso: /get <path>   e.g. /get ~/work/app/report.pdf"
	}
	home, _ := os.UserHomeDir()
	cwd := home
	if len(b.cfg.Workspaces) > 0 {
		cwd = b.cfg.Workspaces[0]
	}
	c, err := b.filePolicy().Check(strings.TrimSpace(rest), cwd)
	if err != nil {
		return "✗ " + m.Escape(err.Error())
	}
	ch := originChannel(ctx)
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, errText := b.sendFile(sctx, ch, OutFile{Path: c.Path, Name: c.Name, Size: c.Size, Mime: mimeOf(c.Path)})
		if errText != "" && ch != nil {
			_ = ch.Send(sctx, "✗ "+ch.Markup().Escape(c.Name+": "+errText))
		}
	}()
	return fmt.Sprintf("📤 Sending %s (%s)…", m.Code(c.Name), files.Size(c.Size))
}

type originKey struct{}

func withOrigin(ctx context.Context, ch Channel) context.Context {
	return context.WithValue(ctx, originKey{}, ch)
}

func originChannel(ctx context.Context) Channel {
	ch, _ := ctx.Value(originKey{}).(Channel)
	return ch
}

// HandleFile saves a file sent from the phone into the inbox and replies
// with its path. A caption starting with /new runs it with the file
// attached; otherwise the next /new within 15 minutes takes it along.
func (b *Bridge) HandleFile(ctx context.Context, ch Channel, in Incoming) {
	m := ch.Markup()
	reply := func(s string) {
		if err := ch.Send(ctx, s); err != nil {
			b.log.Warn("reply failed", "channel", ch.Name(), "err", err)
		}
	}
	max := b.cfg.Files.MaxBytes()
	if in.Size > max {
		reply(fmt.Sprintf("✗ %s is %s, over the %s limit (files.max_mb).", m.Escape(in.Name), files.Size(in.Size), files.Size(max)))
		return
	}
	name := in.Name
	if name == "" {
		name = "phone-" + time.Now().Format("20060102-150405") + files.Ext(in.Mime)
	}
	rc, err := in.Open(ctx)
	if err != nil {
		reply("✗ Could not download " + m.Escape(name) + ": " + m.Escape(err.Error()))
		return
	}
	path, n, err := files.Save(b.inbox, name, rc, max, time.Now())
	rc.Close()
	if err != nil {
		reply("✗ Could not save " + m.Escape(name) + ": " + m.Escape(err.Error()))
		return
	}
	b.log.Info("file received", "path", path, "size", n, "channel", ch.Name())
	b.attached.add(path)

	caption := strings.TrimSpace(in.Caption)
	if cmd, rest, _ := strings.Cut(caption, " "); cmd == "/new" || cmd == "/novo" {
		reply(fmt.Sprintf("📥 %s (%s)\n", m.Code(path), files.Size(n)) + b.newSession(ctx, m, strings.TrimSpace(rest)))
		return
	}
	reply(fmt.Sprintf("📥 Saved / Salvo: %s (%s)\n\n"+
		"Send %s in the next 15 min and the file goes along, or paste the path into any session.\n"+
		"Envie %s nos próximos 15 min e o arquivo vai junto, ou cole o caminho em qualquer sessão.",
		m.Code(path), files.Size(n), m.Code("/new <prompt>"), m.Code("/novo <pedido>")))
}

// runnerArgv builds a /new command line. Received files are listed in the
// prompt and, for Claude Code (which only reads its working directory
// unless told otherwise), their folders are added with --add-dir.
func runnerArgv(tmpl []string, prompt string, paths []string) []string {
	if len(paths) > 0 {
		prompt += "\n\nFiles I sent from my phone (read them from these paths):"
		for _, p := range paths {
			prompt += "\n- " + p
		}
	}
	argv := expandRunner(tmpl, prompt)
	if len(argv) > 0 && filepath.Base(argv[0]) == "claude" {
		seen := map[string]bool{}
		for _, p := range paths {
			if d := filepath.Dir(p); !seen[d] {
				seen[d] = true
				argv = append(argv, "--add-dir", d)
			}
		}
	}
	return argv
}

// --- client side (agyh send)

// SendFiles asks the running bridge to send files (it may wait for your approval).
func (c *Client) SendFiles(ctx context.Context, in FileRequest) ([]FileResult, error) {
	var out struct {
		Results []FileResult `json:"results"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/files", in, &out)
	if err != nil && strings.Contains(err.Error(), "connection refused") {
		err = errors.New("the bridge is not running (start it with: agyh bridge)")
	}
	return out.Results, err
}
