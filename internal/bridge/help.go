package bridge

import (
	"fmt"
	"strings"
)

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

// help lists every advertised command once, with its names in both languages.
func (b *Bridge) help(m Markup) string {
	var sb strings.Builder
	sb.WriteString(m.Bold("agyh") + " — your coding-agent sessions, from anywhere / suas sessões de IA em qualquer lugar.\n\n")
	for _, c := range registry {
		if len(c.names) == 0 {
			continue
		}
		line := "/" + strings.Join(c.names, " · /")
		if c.args != "" {
			line += " " + c.args
		}
		fmt.Fprintf(&sb, "%s — %s\n", m.Escape(line), m.Escape(c.help))
	}
	sb.WriteString("\n" + m.Bold("Aprovações / Approvals:") + " Toque nos botões ou responda " + m.Code("s <id>") + " (sim/allow), " +
		m.Code("n <id>") + " (não/deny) ou " + m.Code("a <id>") + " (autônomo/always).")
	return sb.String()
}
