// Package chatfmt turns the Markdown agents write into what each chat app
// renders: Telegram's HTML subset (code blocks get a copy button, inline
// code copies on tap) and WhatsApp's own markup. It also splits long
// messages without breaking a code block or leaving a tag open.
//
// It covers what agents actually produce, not all of CommonMark: fenced code,
// headings, lists (with task boxes), quotes, rules, pipe tables (shown as a
// monospace block), and inline code, bold, italic, strikethrough and links.
// Anything it does not recognise is shown as plain text, never dropped.
package chatfmt

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type kind int

const (
	para kind = iota
	heading
	item
	quote
	code
	table
	rule
)

type block struct {
	kind   kind
	text   string // inline Markdown (code: raw text)
	lang   string // code
	indent int    // item nesting level
	marker string // item: "•", "1.", "☐", "☑"
	gap    bool   // preceded by a blank line
}

var (
	reHeading  = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	reRule     = regexp.MustCompile(`^ {0,3}(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,}|(?:_[ \t]*){3,})$`)
	reItem     = regexp.MustCompile(`^([ \t]*)([-*+]|\d{1,9}[.)])[ \t]+(.*)$`)
	reTask     = regexp.MustCompile(`^\[([ xX])\][ \t]+`)
	reQuote    = regexp.MustCompile(`^ {0,3}>[ \t]?(.*)$`)
	reFence    = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*([^`\\s]*)")
	reTableSep = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	reLang     = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,20}$`)
)

func parse(md string) []block {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var out []block
	gap := false
	add := func(b block) {
		b.gap = gap && len(out) > 0
		gap = false
		out = append(out, b)
	}
	last := func() *block {
		if len(out) == 0 {
			return nil
		}
		return &out[len(out)-1]
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			gap = true
			continue
		}
		if m := reFence.FindStringSubmatch(line); m != nil {
			fence := m[1]
			var body []string
			for i++; i < len(lines); i++ {
				t := strings.TrimSpace(lines[i])
				if strings.HasPrefix(t, fence[:3]) && strings.Trim(t, fence[:1]) == "" && len(t) >= len(fence) {
					break
				}
				body = append(body, lines[i])
			}
			lang := m[2]
			if !reLang.MatchString(lang) {
				lang = ""
			}
			add(block{kind: code, text: strings.Join(body, "\n"), lang: lang})
			continue
		}
		if m := reHeading.FindStringSubmatch(line); m != nil {
			add(block{kind: heading, text: m[2]})
			continue
		}
		if reRule.MatchString(line) {
			add(block{kind: rule})
			continue
		}
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "|") {
			var rows []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				if !reTableSep.MatchString(lines[i]) {
					rows = append(rows, strings.TrimSpace(lines[i]))
				}
			}
			i--
			add(block{kind: table, text: alignTable(rows)})
			continue
		}
		if m := reItem.FindStringSubmatch(line); m != nil {
			marker, text := "•", m[3]
			if c := m[2][0]; c >= '0' && c <= '9' {
				marker = strings.TrimRight(m[2], ".)") + "."
			}
			if t := reTask.FindStringSubmatch(text); t != nil {
				marker, text = "☐", text[len(t[0]):]
				if t[1] != " " {
					marker = "☑"
				}
			}
			indent := len(strings.ReplaceAll(m[1], "\t", "    ")) / 2
			add(block{kind: item, text: text, marker: marker, indent: min(indent, 4)})
			continue
		}
		if m := reQuote.FindStringSubmatch(line); m != nil {
			if l := last(); l != nil && l.kind == quote && !gap {
				l.text += "\n" + m[1]
			} else {
				add(block{kind: quote, text: m[1]})
			}
			continue
		}
		// A plain line continues the paragraph or list item right above it.
		if l := last(); l != nil && !gap && (l.kind == para || l.kind == item) {
			l.text += "\n" + strings.TrimSpace(line)
			continue
		}
		add(block{kind: para, text: strings.TrimSpace(line)})
	}
	return out
}

// alignTable pads pipe-table cells into columns, for a monospace block.
func alignTable(rows []string) string {
	var cells [][]string
	var widths []int
	for _, r := range rows {
		r = strings.TrimSuffix(strings.TrimPrefix(r, "|"), "|")
		var row []string
		for j, c := range strings.Split(r, "|") {
			c = strings.TrimSpace(stripInline(c))
			row = append(row, c)
			if j >= len(widths) {
				widths = append(widths, 0)
			}
			widths[j] = max(widths[j], utf8.RuneCountInString(c))
		}
		cells = append(cells, row)
	}
	var sb strings.Builder
	for i, row := range cells {
		if i > 0 {
			sb.WriteByte('\n')
		}
		for j, c := range row {
			if j > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(c)
			if j < len(row)-1 {
				sb.WriteString(strings.Repeat(" ", widths[j]-utf8.RuneCountInString(c)))
			}
		}
	}
	return sb.String()
}

// --- inline

type span struct {
	kind     byte // 't' text, 'c' code, 'b' bold, 'i' italic, 's' strike, 'a' link
	text     string
	url      string
	children []span
}

func isPunct(r byte) bool { return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r) >= 0 }

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func runeBefore(s string, i int) rune {
	if i <= 0 {
		return ' '
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func runeAt(s string, i int) rune {
	if i >= len(s) {
		return ' '
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

// closing finds the closing delimiter for an emphasis opened at i.
func closing(s string, from int, delim string) int {
	for j := from; j < len(s); {
		k := strings.Index(s[j:], delim)
		if k < 0 {
			return -1
		}
		k += j
		if k > from && !unicode.IsSpace(runeBefore(s, k)) && (k == 0 || s[k-1] != '\\') {
			if delim[0] != '_' || !isWord(runeAt(s, k+len(delim))) {
				return k
			}
		}
		j = k + len(delim)
	}
	return -1
}

func inline(s string) []span {
	var out []span
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, span{kind: 't', text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			text.WriteByte(s[i+1])
			i += 2
			continue
		case c == '`':
			n := 0
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			fence := s[i : i+n]
			if k := strings.Index(s[i+n:], fence); k >= 0 {
				body := s[i+n : i+n+k]
				if len(body) > 1 && body[0] == ' ' && body[len(body)-1] == ' ' && strings.TrimSpace(body) != "" {
					body = body[1 : len(body)-1]
				}
				flush()
				out = append(out, span{kind: 'c', text: body})
				i += n + k + n
				continue
			}
			text.WriteString(fence)
			i += n
			continue
		case c == '*' || c == '_' || c == '~':
			delim := string(c)
			if i+1 < len(s) && s[i+1] == c {
				delim += string(c)
			}
			if c == '~' && len(delim) == 1 {
				break
			}
			start := i + len(delim)
			opens := start < len(s) && !unicode.IsSpace(runeAt(s, start)) &&
				!(c == '_' && isWord(runeBefore(s, i)))
			if opens {
				if k := closing(s, start, delim); k > start {
					flush()
					kind := byte('i')
					switch {
					case c == '~':
						kind = 's'
					case len(delim) == 2:
						kind = 'b'
					}
					out = append(out, span{kind: kind, children: inline(s[start:k])})
					i = k + len(delim)
					continue
				}
			}
			text.WriteString(delim)
			i += len(delim)
			continue
		case c == '[':
			if end := strings.Index(s[i:], "]("); end > 0 {
				label := s[i+1 : i+end]
				rest := s[i+end+2:]
				if close := strings.IndexByte(rest, ')'); close > 0 && !strings.ContainsAny(label, "\n") {
					url := strings.TrimSpace(rest[:close])
					flush()
					out = append(out, span{kind: 'a', url: url, children: inline(label)})
					i += end + 2 + close + 1
					continue
				}
			}
		case c == '<':
			if end := strings.IndexByte(s[i:], '>'); end > 0 {
				u := s[i+1 : i+end]
				if safeURL(u) && !strings.ContainsAny(u, " \n") {
					flush()
					out = append(out, span{kind: 'a', url: u, children: []span{{kind: 't', text: u}}})
					i += end + 1
					continue
				}
			}
		}
		text.WriteByte(c)
		i++
	}
	flush()
	return out
}

func safeURL(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:") || strings.HasPrefix(l, "tg://")
}

// stripInline removes inline markers, for monospace table cells.
func stripInline(s string) string { return plain(inline(s)) }

func plain(spans []span) string {
	var sb strings.Builder
	for _, sp := range spans {
		switch sp.kind {
		case 't', 'c':
			sb.WriteString(sp.text)
		default:
			sb.WriteString(plain(sp.children))
		}
	}
	return sb.String()
}

// EscapeMD makes plain text (a title, a path) safe to embed in Markdown.
func EscapeMD(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '`', '*', '_', '~', '[', ']', '<', '#', '|', '>':
			sb.WriteByte('\\')
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// --- renderers

type renderer interface {
	span(sb *strings.Builder, sp span)
	text(s string) string
	heading(s string) string
	quote(s string) string
	code(body, lang string) string
}

func render(md string, r renderer) string {
	var sb strings.Builder
	for i, b := range parse(md) {
		if i > 0 {
			sb.WriteString("\n")
			if b.gap || b.kind == code || b.kind == table || b.kind == heading {
				sb.WriteString("\n")
			}
		}
		switch b.kind {
		case para:
			sb.WriteString(spans(r, b.text))
		case heading:
			sb.WriteString(r.heading(spans(r, b.text)))
		case item:
			sb.WriteString(strings.Repeat("    ", b.indent) + b.marker + " " + spans(r, b.text))
		case quote:
			sb.WriteString(r.quote(spans(r, b.text)))
		case code, table:
			sb.WriteString(r.code(b.text, b.lang))
		case rule:
			sb.WriteString(r.text("──────────"))
		}
	}
	return strings.TrimSpace(sb.String())
}

func spans(r renderer, s string) string {
	var sb strings.Builder
	for _, sp := range inline(s) {
		r.span(&sb, sp)
	}
	return sb.String()
}

// Telegram renders Markdown as Telegram's HTML parse mode.
func Telegram(md string) string { return render(md, tg{}) }

type tg struct{}

func (tg) text(s string) string    { return html.EscapeString(s) }
func (tg) heading(s string) string { return "<b>" + s + "</b>" }
func (tg) quote(s string) string   { return "<blockquote>" + s + "</blockquote>" }
func (tg) code(body, lang string) string {
	if lang != "" {
		return `<pre><code class="language-` + lang + `">` + html.EscapeString(body) + "</code></pre>"
	}
	return "<pre>" + html.EscapeString(body) + "</pre>"
}

func (r tg) span(sb *strings.Builder, sp span) {
	inner := func() {
		for _, c := range sp.children {
			r.span(sb, c)
		}
	}
	switch sp.kind {
	case 't':
		sb.WriteString(html.EscapeString(sp.text))
	case 'c':
		sb.WriteString("<code>" + html.EscapeString(sp.text) + "</code>")
	case 'b', 'i', 's':
		tag := string(sp.kind)
		sb.WriteString("<" + tag + ">")
		inner()
		sb.WriteString("</" + tag + ">")
	case 'a':
		if !safeURL(sp.url) {
			inner()
			sb.WriteString(" (" + html.EscapeString(sp.url) + ")")
			return
		}
		sb.WriteString(`<a href="` + html.EscapeString(sp.url) + `">`)
		inner()
		sb.WriteString("</a>")
	}
}

// TelegramPre is a code block with a copy button.
func TelegramPre(body, lang string) string {
	if !reLang.MatchString(lang) {
		lang = ""
	}
	return tg{}.code(body, lang)
}

// WhatsAppPre is a monospace block.
func WhatsAppPre(body string) string { return wa{}.code(body, "") }

// WhatsApp renders Markdown with WhatsApp's markup (*bold*, _italic_,
// ~strike~, `code`, ``` blocks ```, > quotes).
func WhatsApp(md string) string { return render(md, wa{}) }

type wa struct{}

func (wa) text(s string) string    { return s }
func (wa) heading(s string) string { return wrapWA("*", s) }
func (wa) quote(s string) string   { return "> " + strings.ReplaceAll(s, "\n", "\n> ") }
func (wa) code(body, _ string) string {
	return "```\n" + strings.ReplaceAll(body, "```", "'''") + "\n```"
}

// wrapWA applies a WhatsApp marker; it only renders when it hugs the text.
func wrapWA(m, s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	return m + t + m
}

func (r wa) span(sb *strings.Builder, sp span) {
	var inner strings.Builder
	for _, c := range sp.children {
		r.span(&inner, c)
	}
	switch sp.kind {
	case 't':
		sb.WriteString(sp.text)
	case 'c':
		if strings.Contains(sp.text, "`") {
			sb.WriteString("```" + sp.text + "```")
		} else {
			sb.WriteString("`" + sp.text + "`")
		}
	case 'b':
		sb.WriteString(wrapWA("*", inner.String()))
	case 'i':
		sb.WriteString(wrapWA("_", inner.String()))
	case 's':
		sb.WriteString(wrapWA("~", inner.String()))
	case 'a':
		label := inner.String()
		if label == sp.url || label == "" {
			sb.WriteString(sp.url)
		} else {
			sb.WriteString(label + " (" + sp.url + ")")
		}
	}
}
