package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/store"
)

func seeded(t *testing.T, agents ...string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	titles := []string{"Pix refunds", "Auth refactor", "Queue retries"}
	for i, agent := range agents {
		s := model.Session{Agent: agent, ID: strings.Repeat(string(rune('a'+i)), 8),
			Title: titles[i], Workspace: "/w", UpdatedAt: now.Add(-time.Duration(i) * time.Hour), Path: "/p" + titles[i], ModTime: now}
		if err := st.Upsert(context.Background(), s, []model.Message{{Role: "user", Text: titles[i] + " details"}}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func key(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// send feeds a message and runs the resulting commands like Bubble Tea would,
// skipping the cursor blink (which ticks forever).
func send(m *Model, msg tea.Msg) {
	_, cmd := m.Update(msg)
	run(m, cmd)
}

func run(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c)
		}
	case debounceMsg, resultsMsg:
		send(m, msg)
	}
}

func TestSearchIsDebouncedAndStaleResultsDropped(t *testing.T) {
	m := New(context.Background(), seeded(t, "agy", "claude", "codex"), Options{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if v := m.View(); !strings.Contains(v, "Pix refunds") || !strings.Contains(v, "3 sessions") {
		t.Fatalf("initial view:\n%s", v)
	}
	// Typing fast: only the last keystroke's search may apply.
	_, c1 := m.Update(key("r"))
	_, c2 := m.Update(key("e"))
	run(m, c1) // stale debounce: ignored
	if !m.loading || len(m.hits) != 3 {
		t.Fatalf("stale search applied: loading=%v hits=%d", m.loading, len(m.hits))
	}
	send(m, key("t"))
	run(m, c2) // also stale
	if m.loading || len(m.hits) != 1 || m.hits[0].Title != "Queue retries" {
		t.Fatalf("live search: loading=%v %+v", m.loading, m.hits)
	}
	_, cmd := m.Update(key("ctrl+o"))
	if m.result.Resume == nil || m.result.Resume.Agent != "codex" || cmd == nil {
		t.Fatalf("resume not selected: %+v", m.result)
	}
}

func TestTabSkipsAgentsWithoutSessions(t *testing.T) {
	m := New(context.Background(), seeded(t, "agy", "claude"), Options{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	seen := []string{}
	for range 3 {
		send(m, key("tab"))
		seen = append(seen, m.agents[m.agent])
	}
	if strings.Join(seen, ",") != "agy,claude," {
		t.Fatalf("tab cycle = %v, want agy, claude, all (codex has no sessions)", seen)
	}
	if strings.Contains(m.View(), "codex") {
		t.Fatal("codex should not appear anywhere")
	}

	single := New(context.Background(), seeded(t, "claude"), Options{})
	send(single, key("tab"))
	if single.agent != 0 || strings.Contains(single.View(), "tab agent") {
		t.Fatal("with one agent, tab must do nothing and not be advertised")
	}
}

func TestFooterExplainsHowToConnectPhone(t *testing.T) {
	m := New(context.Background(), seeded(t, "agy"), Options{})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	v := m.View()
	if !strings.Contains(v, "agyh connect telegram") || !strings.Contains(v, "agyh connect whatsapp") {
		t.Fatalf("footer without connect hint:\n%s", v)
	}
	m = New(context.Background(), seeded(t, "agy"), Options{Remote: "telegram ✓ · bridge running"})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	if v := m.View(); strings.Contains(v, "agyh connect") || !strings.Contains(v, "bridge running") {
		t.Fatalf("connected footer:\n%s", v)
	}
}

func TestProjectPrompt(t *testing.T) {
	st := seeded(t, "agy", "claude", "codex")
	m := New(context.Background(), st, Options{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	send(m, key("tab")) // agy only
	if len(m.hits) != 1 || m.hits[0].Agent != "agy" {
		t.Fatalf("agent filter: %+v", m.hits)
	}
	send(m, key("ctrl+p"))
	for _, r := range "pagamentos" {
		send(m, key(string(r)))
	}
	send(m, key("enter"))
	ps, _ := st.Projects(context.Background())
	if len(ps) != 1 || ps[0].Name != "pagamentos" || ps[0].Sessions != 1 {
		t.Fatalf("project not saved: %+v (status %q)", ps, m.status)
	}
}
