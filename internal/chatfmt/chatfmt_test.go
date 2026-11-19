package chatfmt

import (
	"strings"
	"testing"
)

// A typical agent reply: what the phone used to show as one run of raw text.
const reply = "## Pronto\n\n" +
	"Criei o **manual de marca** em `docs/brand.pdf` e atualizei _2 arquivos_:\n\n" +
	"- `README.md`\n- [x] testes passando\n  1. sub item\n\n" +
	"```bash\ngo test ./... && echo <ok>\n```\n\n" +
	"> nota: rode `make` antes\n\n" +
	"| Arquivo | Linhas |\n|---|---:|\n| a.go | 10 |\n| main_test.go | 200 |\n\n" +
	"Veja [a doc](https://example.com/x?a=1&b=2) e snake_case_name, 2 * 3 * 4."

func TestTelegram(t *testing.T) {
	got := Telegram(reply)
	for _, want := range []string{
		"<b>Pronto</b>",
		"<b>manual de marca</b>",
		"<code>docs/brand.pdf</code>",
		"<i>2 arquivos</i>",
		"• <code>README.md</code>",
		"☑ testes passando",
		"    1. sub item",
		`<pre><code class="language-bash">go test ./... &amp;&amp; echo &lt;ok&gt;</code></pre>`,
		"<blockquote>nota: rode <code>make</code> antes</blockquote>",
		"<pre>Arquivo       Linhas\na.go          10\nmain_test.go  200</pre>",
		`<a href="https://example.com/x?a=1&amp;b=2">a doc</a>`,
		"snake_case_name, 2 * 3 * 4.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestWhatsApp(t *testing.T) {
	got := WhatsApp(reply)
	for _, want := range []string{
		"*Pronto*",
		"*manual de marca*",
		"`docs/brand.pdf`",
		"_2 arquivos_",
		"• `README.md`",
		"```\ngo test ./... && echo <ok>\n```",
		"> nota: rode `make` antes",
		"a doc (https://example.com/x?a=1&b=2)",
		"snake_case_name",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "<b>") {
		t.Errorf("leftover markup:\n%s", got)
	}
}

func TestUnclosedFenceAndEscapes(t *testing.T) {
	// A notification truncated in the middle of a code block.
	got := Telegram("veja:\n```go\nfunc main() {")
	if got != "veja:\n\n<pre><code class=\"language-go\">func main() {</code></pre>" {
		t.Fatalf("%q", got)
	}
	if got := Telegram(EscapeMD("a *b* `c` [d](e) <f>")); got != "a *b* `c` [d](e) &lt;f&gt;" {
		t.Fatalf("escaped text was formatted: %q", got)
	}
	if got := Telegram(`<script>alert(1)</script> [x](javascript:alert(1))`); strings.Contains(got, "<script") || strings.Contains(got, `href="javascript`) {
		t.Fatalf("unsafe output: %q", got)
	}
}

func TestSplitHTMLKeepsEveryPartValid(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<b>Resumo</b>\n<pre><code class=\"language-go\">")
	for i := range 400 {
		sb.WriteString("fmt.Println(\"linha &amp; " + strings.Repeat("x", i%30) + "\")\n")
	}
	sb.WriteString("</code></pre>\nfim")
	parts := SplitHTML(sb.String(), 1000)
	if len(parts) < 10 {
		t.Fatalf("only %d parts", len(parts))
	}
	for i, p := range parts {
		if len(p) > 1000 {
			t.Fatalf("part %d has %d bytes", i, len(p))
		}
		if len(tagsOpenAfter(nil, p)) != 0 {
			t.Fatalf("part %d leaves a tag open:\n%s", i, p)
		}
		if i > 0 && i < len(parts)-1 && !strings.HasPrefix(p, `<pre><code class="language-go">`) {
			t.Fatalf("part %d does not reopen the code block:\n%.80s", i, p)
		}
		if strings.Contains(p, "&am\n") || strings.HasSuffix(strings.TrimSuffix(p, "</code></pre>"), "&amp") {
			t.Fatalf("part %d cut an entity", i)
		}
	}
	if PlainFromHTML(parts[0])[:6] != "Resumo" {
		t.Fatalf("plain: %q", PlainFromHTML(parts[0])[:20])
	}
}

func TestSplitHTMLLongLineWithoutBreaks(t *testing.T) {
	s := "<code>" + strings.Repeat("ação ", 500) + "</code>"
	for i, p := range SplitHTML(s, 700) {
		if len(p) > 700 || !strings.HasPrefix(p, "<code>") || !strings.HasSuffix(p, "</code>") {
			t.Fatalf("part %d: %d bytes %q…", i, len(p), p[:20])
		}
		if !strings.Contains(p, "ação") || strings.ContainsRune(p, '�') {
			t.Fatalf("part %d broke a character", i)
		}
	}
}

func TestSplitTextReopensCodeBlocks(t *testing.T) {
	s := "intro\n```\n" + strings.Repeat("linha de código\n", 100) + "```\nfim"
	parts := SplitText(s, 300)
	for i, p := range parts {
		if len(p) > 300 {
			t.Fatalf("part %d too long: %d", i, len(p))
		}
		if strings.Count(p, "```")%2 != 0 {
			t.Fatalf("part %d leaves a block open:\n%s", i, p)
		}
	}
	if !strings.HasSuffix(parts[len(parts)-1], "```\nfim") {
		t.Fatalf("last part: %q", parts[len(parts)-1])
	}
}
