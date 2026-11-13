package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/policy"
	"github.com/EliasOlie/agyh/internal/store"
	vaultsync "github.com/EliasOlie/agyh/internal/sync"
)

// Command describes one chat command, used for /help and Telegram's menu.
type Command struct {
	Name, Args, Help string
}

var Commands = []Command{
	{"chats", "[n]", "recent sessions / sessões recentes"},
	{"sessoes", "[n]", "sessões recentes de todos os agentes"},
	{"search", "<words>", "full-text search / busca em todo o histórico"},
	{"buscar", "<termos>", "busca textual em todas as sessões"},
	{"show", "<id>", "details and messages / detalhes e mensagens"},
	{"ver", "<id>", "ver conversa e detalhes de uma sessão"},
	{"projects", "", "list projects / listar projetos"},
	{"projetos", "", "listar todos os seus projetos"},
	{"project", "<name>", "sessions saved in a project"},
	{"projeto", "<nome>", "ver sessões de um projeto"},
	{"save", "<id> <project> [note]", "save session into a project"},
	{"salvar", "<id> <projeto> [nota]", "salvar sessão em um projeto"},
	{"new", "[agent] [@workspace] <prompt>", "start headless agent task"},
	{"novo", "[agent] [@workspace] <prompt>", "iniciar tarefa de agente em background"},
	{"ws", "", "workspaces available to /new"},
	{"pending", "", "approvals waiting for you"},
	{"pendentes", "", "aprovações aguardando resposta"},
	{"auto", "[off <id>]", "sessions in autonomous mode"},
	{"sync", "", "sync sessions with encrypted cloud vault"},
	{"ajuda", "", "onboarding e guia completo de comandos"},
	{"help", "", "command guide and onboarding"},
}

// HandleText processes one inbound chat message and replies on the same channel.
func (b *Bridge) HandleText(ctx context.Context, ch Channel, text string) {
	reply := b.Execute(ctx, ch.Markup(), ch.Name(), strings.TrimSpace(text))
	if reply == "" {
		return
	}
	if err := ch.Send(ctx, reply); err != nil {
		b.log.Warn("reply failed", "channel", ch.Name(), "err", err)
	}
}

// Execute runs a command and returns the reply text (empty for no reply).
func (b *Bridge) Execute(ctx context.Context, m Markup, origin, text string) string {
	if text == "" {
		return ""
	}
	// Text approvals ("y a1b2c3", "n", "a a1b2c3") for channels without buttons.
	if r, ok := b.textApproval(ctx, origin, text); ok {
		return r
	}
	if !strings.HasPrefix(text, "/") {
		return "Unknown message. Send /help for commands."
	}
	name, rest, _ := strings.Cut(text[1:], " ")
	name, _, _ = strings.Cut(name, "@") // Telegram appends @botname in groups
	rest = strings.TrimSpace(rest)

	switch strings.ToLower(name) {
	case "start":
		return b.onboarding(m)
	case "help", "ajuda":
		return b.help(m)
	case "chats", "history", "sessoes", "sessões", "historico", "histórico":
		n := 8
		if v, err := strconv.Atoi(rest); err == nil && v > 0 && v <= 30 {
			n = v
		}
		hits, err := b.store.List(ctx, store.Filter{Limit: n})
		return b.listReply(m, "Recent sessions / Sessões recentes", hits, err)
	case "search", "buscar", "pesquisar", "busca":
		if rest == "" {
			return "Usage / Uso: /search <words> ou /buscar <termos>"
		}
		hits, err := b.store.Search(ctx, rest, store.Filter{Limit: 8})
		return b.listReply(m, "Results for / Resultados para "+m.Bold(m.Escape(rest)), hits, err)
	case "show", "ver", "exibir":
		return b.show(ctx, m, rest)
	case "projects", "projetos":
		return b.projects(ctx, m)
	case "project", "projeto":
		if rest == "" {
			return "Usage / Uso: /project <name> ou /projeto <nome>"
		}
		hits, err := b.store.List(ctx, store.Filter{Project: rest, Limit: 20, IncludeSubs: true})
		return b.listReply(m, "Project / Projeto "+m.Bold(m.Escape(rest)), hits, err)
	case "save", "salvar":
		return b.save(ctx, m, rest)
	case "new", "novo", "run", "iniciar":
		return b.newSession(ctx, m, rest)
	case "ws", "cd", "workspaces":
		return b.workspaces(m)
	case "pending", "pendentes", "fila":
		return b.pending(m)
	case "auto", "autonomo", "autônomo":
		return b.auto(ctx, m, rest)
	case "sync", "sincronizar":
		return b.syncVault(ctx, m)
	}
	return "Unknown command /" + m.Escape(name) + ". Send /help or /ajuda."
}

func (b *Bridge) syncVault(ctx context.Context, m Markup) string {
	if !b.cfg.Sync.Enabled {
		return "Cloud vault is not configured. Add \"sync\" to your config."
	}
	engine, err := vaultsync.NewEngine(b.cfg.Sync, b.store)
	if err != nil {
		return "Error: " + m.Escape(err.Error())
	}
	res, err := engine.Sync(ctx)
	if err != nil {
		return "Sync failed: " + m.Escape(err.Error())
	}
	return fmt.Sprintf("☁️ Synced with vault: %d uploaded, %d downloaded (%d total)",
		res.Uploaded, res.Downloaded, res.Total)
}

func (b *Bridge) onboarding(m Markup) string {
	var sb strings.Builder
	sb.WriteString("👋 " + m.Bold("Bem-vindo ao agyh! / Welcome to agyh!") + "\n\n")
	sb.WriteString("O " + m.Bold("agyh") + " conecta seus coding agents (Antigravity, Claude Code, Codex) diretamente ao seu mensageiro, com histórico unificado e controle de permissões remoto.\n\n")
	sb.WriteString(m.Bold("🔐 Como responder aprovações / How to approve:") + "\n")
	sb.WriteString("Quando um agente precisar rodar um comando (ex: git push, migrations):\n")
	sb.WriteString("• No Telegram: toque diretamente nos botões.\n")
	sb.WriteString("• No WhatsApp ou por texto, responda:\n")
	sb.WriteString("  - " + m.Code("s") + ", " + m.Code("sim") + " ou " + m.Code("y") + " ➔ Permitir / Allow\n")
	sb.WriteString("  - " + m.Code("n") + ", " + m.Code("nao") + " ou " + m.Code("rejeitar") + " ➔ Negar / Deny\n")
	sb.WriteString("  - " + m.Code("a") + ", " + m.Code("auto") + " ou " + m.Code("sempre") + " ➔ Sessão Autônoma ⚡\n\n")
	sb.WriteString(m.Bold("📱 Comandos Principais / Main Commands:") + "\n")
	sb.WriteString("• " + m.Code("/sessoes") + " ou " + m.Code("/chats") + " — Ver histórico recente\n")
	sb.WriteString("• " + m.Code("/buscar <termo>") + " ou " + m.Code("/search <words>") + " — Busca textual instantânea\n")
	sb.WriteString("• " + m.Code("/ver <id>") + " ou " + m.Code("/show <id>") + " — Detalhes e mensagens da conversa\n")
	sb.WriteString("• " + m.Code("/novo [agent] <prompt>") + " ou " + m.Code("/new ...") + " — Iniciar tarefa em background\n")
	sb.WriteString("• " + m.Code("/projetos") + " ou " + m.Code("/projects") + " — Listar seus projetos organizados\n")
	sb.WriteString("• " + m.Code("/sync") + " — Sincronizar com o Cloud Vault criptografado\n")
	sb.WriteString("• " + m.Code("/ajuda") + " ou " + m.Code("/help") + " — Ver todos os comandos\n\n")
	sb.WriteString("💡 Dica: Envie " + m.Code("/sessoes") + " agora para ver suas sessões recentes!")
	return sb.String()
}

func (b *Bridge) help(m Markup) string {
	var sb strings.Builder
	sb.WriteString(m.Bold("agyh") + " — your coding-agent sessions, from anywhere / suas sessões de IA em qualquer lugar.\n\n")
	for _, c := range Commands {
		line := "/" + c.Name
		if c.Args != "" {
			line += " " + c.Args
		}
		fmt.Fprintf(&sb, "%s — %s\n", m.Escape(line), m.Escape(c.Help))
	}
	sb.WriteString("\n" + m.Bold("Aprovações / Approvals:") + " Toque nos botões ou responda " + m.Code("s <id>") + " (sim/allow), " +
		m.Code("n <id>") + " (não/deny) ou " + m.Code("a <id>") + " (autônomo/always).")
	return sb.String()
}

func (b *Bridge) listReply(m Markup, title string, hits []store.Hit, err error) string {
	if err != nil {
		return "Error: " + m.Escape(err.Error())
	}
	if len(hits) == 0 {
		return title + "\nNothing found."
	}
	var sb strings.Builder
	sb.WriteString(title + "\n")
	for i, h := range hits {
		fmt.Fprintf(&sb, "\n%d. %s %s\n   %s · %s · %s\n", i+1, m.Bold("["+h.Agent+"]"),
			m.Escape(model.Truncate(h.Title, 70)), m.Escape(shortWS(h.Workspace)), Ago(h.UpdatedAt), m.Code(h.ShortID()))
		if h.Snippet != "" {
			sb.WriteString("   " + m.Escape(model.Truncate(h.Snippet, 120)) + "\n")
		}
	}
	return sb.String()
}

func (b *Bridge) show(ctx context.Context, m Markup, ref string) string {
	if ref == "" {
		return "Usage: /show <id>"
	}
	sess, err := b.resolve(ctx, ref)
	if err != nil {
		return m.Escape(err.Error())
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n%s · %s · %d messages\n", m.Bold("["+sess.Agent+"]"), m.Escape(sess.Title),
		m.Escape(shortWS(sess.Workspace)), Ago(sess.UpdatedAt), sess.Messages)
	if sess.Preview != "" {
		sb.WriteString("\n" + m.Escape(model.Truncate(sess.Preview, 600)) + "\n")
	}
	fmt.Fprintf(&sb, "\nResume: %s", m.Code("agyh resume "+sess.ShortID()))
	return sb.String()
}

func (b *Bridge) projects(ctx context.Context, m Markup) string {
	ps, err := b.store.Projects(ctx)
	if err != nil {
		return "Error: " + m.Escape(err.Error())
	}
	if len(ps) == 0 {
		return "No projects yet. Save a session with /save <id> <project>."
	}
	var sb strings.Builder
	sb.WriteString(m.Bold("Projects") + "\n")
	for _, p := range ps {
		fmt.Fprintf(&sb, "\n• %s — %d sessions", m.Code(p.Name), p.Sessions)
		if !p.LastActive.IsZero() {
			sb.WriteString(" · " + Ago(p.LastActive))
		}
	}
	return sb.String()
}

func (b *Bridge) save(ctx context.Context, m Markup, rest string) string {
	f := strings.Fields(rest)
	if len(f) < 2 {
		return "Usage: /save <id> <project> [note]"
	}
	sess, err := b.resolve(ctx, f[0])
	if err != nil {
		return m.Escape(err.Error())
	}
	note := strings.Join(f[2:], " ")
	if err := b.store.AddToProject(ctx, f[1], sess, note); err != nil {
		return "Error: " + m.Escape(err.Error())
	}
	return fmt.Sprintf("Saved %s into %s.", m.Code(sess.ShortID()), m.Bold(m.Escape(f[1])))
}

func (b *Bridge) workspaces(m Markup) string {
	if len(b.cfg.Workspaces) == 0 {
		return "No workspaces configured. Add absolute paths to \"workspaces\" in the config."
	}
	var sb strings.Builder
	sb.WriteString(m.Bold("Workspaces") + " (use with /new @name)\n")
	for _, w := range b.cfg.Workspaces {
		fmt.Fprintf(&sb, "\n• %s  %s", m.Code(filepath.Base(w)), m.Escape(shortWS(w)))
	}
	return sb.String()
}

// newSession parses "/new [agent] [@workspace] <prompt>" and starts a runner in the background.
func (b *Bridge) newSession(ctx context.Context, m Markup, rest string) string {
	f := strings.Fields(rest)
	agent := ""
	if len(f) > 0 {
		if _, ok := b.cfg.Runners[f[0]]; ok {
			agent, f = f[0], f[1:]
		}
	}
	if agent == "" {
		agent = b.defaultRunner()
	}
	if agent == "" {
		return "No runner configured. Add one under \"runners\" in the config."
	}
	dir := ""
	if len(f) > 0 && strings.HasPrefix(f[0], "@") {
		ws, err := b.workspace(strings.TrimPrefix(f[0], "@"))
		if err != nil {
			return m.Escape(err.Error())
		}
		dir, f = ws, f[1:]
	}
	if dir == "" {
		if len(b.cfg.Workspaces) > 0 {
			dir = b.cfg.Workspaces[0]
		} else {
			dir, _ = os.UserHomeDir()
		}
	}
	prompt := strings.Join(f, " ")
	if prompt == "" {
		return "Usage: /new [agent] [@workspace] <prompt>"
	}
	argv := expandRunner(b.cfg.Runners[agent], prompt)
	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		start := time.Now()
		out, err := b.runner.Run(runCtx, dir, argv)
		took := time.Since(start).Round(time.Second)
		b.Broadcast(context.Background(), func(m Markup) string {
			status := "✅ " + m.Bold(agent) + " finished"
			if err != nil {
				status = "❌ " + m.Bold(agent) + " failed: " + m.Escape(err.Error())
			}
			return fmt.Sprintf("%s in %s (%s)\n\n%s", status, m.Escape(shortWS(dir)), took, m.Escape(tail(out, 3000)))
		})
	}()
	return fmt.Sprintf("🚀 Started %s in %s. I'll send the result here.", m.Bold(agent), m.Escape(shortWS(dir)))
}

func (b *Bridge) defaultRunner() string {
	if _, ok := b.cfg.Runners["claude"]; ok {
		return "claude"
	}
	for k := range b.cfg.Runners {
		return k
	}
	return ""
}

// workspace only accepts configured workspaces, so chat can't point a runner anywhere.
func (b *Bridge) workspace(name string) (string, error) {
	for _, w := range b.cfg.Workspaces {
		if filepath.Base(w) == name || w == name {
			return w, nil
		}
	}
	return "", fmt.Errorf("unknown workspace %q: see /ws", name)
}

func (b *Bridge) pending(m Markup) string {
	ps := b.approvals.Pending()
	if len(ps) == 0 {
		return "No pending approvals."
	}
	var sb strings.Builder
	sb.WriteString(m.Bold("Pending approvals") + "\n")
	for _, a := range ps {
		fmt.Fprintf(&sb, "\n%s [%s] %s", m.Code(a.ID), a.Req.Agent, m.Escape(model.Truncate(a.Req.Describe(), 120)))
	}
	return sb.String()
}

func (b *Bridge) auto(ctx context.Context, m Markup, rest string) string {
	f := strings.Fields(rest)
	if len(f) == 2 && f[0] == "off" {
		for _, k := range b.approvals.AutoSessions() {
			_, id, _ := strings.Cut(k, ":")
			if strings.HasPrefix(id, f[1]) {
				b.approvals.SetAuto(k, false)
				return "Autonomous mode off for " + m.Code(k) + "."
			}
		}
		return "No autonomous session matches " + m.Code(f[1]) + "."
	}
	keys := b.approvals.AutoSessions()
	if len(keys) == 0 {
		return "No session in autonomous mode. Use the ⚡ button (or a <id>) on an approval to enable it."
	}
	var sb strings.Builder
	sb.WriteString(m.Bold("Autonomous sessions") + " (fatal commands are still blocked)\n")
	for _, k := range keys {
		sb.WriteString("\n• " + m.Code(k))
	}
	sb.WriteString("\n\nDisable with /auto off <id>")
	return sb.String()
}

// textApproval handles "y <id>", "n <id>", "a <id>"; the id may be omitted
// when exactly one approval is pending.
func (b *Bridge) textApproval(ctx context.Context, origin, text string) (string, bool) {
	f := strings.Fields(strings.ToLower(text))
	if len(f) == 0 || len(f) > 2 {
		return "", false
	}
	var v policy.Verdict
	auto := false
	switch f[0] {
	case "y", "yes", "s", "sim", "ok", "permitir", "aprovar", "liberar", "allow":
		v = policy.Allow
	case "n", "no", "não", "nao", "negar", "rejeitar", "bloquear", "deny":
		v = policy.Deny
	case "a", "always", "sempre", "all", "todos", "auto", "autonomo", "autônomo":
		v, auto = policy.Allow, true
	default:
		return "", false
	}
	pending := b.approvals.Pending()
	var id string
	switch {
	case len(f) == 2:
		id = f[1]
	case len(pending) == 1:
		id = pending[0].ID
	case len(pending) == 0:
		return "No pending approvals.", true
	default:
		return "More than one approval is pending: reply with the id, e.g. y " + pending[0].ID, true
	}
	d := b.Decide(ctx, id, Decision{Verdict: v, Reason: "answered on " + origin, By: origin}, auto)
	if d.By == "bridge" {
		return "Approval " + id + " not found (already decided or expired).", true
	}
	return "", true // ApprovalResolved already notified every channel
}

func (b *Bridge) resolve(ctx context.Context, ref string) (model.Session, error) {
	sess, err := b.store.Resolve(ctx, ref)
	var amb *store.ErrAmbiguous
	switch {
	case errors.Is(err, store.ErrNotFound):
		return sess, fmt.Errorf("no session matches %q", ref)
	case errors.As(err, &amb):
		return sess, fmt.Errorf("%q matches %d sessions, use more characters", ref, len(amb.Matches))
	}
	return sess, err
}

func expandRunner(tmpl []string, prompt string) []string {
	argv := make([]string, len(tmpl))
	for i, a := range tmpl {
		argv[i] = strings.ReplaceAll(a, "{prompt}", prompt)
	}
	return argv
}

func shortWS(p string) string {
	if p == "" {
		return "no workspace"
	}
	return hook.ShortenHome(p)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}

// Ago renders a compact relative time ("3m ago", "2d ago").
func Ago(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}
