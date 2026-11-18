package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func press(m *Model, t tea.KeyType) { send(m, tea.KeyMsg{Type: t}) }

func TestProjectsPickerScopesTheBrowser(t *testing.T) {
	st := seeded(t, "claude", "agy")
	ctx := context.Background()
	h, _ := st.Resolve(ctx, "aaaaaaaa")
	st.AddToProject(ctx, "pix", h, "")
	st.SetProjectColor(ctx, "pix", "purple")
	m := New(ctx, st, Options{})
	send(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	if len(m.hits) != 2 || !strings.Contains(m.View(), "#pix") {
		t.Fatalf("list should tag the session with its project:\n%s", m.View())
	}

	press(m, tea.KeyCtrlG)
	if m.mode != modeProjects || !strings.Contains(m.View(), "● pix") || !strings.Contains(m.View(), "All sessions") {
		t.Fatalf("projects view:\n%s", m.View())
	}
	press(m, tea.KeyDown)
	press(m, tea.KeyEnter)
	if m.project != "pix" || len(m.hits) != 1 || m.mode != modeList || !strings.Contains(m.View(), "● pix") {
		t.Fatalf("scoped to pix: project=%q hits=%d\n%s", m.project, len(m.hits), m.View())
	}
	press(m, tea.KeyCtrlG)
	press(m, tea.KeyUp)
	press(m, tea.KeyEnter)
	if m.project != "" || len(m.hits) != 2 {
		t.Fatalf("back to all sessions: project=%q hits=%d", m.project, len(m.hits))
	}
}

func TestNewSessionPanel(t *testing.T) {
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	lookPath = func(bin string) (string, error) {
		if bin == "codex" {
			return "", errors.New("not found")
		}
		return "/bin/" + bin, nil
	}
	ws := t.TempDir()
	st := seeded(t, "claude")
	m := New(context.Background(), st, Options{Project: "pix", Workspaces: []string{ws}})
	send(m, tea.WindowSizeMsg{Width: 120, Height: 30})

	press(m, tea.KeyCtrlN)
	view := m.View()
	if m.mode != modeNew || !strings.Contains(view, "claude") || !strings.Contains(view, "agy") || strings.Contains(view, "codex") {
		t.Fatalf("only installed agents are offered:\n%s", view)
	}
	if !strings.Contains(view, "● pix") {
		t.Fatalf("the current project is shown:\n%s", view)
	}

	press(m, tea.KeyTab) // agy
	for m.newDirs[m.newDir] != ws {
		press(m, tea.KeyDown)
	}
	send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("fix the build")})
	press(m, tea.KeyEnter)
	n := m.result.New
	if n == nil || n.Agent != "agy" || n.Dir != ws || n.Prompt != "fix the build" || n.Project != "pix" {
		t.Fatalf("result: %+v", n)
	}

	// esc cancels without a result.
	m2 := New(context.Background(), st, Options{})
	send(m2, tea.WindowSizeMsg{Width: 120, Height: 30})
	press(m2, tea.KeyCtrlN)
	press(m2, tea.KeyEsc)
	if m2.mode != modeList || m2.result.New != nil {
		t.Fatal("esc must cancel")
	}

	// No agent installed: a clear status instead of an empty panel.
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	m3 := New(context.Background(), st, Options{})
	send(m3, tea.WindowSizeMsg{Width: 120, Height: 30})
	press(m3, tea.KeyCtrlN)
	if m3.mode != modeList || !strings.Contains(m3.status, "no agent CLI") {
		t.Fatalf("no agents: mode=%v status=%q", m3.mode, m3.status)
	}
}
