package chatfmt

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

var reTag = regexp.MustCompile(`<(/?)([a-z]+)[^>]*>`)

// SplitHTML cuts Telegram HTML into messages of at most n bytes, preferring
// line breaks. A tag open at a cut (a code block, a quote, bold) is closed at
// the end of one message and reopened at the start of the next, so each
// message is valid on its own: Telegram rejects the whole message otherwise.
func SplitHTML(s string, n int) []string {
	var out []string
	var open []string // full opening tags, outermost first
	for {
		prefix := strings.Join(open, "")
		if len(prefix)+len(s) <= n {
			if strings.TrimSpace(s) != "" {
				out = append(out, prefix+s)
			}
			return out
		}
		budget := n - len(prefix) - closingLen(open) - 64 // room for tags closed at the cut
		budget = max(budget, n/4)
		cut := cutPoint(s, budget)
		part := s[:cut]
		next := tagsOpenAfter(open, part)
		out = append(out, prefix+part+closeTags(next))
		open = next
		s = strings.TrimPrefix(s[cut:], "\n")
	}
}

// cutPoint picks where to split: the last line break within limit, else
// the limit itself, moved back out of a tag, an entity or a UTF-8 sequence.
func cutPoint(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	if nl := strings.LastIndexByte(s[:limit], '\n'); nl > limit/2 {
		return nl
	}
	cut := limit
	if lt := strings.LastIndexByte(s[:cut], '<'); lt >= 0 && !strings.Contains(s[lt:cut], ">") {
		cut = lt
	}
	if amp := strings.LastIndexByte(s[:cut], '&'); amp >= 0 && cut-amp < 10 && !strings.Contains(s[amp:cut], ";") {
		cut = amp
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 { // a single huge tag: give up on keeping it whole
		cut = limit
	}
	return cut
}

func tagsOpenAfter(open []string, part string) []string {
	stack := append([]string(nil), open...)
	for _, m := range reTag.FindAllStringSubmatch(part, -1) {
		if m[1] == "" {
			stack = append(stack, m[0])
			continue
		}
		for i := len(stack) - 1; i >= 0; i-- {
			if tagName(stack[i]) == m[2] {
				stack = append(stack[:i], stack[i+1:]...)
				break
			}
		}
	}
	return stack
}

func tagName(open string) string {
	m := reTag.FindStringSubmatch(open)
	if m == nil {
		return ""
	}
	return m[2]
}

func closeTags(open []string) string {
	var sb strings.Builder
	for i := len(open) - 1; i >= 0; i-- {
		sb.WriteString("</" + tagName(open[i]) + ">")
	}
	return sb.String()
}

func closingLen(open []string) int { return len(closeTags(open)) }

// PlainFromHTML strips tags, for a fallback send when Telegram refuses the markup.
func PlainFromHTML(s string) string {
	return html.UnescapeString(reTag.ReplaceAllString(s, ""))
}

// SplitText cuts WhatsApp text into messages of at most n bytes on line
// breaks, closing a ``` block at a cut and reopening it in the next message.
func SplitText(s string, n int) []string {
	var out []string
	inBlock := false
	for s != "" {
		prefix := ""
		if inBlock {
			prefix = "```\n"
		}
		if len(prefix)+len(s) <= n {
			out = append(out, prefix+s)
			break
		}
		cut := cutLine(s, n-len(prefix)-4)
		part := s[:cut]
		inBlock = inBlock != (strings.Count(part, "```")%2 == 1)
		if inBlock {
			part += "\n```"
		}
		out = append(out, prefix+part)
		s = strings.TrimLeft(s[cut:], "\n")
	}
	return out
}

func cutLine(s string, limit int) int {
	if nl := strings.LastIndexByte(s[:limit], '\n'); nl > limit/2 {
		return nl
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}
