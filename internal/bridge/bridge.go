// Package bridge is the long-running daemon that connects local agent hooks
// to your phone. Hooks call a loopback HTTP API; the bridge fans approval
// requests out to every connected chat channel (Telegram, WhatsApp) and the
// first answer wins. The same channels accept commands (/chats, /search,
// /new, …) so you can drive your agents from anywhere.
package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/policy"
	"github.com/EliasOlie/agyh/internal/store"
)

// Markup formats text for one chat platform.
type Markup interface {
	Bold(s string) string
	Code(s string) string
	Escape(s string) string
}

// Channel is a chat platform the bridge talks through.
type Channel interface {
	Name() string
	Markup() Markup
	// Run receives messages until ctx is done, calling b.HandleText / b.Decide.
	Run(ctx context.Context, b *Bridge) error
	Send(ctx context.Context, text string) error
	PromptApproval(ctx context.Context, a *Approval) error
	ApprovalResolved(ctx context.Context, a *Approval, d Decision) error
}

type Bridge struct {
	cfg       *config.Config
	store     *store.Store
	policy    *policy.Policy
	approvals *Approvals
	log       *slog.Logger
	runner    Runner

	mu       sync.RWMutex
	channels []Channel
}

type Options struct {
	Config    *config.Config
	Store     *store.Store
	Policy    *policy.Policy
	StatePath string
	Logger    *slog.Logger
	Runner    Runner
}

func New(o Options) *Bridge {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Policy == nil {
		o.Policy = policy.MustDefault()
	}
	return &Bridge{cfg: o.Config, store: o.Store, policy: o.Policy, approvals: NewApprovals(o.StatePath),
		log: o.Logger, runner: o.Runner}
}

func (b *Bridge) AddChannel(c Channel) {
	b.mu.Lock()
	b.channels = append(b.channels, c)
	b.mu.Unlock()
}

func (b *Bridge) Channels() []Channel {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Channel(nil), b.channels...)
}

func (b *Bridge) Approvals() *Approvals { return b.approvals }

// Serve starts the loopback API and every channel, and blocks until ctx is done.
func (b *Bridge) Serve(ctx context.Context) error {
	if err := checkLoopback(b.cfg.Bridge.Addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", b.cfg.Bridge.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w (is another bridge running?)", b.cfg.Bridge.Addr, err)
	}
	srv := &http.Server{Handler: b.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1+len(b.Channels()))
	go func() { errc <- srv.Serve(ln) }()
	for _, c := range b.Channels() {
		go func(c Channel) {
			if err := c.Run(ctx, b); err != nil && !errors.Is(err, context.Canceled) {
				b.log.Error("channel stopped", "channel", c.Name(), "err", err)
			}
		}(c)
	}
	b.log.Info("bridge listening", "addr", b.cfg.Bridge.Addr, "channels", len(b.Channels()))
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// The API can approve shell commands, so it must never leave the machine.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("bridge.addr must be a loopback address, got %q", addr)
	}
	return nil
}

// Handler exposes the hook API. Every route needs the bearer secret.
func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		names := []string{}
		for _, c := range b.Channels() {
			names = append(names, c.Name())
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "channels": names})
	})
	mux.HandleFunc("POST /v1/approvals", b.handleApproval)
	mux.HandleFunc("POST /v1/approvals/{id}/decision", b.handleExternalDecision)
	mux.HandleFunc("POST /v1/notify", b.handleNotify)
	return b.auth(mux)
}

func (b *Bridge) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + b.cfg.Bridge.Secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if b.cfg.Bridge.Secret == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ApprovalRequest is the body hooks send.
type ApprovalRequest struct {
	ID      string       `json:"id"`
	Request hook.Request `json:"request"`
}

// ApprovalResponse is what hooks get back.
type ApprovalResponse struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
}

func (b *Bridge) handleApproval(w http.ResponseWriter, r *http.Request) {
	var in ApprovalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// Defense in depth: the hook already checked, but the bridge never approves a fatal command.
	if in.Request.IsShell() {
		if v, reason := b.policy.Classify(in.Request.Command); v == policy.Deny {
			writeJSON(w, http.StatusOK, ApprovalResponse{ID: in.ID, Verdict: "deny", Reason: reason, By: "policy"})
			return
		}
	}
	key := in.Request.Agent + ":" + in.Request.SessionID
	if in.Request.SessionID != "" && b.approvals.IsAuto(key) {
		writeJSON(w, http.StatusOK, ApprovalResponse{ID: in.ID, Verdict: "allow", Reason: "autonomous mode", By: "auto"})
		return
	}
	channels := b.Channels()
	if len(channels) == 0 {
		writeJSON(w, http.StatusOK, ApprovalResponse{ID: in.ID, Verdict: "ask", Reason: "no chat channel connected", By: "bridge"})
		return
	}

	ap := b.approvals.Create(in.ID, in.Request)
	for _, c := range channels {
		if err := c.PromptApproval(r.Context(), ap); err != nil {
			b.log.Warn("prompt approval failed", "channel", c.Name(), "err", err)
		}
	}

	timer := time.NewTimer(b.cfg.ApprovalTimeout())
	defer timer.Stop()
	var d Decision
	select {
	case <-ap.Done():
		d = ap.Decision()
	case <-timer.C:
		d = b.Decide(context.Background(), ap.ID, Decision{Verdict: policy.Ask, Reason: "no answer in time", By: "timeout"}, false)
	case <-r.Context().Done(): // the hook gave up (agent cancelled, or answered in the terminal)
		d = b.Decide(context.Background(), ap.ID, Decision{Verdict: policy.Ask, Reason: "request cancelled", By: "cancelled"}, false)
	}
	writeJSON(w, http.StatusOK, ApprovalResponse{ID: ap.ID, Verdict: d.Verdict.String(), Reason: d.Reason, By: d.By})
}

func (b *Bridge) handleExternalDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Verdict string `json:"verdict"`
		By      string `json:"by"`
		Auto    bool   `json:"auto"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	v := map[string]policy.Verdict{"allow": policy.Allow, "deny": policy.Deny, "ask": policy.Ask}[in.Verdict]
	if in.By == "" {
		in.By = "terminal"
	}
	d := b.Decide(r.Context(), r.PathValue("id"), Decision{Verdict: v, Reason: "answered in " + in.By, By: in.By}, in.Auto)
	writeJSON(w, http.StatusOK, ApprovalResponse{ID: r.PathValue("id"), Verdict: d.Verdict.String(), Reason: d.Reason, By: d.By})
}

func (b *Bridge) handleNotify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text required"})
		return
	}
	b.Broadcast(r.Context(), func(m Markup) string { return m.Escape(in.Text) })
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Decide resolves an approval (first answer wins) and updates every channel.
// With auto=true the whole session switches to autonomous mode.
// It returns the effective decision, which may be an earlier one.
func (b *Bridge) Decide(ctx context.Context, id string, d Decision, auto bool) Decision {
	ap, won := b.approvals.Resolve(id, d)
	if ap == nil {
		return Decision{Verdict: policy.Ask, Reason: "approval not found or already decided", By: "bridge"}
	}
	if !won {
		return ap.Decision()
	}
	if auto && d.Verdict == policy.Allow && ap.Req.SessionID != "" {
		b.approvals.SetAuto(ap.SessionKey(), true)
	}
	for _, c := range b.Channels() {
		if err := c.ApprovalResolved(ctx, ap, d); err != nil {
			b.log.Warn("update approval message failed", "channel", c.Name(), "err", err)
		}
	}
	b.log.Info("approval decided", "id", id, "verdict", d.Verdict.String(), "by", d.By)
	return d
}

// Broadcast sends a message to every channel, rendered with each channel's markup.
func (b *Bridge) Broadcast(ctx context.Context, render func(Markup) string) {
	for _, c := range b.Channels() {
		if err := c.Send(ctx, render(c.Markup())); err != nil {
			b.log.Warn("send failed", "channel", c.Name(), "err", err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
