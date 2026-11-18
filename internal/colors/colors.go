// Package colors gives every project a stable color: the user's choice, or
// one derived from its name, so the same project looks the same in the
// browser, the CLI tables and chat.
package colors

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
)

// Color is one palette entry: a terminal color and its closest chat emoji.
type Color struct {
	Name  string
	Hex   string
	Emoji string
}

// Palette is readable on dark and light terminals.
var Palette = []Color{
	{"blue", "#60A5FA", "🟦"},
	{"green", "#34D399", "🟩"},
	{"orange", "#FB923C", "🟧"},
	{"purple", "#A78BFA", "🟪"},
	{"red", "#F87171", "🟥"},
	{"yellow", "#FACC15", "🟨"},
	{"pink", "#F472B6", "🩷"},
	{"cyan", "#22D3EE", "🩵"},
	{"brown", "#C08457", "🟫"},
	{"gray", "#9CA3AF", "⬜"},
}

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// Parse validates a user color: a palette name or #RRGGBB. "" resets.
func Parse(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "auto" {
		return "", nil
	}
	for _, c := range Palette {
		if c.Name == s {
			return c.Name, nil
		}
	}
	if hexColor.MatchString(s) {
		return strings.ToUpper(s), nil
	}
	names := make([]string, len(Palette))
	for i, c := range Palette {
		names[i] = c.Name
	}
	return "", fmt.Errorf("unknown color %q: use one of %s, a #RRGGBB hex value, or auto", s, strings.Join(names, ", "))
}

// For returns a project's color: stored (a palette name or #RRGGBB) or,
// when empty, derived from the name.
func For(project, stored string) Color {
	if stored != "" {
		for _, c := range Palette {
			if c.Name == stored {
				return c
			}
		}
		if hexColor.MatchString(stored) {
			return Color{Name: stored, Hex: strings.ToUpper(stored), Emoji: nearest(stored).Emoji}
		}
	}
	h := fnv.New32a()
	h.Write([]byte(project))
	return Palette[h.Sum32()%uint32(len(Palette))]
}

// nearest finds the palette color closest to a hex color (for the emoji).
func nearest(hex string) Color {
	r, g, b := rgb(hex)
	best, bestD := Palette[0], -1
	for _, c := range Palette {
		cr, cg, cb := rgb(c.Hex)
		d := (r-cr)*(r-cr) + (g-cg)*(g-cg) + (b-cb)*(b-cb)
		if bestD < 0 || d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func rgb(hex string) (int, int, int) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	return int(v >> 16 & 0xFF), int(v >> 8 & 0xFF), int(v & 0xFF)
}
