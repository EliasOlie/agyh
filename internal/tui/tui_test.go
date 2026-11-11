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

func seeded(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	for i, title := range []string{"Pix refunds", "Auth refactor", "Queue retries"} {
		s := model.Session{Agent: []string{"agy", "claude", "codex"}[i], ID: strings.Repeat(string(rune('a'+i)), 8),
			Title: title, Workspace: "/w", UpdatedAt: now.Add(-time.Duration(i) * time.Hour), Path: "/p" + title, ModTime: now}
		if err := st.Upsert(context.Background(), s, []model.Message{{Role: "user", Text: title + " details"}}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
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

func TestSearchFilterAndResume(t *testing.T) {
	m := New(context.Background(), seeded(t), "", "")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if v := m.View(); !strings.Contains(v, "Pix refunds") || !strings.Contains(v, "3 sessions") {
		t.Fatalf("initial view:\n%s", v)
	}
	for _, r := range "retr" {
		m.Update(key(string(r)))
	}
	if len(m.hits) != 1 || m.hits[0].Title != "Queue retries" {
		t.Fatalf("live search: %+v", m.hits)
	}
	_, cmd := m.Update(key("ctrl+o"))
	if m.result.Resume == nil || m.result.Resume.Agent != "codex" || cmd == nil {
		t.Fatalf("resume not selected: %+v", m.result)
	}
}

func TestAgentTabAndProjectPrompt(t *testing.T) {
	st := seeded(t)
	m := New(context.Background(), st, "", "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(key("tab")) // agy only
	if len(m.hits) != 1 || m.hits[0].Agent != "agy" {
		t.Fatalf("agent filter: %+v", m.hits)
	}
	m.Update(key("ctrl+p"))
	for _, r := range "pagamentos" {
		m.Update(key(string(r)))
	}
	m.Update(key("enter"))
	ps, _ := st.Projects(context.Background())
	if len(ps) != 1 || ps[0].Name != "pagamentos" || ps[0].Sessions != 1 {
		t.Fatalf("project not saved: %+v (status %q)", ps, m.status)
	}
}
