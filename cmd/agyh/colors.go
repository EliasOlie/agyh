package main

import (
	"context"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/EliasOlie/agyh/internal/colors"
	"github.com/EliasOlie/agyh/internal/store"
)

// useColor: only on a terminal, and never when NO_COLOR is set
// (https://no-color.org): pipes, --json and tests stay plain text.
var useColor = func() bool {
	return os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stdout.Fd()))
}

// paint renders text in a project's color when the output is a terminal.
func paint(text, project, stored string) string {
	if !useColor() {
		return text
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colors.For(project, stored).Hex)).Render(text)
}

// projectTags renders "#a #b", each in its project's color.
func projectTags(projects []string, stored map[string]string) string {
	tags := make([]string, len(projects))
	for i, p := range projects {
		tags[i] = paint("#"+p, p, stored[p])
	}
	return strings.Join(tags, " ")
}

// projectColors reads stored project colors from an open index (best
// effort: colors are cosmetic, so errors just mean derived colors).
func projectColors(ctx context.Context, st *store.Store) map[string]string {
	if !useColor() {
		return nil
	}
	m, _ := st.ProjectColors(ctx)
	return m
}

func paletteNames() string {
	names := make([]string, len(colors.Palette))
	for i, c := range colors.Palette {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}
