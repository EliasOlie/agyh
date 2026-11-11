// Package tui is agyh's interactive browser: type to search every agent's
// history, Enter to read a session, Ctrl+O to resume it, Ctrl+P to save it
// into a project.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

// Result is what the user chose before leaving the TUI.
type Result struct {
	Resume *model.Session
}

type mode int

const (
	modeList mode = iota
	modeDetail
	modeProject
)

var agents = []string{"", "agy", "claude", "codex"}

var (
	accent    = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#A78BFA"}
	muted     = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	titleSt   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	mutedSt   = lipgloss.NewStyle().Foreground(muted)
	selSt     = lipgloss.NewStyle().Bold(true)
	userSt    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"})
	agentSt   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	statusSt  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#047857", Dark: "#34D399"})
	agentTags = map[string]lipgloss.Style{
		"agy":    lipgloss.NewStyle().Foreground(lipgloss.Color("#60A5FA")),
		"claude": lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B")),
		"codex":  lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")),
	}
)

type Model struct {
	ctx     context.Context
	st      *store.Store
	project string

	search  textinput.Model
	prompt  textinput.Model
	view    viewport.Model
	mode    mode
	agent   int
	hits    []store.Hit
	cursor  int
	offset  int
	width   int
	height  int
	status  string
	err     error
	result  Result
	current *store.Hit
}

// New builds the TUI. project optionally restricts it to one project.
func New(ctx context.Context, st *store.Store, initialQuery, project string) *Model {
	in := textinput.New()
	in.Placeholder = "search every session (accents and case ignored)…"
	in.Prompt = "🔎 "
	in.SetValue(initialQuery)
	in.Focus()
	pr := textinput.New()
	pr.Prompt = "project: "
	pr.Placeholder = "name (created if new)"
	m := &Model{ctx: ctx, st: st, project: project, search: in, prompt: pr, view: viewport.New(80, 20)}
	m.refresh()
	return m
}

// Run starts the program and returns the user's choice.
func Run(ctx context.Context, st *store.Store, initialQuery, project string) (Result, error) {
	m := New(ctx, st, initialQuery, project)
	final, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if err != nil {
		return Result{}, err
	}
	return final.(*Model).result, nil
}

func (m *Model) Init() tea.Cmd { return textinput.Blink }

func (m *Model) filter() store.Filter {
	return store.Filter{Agent: agents[m.agent], Project: m.project, Limit: 200, IncludeSubs: m.project != ""}
}

func (m *Model) refresh() {
	q := strings.TrimSpace(m.search.Value())
	var err error
	if q == "" {
		m.hits, err = m.st.List(m.ctx, m.filter())
	} else {
		m.hits, err = m.st.Search(m.ctx, q, m.filter())
	}
	m.err = err
	if m.cursor >= len(m.hits) {
		m.cursor = max(len(m.hits)-1, 0)
	}
	m.clampOffset()
}

func (m *Model) listHeight() int { return max(m.height-6, 3) }

func (m *Model) clampOffset() {
	h := m.listHeight() / 2 // two lines per row
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(m.offset, 0)
}

func (m *Model) selected() *store.Hit {
	if m.cursor < 0 || m.cursor >= len(m.hits) {
		return nil
	}
	return &m.hits[m.cursor]
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.search.Width = max(msg.Width-6, 10)
		m.view.Width, m.view.Height = msg.Width, max(msg.Height-5, 3)
		if m.mode == modeDetail && m.current != nil {
			m.view.SetContent(m.renderDetail(*m.current))
		}
		m.clampOffset()
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.mode {
		case modeDetail:
			return m.updateDetail(msg)
		case modeProject:
			return m.updateProject(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

func (m *Model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.search.Value() != "" {
			m.search.SetValue("")
			m.refresh()
			return m, nil
		}
		return m, tea.Quit
	case "up", "ctrl+k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "ctrl+j":
		m.cursor = min(m.cursor+1, max(len(m.hits)-1, 0))
	case "pgup":
		m.cursor = max(m.cursor-m.listHeight()/2, 0)
	case "pgdown":
		m.cursor = min(m.cursor+m.listHeight()/2, max(len(m.hits)-1, 0))
	case "tab":
		m.agent = (m.agent + 1) % len(agents)
		m.refresh()
	case "shift+tab":
		m.agent = (m.agent + len(agents) - 1) % len(agents)
		m.refresh()
	case "enter":
		if h := m.selected(); h != nil {
			m.openDetail(*h)
		}
	case "ctrl+o":
		return m.resume()
	case "ctrl+p":
		return m.askProject()
	case "ctrl+y":
		m.copyID()
	default:
		var cmd tea.Cmd
		before := m.search.Value()
		m.search, cmd = m.search.Update(msg)
		if m.search.Value() != before {
			m.cursor, m.offset = 0, 0
			m.refresh()
		}
		return m, cmd
	}
	m.clampOffset()
	return m, nil
}

func (m *Model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "backspace":
		m.mode = modeList
		return m, nil
	case "enter", "r", "ctrl+o":
		return m.resume()
	case "p", "ctrl+p":
		return m.askProject()
	case "y", "ctrl+y":
		m.copyID()
		return m, nil
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(msg)
	return m, cmd
}

func (m *Model) updateProject(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.search.Focus()
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.prompt.Value())
		if h := m.current; h != nil && name != "" {
			if err := m.st.AddToProject(m.ctx, name, h.Session, ""); err != nil {
				m.status = "✗ " + err.Error()
			} else {
				m.status = fmt.Sprintf("✓ saved %s into %s", h.ShortID(), name)
				m.refresh()
			}
		}
		m.mode = modeList
		m.search.Focus()
		return m, nil
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	return m, cmd
}

func (m *Model) askProject() (tea.Model, tea.Cmd) {
	h := m.selected()
	if m.mode == modeDetail {
		h = m.current
	}
	if h == nil {
		return m, nil
	}
	m.current = h
	m.prompt.SetValue("")
	m.prompt.Focus()
	m.mode = modeProject
	return m, textinput.Blink
}

func (m *Model) resume() (tea.Model, tea.Cmd) {
	h := m.selected()
	if m.mode == modeDetail && m.current != nil {
		h = m.current
	}
	if h == nil {
		return m, nil
	}
	s := h.Session
	m.result.Resume = &s
	return m, tea.Quit
}

func (m *Model) copyID() {
	h := m.selected()
	if m.mode == modeDetail && m.current != nil {
		h = m.current
	}
	if h == nil {
		return
	}
	_, _ = osc52.New(h.ID).WriteTo(os.Stderr)
	m.status = "✓ copied " + h.ID
}

func (m *Model) openDetail(h store.Hit) {
	hc := h
	m.current = &hc
	m.mode = modeDetail
	m.view.SetContent(m.renderDetail(h))
	m.view.GotoTop()
}

func (m *Model) renderDetail(h store.Hit) string {
	w := max(m.width-2, 20)
	var sb strings.Builder
	sb.WriteString(titleSt.Render(h.Title) + "\n")
	sb.WriteString(mutedSt.Render(fmt.Sprintf("%s · %s · %s · %d messages · %s",
		h.Agent, hook.ShortenHome(h.Workspace), h.UpdatedAt.Local().Format("2006-01-02 15:04"), h.Messages, h.ID)) + "\n")
	if len(h.Projects) > 0 {
		sb.WriteString(statusSt.Render("projects: "+strings.Join(h.Projects, ", ")) + "\n")
	}
	sb.WriteString("\n")
	src := source.ByName(h.Agent)
	if src == nil {
		return sb.String()
	}
	meta := h.Session
	_, msgs, err := src.Load(m.ctx, source.Ref{Agent: h.Agent, ID: h.ID, Path: h.Path, Meta: &meta})
	if err != nil {
		sb.WriteString("could not read transcript: " + err.Error())
		return sb.String()
	}
	body := lipgloss.NewStyle().Width(w)
	for _, msg := range msgs {
		label := agentSt.Render("◆ " + h.Agent)
		if msg.Role == "user" {
			label = userSt.Render("› you")
		}
		text := msg.Text
		if r := []rune(text); len(r) > 4000 {
			text = string(r[:4000]) + "…"
		}
		sb.WriteString(label + mutedSt.Render("  "+msg.At.Local().Format("15:04")) + "\n")
		sb.WriteString(body.Render(text) + "\n\n")
	}
	return sb.String()
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	switch m.mode {
	case modeDetail:
		return m.view.View() + "\n" + mutedSt.Render(fmt.Sprintf(
			"↑/↓ scroll · enter resume · p save to project · y copy id · esc back   %3.f%%", m.view.ScrollPercent()*100))
	}
	var sb strings.Builder
	header := titleSt.Render("agyh")
	scope := "all agents"
	if agents[m.agent] != "" {
		scope = agents[m.agent]
	}
	if m.project != "" {
		scope += " · project " + m.project
	}
	sb.WriteString(header + mutedSt.Render(fmt.Sprintf("  %s · %d sessions", scope, len(m.hits))) + "\n")
	if m.mode == modeProject {
		sb.WriteString(m.prompt.View() + "\n\n")
	} else {
		sb.WriteString(m.search.View() + "\n\n")
	}
	if m.err != nil {
		sb.WriteString("error: " + m.err.Error() + "\n")
	}
	if len(m.hits) == 0 && m.err == nil {
		sb.WriteString(mutedSt.Render("  nothing here yet — run `agyh index` or change the filter") + "\n")
	}
	rows := m.listHeight() / 2
	for i := m.offset; i < len(m.hits) && i < m.offset+rows; i++ {
		sb.WriteString(m.renderRow(i) + "\n")
	}
	sb.WriteString("\n" + mutedSt.Render("type to search · ↑/↓ · enter open · ctrl+o resume · ctrl+p project · tab agent · ctrl+y copy · esc quit"))
	if m.status != "" {
		sb.WriteString("  " + statusSt.Render(m.status))
	}
	return sb.String()
}

func (m *Model) renderRow(i int) string {
	h := m.hits[i]
	tag := agentTags[h.Agent].Render(fmt.Sprintf("%-6s", h.Agent))
	when := ago(h.UpdatedAt)
	ws := hook.ShortenHome(h.Workspace)
	titleW := max(m.width-len(when)-14, 10)
	title := model.Truncate(h.Title, titleW)
	marker := "  "
	line1 := fmt.Sprintf("%s%s %s", marker, tag, title)
	if i == m.cursor {
		marker = titleSt.Render("▸ ")
		line1 = fmt.Sprintf("%s%s %s", marker, tag, selSt.Render(title))
	}
	pad := max(m.width-lipgloss.Width(line1)-len(when)-1, 1)
	line1 += strings.Repeat(" ", pad) + mutedSt.Render(when)
	second := ws
	if len(h.Projects) > 0 {
		second += " · #" + strings.Join(h.Projects, " #")
	}
	if h.Snippet != "" {
		second += " · " + h.Snippet
	} else if h.Preview != "" {
		second += " · " + h.Preview
	}
	return line1 + "\n" + mutedSt.Render("         "+model.Truncate(second, max(m.width-10, 10)))
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}
