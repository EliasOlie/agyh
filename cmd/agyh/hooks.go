package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/policy"
	"github.com/EliasOlie/agyh/internal/source"
)

func (a *app) hookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Entry points called by agent hooks (see `agyh install-hooks`)",
		Hidden: false,
	}
	var agent, tty string

	pre := &cobra.Command{
		Use:   "pre-tool-use",
		Short: "Decide whether a tool call may run: policy first, then terminal and phone race",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			req, err := hook.Parse(agent, os.Stdin)
			if err != nil {
				out.Write(hook.Output(agent, policy.Ask, err.Error()))
				return nil
			}
			cfg, err := config.Load()
			if err != nil {
				out.Write(hook.Output(agent, policy.Ask, "agyh config error: "+err.Error()))
				return nil
			}
			v, reason := decide(cmd.Context(), cfg, req, useTTY(agent, tty))
			out.Write(hook.Output(agent, v, reason))
			return nil
		},
	}
	pre.Flags().StringVar(&agent, "agent", "claude", "agent calling the hook: claude or agy")
	pre.Flags().StringVar(&tty, "tty", "auto", "also ask in the terminal: auto (agy only), on, off")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Notify your phone that the agent finished, with its last message",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := hook.Parse(agent, os.Stdin)
			if err != nil {
				return nil // never break the agent because of a notification
			}
			a.notifyStop(cmd.Context(), req)
			return nil
		},
	}
	stop.Flags().StringVar(&agent, "agent", "claude", "agent calling the hook: claude or agy")

	cmd.AddCommand(pre, stop)
	return cmd
}

func useTTY(agent, flag string) bool {
	switch flag {
	case "on", "true", "yes":
		return true
	case "off", "false", "no":
		return false
	}
	// Claude Code owns the terminal while hooks run and has its own prompt; Antigravity hooks can use it.
	return agent == "agy"
}

// decide applies the local policy, then races the phone against the terminal.
func decide(ctx context.Context, cfg *config.Config, req hook.Request, withTTY bool) (policy.Verdict, string) {
	pol, err := policy.New(cfg.Policy.Safe, cfg.Policy.Fatal)
	if err != nil {
		return policy.Ask, "agyh policy error: " + err.Error()
	}
	if req.IsShell() {
		if v, reason := pol.Classify(req.Command); v != policy.Ask {
			return v, reason
		}
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.ApprovalTimeout()+15*time.Second)
	defer cancel()
	id := bridge.NewID()
	client := bridge.NewClient(cfg)

	remote := make(chan bridge.ApprovalResponse, 1)
	go func() {
		resp, err := client.Ask(ctx, id, req)
		if err != nil {
			resp = bridge.ApprovalResponse{Verdict: "ask", Reason: "bridge not running", By: "none"}
		}
		remote <- resp
	}()

	var local <-chan ttyAnswer
	var prompt *ttyPrompt
	if withTTY {
		if p, err := openTTYPrompt(req, id); err == nil {
			prompt = p
			local = p.answers
			defer p.close()
		}
	}

	for {
		select {
		case ans := <-local:
			verdict := "deny"
			if ans.verdict == policy.Allow {
				verdict = "allow"
			}
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = client.Decide(dctx, id, verdict, "terminal", ans.auto)
			dcancel()
			return ans.verdict, "answered in the terminal"
		case resp := <-remote:
			switch resp.Verdict {
			case "allow":
				prompt.notice("✔ allowed via " + resp.By)
				return policy.Allow, resp.Reason
			case "deny":
				prompt.notice("✘ denied via " + resp.By)
				return policy.Deny, resp.Reason
			}
			if prompt == nil {
				return policy.Ask, "no remote answer (" + resp.Reason + ")"
			}
			remote = nil // keep waiting for the terminal only
		case <-ctx.Done():
			return policy.Ask, "no answer in time"
		}
	}
}

type ttyAnswer struct {
	verdict policy.Verdict
	auto    bool
}

// ttyPrompt asks y/n/a on the controlling terminal, one keypress, in raw mode.
type ttyPrompt struct {
	f       *os.File
	state   *term.State
	answers chan ttyAnswer
}

func openTTYPrompt(req hook.Request, id string) (*ttyPrompt, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		f.Close()
		return nil, err
	}
	p := &ttyPrompt{f: f, state: state, answers: make(chan ttyAnswer, 1)}
	fmt.Fprintf(f, "\r\n\x1b[1;35magyh\x1b[0m %s wants to run:\r\n  \x1b[1m%s\x1b[0m\r\n  [y] allow  [n] deny  [a] allow whole session   (also sent to your phone, id %s)\r\n",
		req.Agent, strings.ReplaceAll(model.Truncate(req.Describe(), 300), "\n", " "), id)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := f.Read(buf)
			if err != nil || n == 0 {
				return
			}
			switch buf[0] {
			case 'y', 'Y', '\r':
				p.answers <- ttyAnswer{verdict: policy.Allow}
				return
			case 'n', 'N', 3: // 3 = Ctrl+C
				p.answers <- ttyAnswer{verdict: policy.Deny}
				return
			case 'a', 'A':
				p.answers <- ttyAnswer{verdict: policy.Allow, auto: true}
				return
			}
		}
	}()
	return p, nil
}

func (p *ttyPrompt) notice(msg string) {
	if p != nil {
		fmt.Fprintf(p.f, "  %s\r\n", msg)
	}
}

func (p *ttyPrompt) close() {
	_ = p.f.SetReadDeadline(time.Now())
	_ = term.Restore(int(p.f.Fd()), p.state)
	_ = p.f.Close()
}

// notifyStop sends "agent finished" with the last assistant message, and
// refreshes that session in the index so chat commands see it immediately.
func (a *app) notifyStop(ctx context.Context, req hook.Request) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	src := source.ByName(req.Agent)
	var sess model.Session
	var msgs []model.Message
	if src != nil && req.TranscriptPath != "" {
		id := req.SessionID
		sess, msgs, _ = src.Load(ctx, source.Ref{Agent: req.Agent, ID: id, Path: req.TranscriptPath})
		if st, err := a.open(); err == nil {
			if sess.ID != "" {
				if fi, err := os.Stat(req.TranscriptPath); err == nil {
					sess.ModTime = fi.ModTime()
				}
				if req.Agent != "agy" { // agy metadata lives in its own DB; the next sync picks it up
					_ = st.Upsert(ctx, sess, msgs)
				}
			}
			if s2, err := st.Resolve(ctx, req.Agent+":"+req.SessionID); err == nil && sess.Title == "" {
				sess.Title = s2.Title
			}
			st.Close()
		}
	}
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			last = msgs[i].Text
			break
		}
	}
	text := fmt.Sprintf("✅ %s finished in %s", req.Agent, hook.ShortenHome(firstNonEmpty(req.Workspace, sess.Workspace)))
	if sess.Title != "" {
		text += "\n" + sess.Title
	}
	if last != "" {
		text += "\n\n" + model.Truncate(last, 1500)
	}
	if id := firstNonEmpty(req.SessionID, sess.ID); id != "" {
		text += "\n\nagyh resume " + model.Session{ID: id}.ShortID()
	}
	nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = bridge.NewClient(cfg).Notify(nctx, text)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
