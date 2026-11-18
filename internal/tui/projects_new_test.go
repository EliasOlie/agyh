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

func typeText(m *Model, s string) { send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }

func TestProjectActionsInPicker(t *testing.T) {
	st := seeded(t, "claude", "agy")
	ctx := context.Background()
	a, _ := st.Resolve(ctx, "aaaaaaaa")
	b, _ := st.Resolve(ctx, "bbbbbbbb")
	st.AddToProject(ctx, "conecta-me", a, "")
	st.AddToProject(ctx, "conectame", b, "")
	m := New(ctx, st, Options{})
	send(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	press(m, tea.KeyCtrlG)
	pick := func(name string) {
		for i, p := range m.projects {
			if p.Name == name {
				m.projCursor = i + 1
			}
		}
	}

	// merge conectame into conecta-me (tab completes the target)
	pick("conectame")
	typeText(m, "m")
	typeText(m, "conecta-")
	press(m, tea.KeyTab)
	if m.prompt.Value() != "conecta-me" {
		t.Fatalf("tab completion: %q", m.prompt.Value())
	}
	press(m, tea.KeyEnter)
	if len(m.projects) != 1 || m.projects[0].Sessions != 2 || !strings.Contains(m.status, "moved 1 sessions") {
		t.Fatalf("merge: %+v %q", m.projects, m.status)
	}

	// color cycles
	pick("conecta-me")
	before := m.projects[0].Color
	typeText(m, "c")
	if m.projects[0].Color == before || m.projects[0].Color == "" {
		t.Fatalf("color: %q -> %q", before, m.projects[0].Color)
	}

	// rename
	pick("conecta-me")
	typeText(m, "r")
	typeText(m, "conecta")
	press(m, tea.KeyEnter)
	if m.projects[0].Name != "conecta" {
		t.Fatalf("rename: %+v %q", m.projects, m.status)
	}

	// delete asks first; n keeps it, y deletes it, sessions stay
	pick("conecta")
	typeText(m, "d")
	if !strings.Contains(m.View(), "Its 2 sessions are kept") {
		t.Fatalf("delete confirmation:\n%s", m.View())
	}
	typeText(m, "n")
	if len(m.projects) != 1 {
		t.Fatal("n must keep the project")
	}
	typeText(m, "d")
	typeText(m, "y")
	if len(m.projects) != 0 {
		t.Fatalf("delete: %+v", m.projects)
	}
	if c, _ := st.Count(ctx); c["claude"]+c["agy"] != 2 {
		t.Fatal("sessions must survive")
	}
}

func TestSavePromptSteersAwayFromDuplicates(t *testing.T) {
	st := seeded(t, "claude", "agy")
	ctx := context.Background()
	a, _ := st.Resolve(ctx, "aaaaaaaa")
	st.AddToProject(ctx, "conecta-me", a, "")
	m := New(ctx, st, Options{})
	send(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m.cursor = 1 // the agy session
	press(m, tea.KeyCtrlP)
	typeText(m, "conectame")
	press(m, tea.KeyEnter)
	if m.mode != modeProject || m.prompt.Value() != "conecta-me" || !strings.Contains(m.status, "looks like the existing conecta-me") {
		t.Fatalf("guard: mode=%v value=%q status=%q", m.mode, m.prompt.Value(), m.status)
	}
	press(m, tea.KeyEnter)
	ps, _ := st.Projects(ctx)
	if len(ps) != 1 || ps[0].Sessions != 2 {
		t.Fatalf("saved into the existing project: %+v", ps)
	}
}
