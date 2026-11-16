package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/policy"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type propEnv struct {
	t    *testing.T
	b    *Bridge
	ch   *fakeChannel
	home string
}

func newPropEnv(t *testing.T, timeoutSec int) *propEnv {
	t.Helper()
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_PLUGIN_CACHE_DIR", "AGYH_AGY_CONFIG_DIR", "AGYH_AGY_DIR", "CODEX_HOME"} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("AGYH_CODEX_ADMIN_SKILLS", filepath.Join(home, "none"))
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	b, _, _, cfg := newBridge(t, timeoutSec)
	cfg.ManageAgents, cfg.AutoPropagate = true, true
	b.homes = func() []agentkit.Home {
		return []agentkit.Home{agentkit.NewClaude(home), agentkit.NewAgy(home), agentkit.NewCodex(home)}
	}
	b.ledgerPath = filepath.Join(home, "data", "links.json")
	b.propagationPath = filepath.Join(home, "data", "propagation.json")
	ch := newFake("telegram")
	b.AddChannel(ch)
	return &propEnv{t: t, b: b, ch: ch, home: home}
}

func (e *propEnv) agySkill(name, body string) {
	put(e.t, filepath.Join(e.home, ".gemini", "config", "plugins", "plug", "skills", name, "SKILL.md"),
		"---\nname: "+name+"\n---\n"+body+"\n")
}

// pass runs one propagation pass, answering its prompts with answers in order.
func (e *propEnv) pass(answers ...func(*Approval)) []*Approval {
	e.t.Helper()
	done := make(chan error, 1)
	go func() { done <- e.b.PropagateSkills(context.Background()) }()
	var prompts []*Approval
	for {
		select {
		case err := <-done:
			if err != nil {
				e.t.Fatal(err)
			}
			if len(prompts) != len(answers) {
				e.t.Fatalf("got %d prompts, want %d", len(prompts), len(answers))
			}
			return prompts
		case a := <-e.ch.prompted:
			if len(prompts) >= len(answers) {
				e.t.Fatalf("unexpected prompt: %s", a.Req.Summary)
			}
			answers[len(prompts)](a)
			prompts = append(prompts, a)
		case <-time.After(10 * time.Second):
			e.t.Fatal("propagation pass hung")
		}
	}
}

func (e *propEnv) answer(v policy.Verdict, auto bool) func(*Approval) {
	return func(a *Approval) {
		go e.b.Decide(context.Background(), a.ID, Decision{Verdict: v, Reason: "test", By: "telegram"}, auto)
	}
}

func noAnswer(*Approval) {}

func (e *propEnv) installed(agent, name string) bool {
	dir := filepath.Join(e.home, ".claude", "skills", name)
	if agent == "codex" {
		dir = filepath.Join(e.home, ".agents", "skills", name)
	}
	_, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil
}

func TestPropagationAsksBeforeInstalling(t *testing.T) {
	e := newPropEnv(t, 5)
	e.agySkill("existing", "was here before")

	// First pass: record what exists, ask nothing.
	e.pass()
	if e.installed("claude", "existing") {
		t.Fatal("baseline skills must not be propagated")
	}

	// A new skill: one prompt, allowed, installed where missing.
	e.agySkill("fresh", "v1")
	prompts := e.pass(e.answer(policy.Allow, false))
	if s := prompts[0].Req.Summary; !strings.Contains(s, "New skill fresh (from agy plugin plug") || !strings.Contains(s, "into claude, codex") {
		t.Fatalf("prompt: %q", s)
	}
	if !e.installed("claude", "fresh") || !e.installed("codex", "fresh") {
		t.Fatal("allowed skill not installed")
	}
	waitFor(t, func() bool { return strings.Contains(e.ch.last(), "✓ claude") })
	e.pass() // nothing new

	// Denied: not installed, never asked again for that version.
	e.agySkill("nope", "no thanks")
	e.pass(e.answer(policy.Deny, false))
	if e.installed("claude", "nope") {
		t.Fatal("denied skill installed")
	}
	e.pass()

	// Changed: asked again, agyh's untouched copies are updated.
	e.agySkill("fresh", "v2")
	prompts = e.pass(e.answer(policy.Allow, false))
	if !strings.Contains(prompts[0].Req.Summary, "Skill changed: fresh") {
		t.Fatalf("changed prompt: %q", prompts[0].Req.Summary)
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "skills", "fresh", "SKILL.md")); !strings.Contains(string(b), "v2") {
		t.Fatal("agyh's copy not updated")
	}

	// ⚡: approves this one and the next ones without asking.
	e.agySkill("auto1", "a")
	e.pass(e.answer(policy.Allow, true))
	e.agySkill("auto2", "b")
	e.pass()
	if !e.installed("claude", "auto2") || !strings.Contains(e.ch.last(), "⚡ auto") {
		t.Fatalf("auto: installed=%v last=%q", e.installed("claude", "auto2"), e.ch.last())
	}
}

func TestPropagationUnansweredIsNotReaskedRightAway(t *testing.T) {
	e := newPropEnv(t, 1)
	e.pass()
	e.agySkill("later", "x")
	e.pass(noAnswer) // times out
	if e.installed("claude", "later") {
		t.Fatal("installed without an answer")
	}
	e.pass() // within 24h: no new prompt
}

func TestPropagationNeverOverwritesUserFolders(t *testing.T) {
	e := newPropEnv(t, 5)
	// The user's own skill, there before propagation was enabled.
	put(t, filepath.Join(e.home, ".claude", "skills", "clash", "SKILL.md"), "the user's own\n")
	e.pass()
	e.agySkill("clash", "from a plugin")
	e.pass(e.answer(policy.Allow, false))
	if b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "skills", "clash", "SKILL.md")); string(b) != "the user's own\n" {
		t.Fatal("user's folder overwritten")
	}
	waitFor(t, func() bool { return strings.Contains(e.ch.last(), "left untouched") })
	if !e.installed("codex", "clash") {
		t.Fatal("other targets still get it")
	}
}

func TestPropagationIsOffUnlessEnabled(t *testing.T) {
	e := newPropEnv(t, 5)
	e.b.cfg.AutoPropagate = false
	e.agySkill("x", "y")
	e.pass()
	e.b.cfg.AutoPropagate, e.b.cfg.ManageAgents = true, false
	e.pass()
	if _, err := os.Stat(e.b.propagationPath); !os.IsNotExist(err) {
		t.Fatal("state written while disabled")
	}
}
