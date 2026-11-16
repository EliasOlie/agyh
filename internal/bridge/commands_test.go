package bridge

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	vaultsync "github.com/EliasOlie/agyh/internal/sync"
)

func TestCommandRegistry(t *testing.T) {
	b, _, _, _ := newBridge(t, 5)
	m := waMarkup{}
	help := b.Execute(context.Background(), m, "test", "/help")

	telegramSafe := regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	advertised := 0
	for _, c := range registry {
		if c.run == nil {
			t.Fatalf("command %v has no handler", c.names)
		}
		for _, n := range c.names {
			advertised++
			if !telegramSafe.MatchString(n) {
				t.Errorf("/%s is not a valid Telegram command name", n)
			}
			if c.help == "" {
				t.Errorf("/%s has no help text", n)
			}
			if !strings.Contains(help, "/"+n+" ") { // every name is followed by " ·", " <args>" or " —"
				t.Errorf("/help does not list /%s:\n%s", n, help)
			}
		}
	}
	if len(Commands) != advertised {
		t.Fatalf("Telegram menu has %d entries, want %d", len(Commands), advertised)
	}
	if strings.Contains(help, "/start") || strings.Contains(help, "/sessões") {
		t.Fatalf("aliases must not be advertised:\n%s", help)
	}

	// Every spelling the old switch accepted still routes somewhere.
	for _, name := range []string{"start", "help", "ajuda", "chats", "history", "sessoes", "sessões", "historico", "histórico",
		"search", "buscar", "pesquisar", "busca", "show", "ver", "exibir", "projects", "projetos", "project", "projeto",
		"save", "salvar", "new", "novo", "run", "iniciar", "ws", "cd", "workspaces", "pending", "pendentes", "fila",
		"auto", "autonomo", "autônomo", "sync", "sincronizar"} {
		if out := b.Execute(context.Background(), m, "test", "/"+strings.ToUpper(name[:1])+name[1:]); strings.Contains(out, "Unknown command") {
			t.Errorf("/%s no longer routes: %s", name, out)
		}
	}
}

func TestSyncRunsInBackground(t *testing.T) {
	b, _, _, cfg := newBridge(t, 5)
	ch := newFake("telegram")
	b.AddChannel(ch)
	m := waMarkup{}
	ctx := context.Background()

	if out := b.Execute(ctx, m, "telegram", "/sync"); !strings.Contains(out, "not configured") {
		t.Fatalf("disabled vault: %s", out)
	}
	cfg.Sync.Enabled = true

	release := make(chan error)
	b.vaultSync = func(ctx context.Context) (vaultsync.SyncResult, error) {
		return vaultsync.SyncResult{Uploaded: 2, Downloaded: 1, Total: 9}, <-release
	}

	start := time.Now()
	if out := b.Execute(ctx, m, "telegram", "/sync"); !strings.Contains(out, "Sync started") {
		t.Fatalf("/sync: %s", out)
	}
	if time.Since(start) > time.Second {
		t.Fatal("/sync blocked the chat handler")
	}
	if out := b.Execute(ctx, m, "telegram", "/sincronizar"); !strings.Contains(out, "already running") {
		t.Fatalf("overlapping /sync: %s", out)
	}
	// The channel keeps answering while the sync runs.
	if out := b.Execute(ctx, m, "telegram", "/pending"); !strings.Contains(out, "No pending approvals") {
		t.Fatalf("/pending during sync: %s", out)
	}

	release <- nil
	waitFor(t, func() bool { return strings.Contains(ch.last(), "2 uploaded, 1 downloaded (9 total)") })
	waitFor(t, func() bool { return !b.syncing.Load() })

	// Failures are reported too, and a new sync can start afterwards.
	if out := b.Execute(ctx, m, "telegram", "/sync"); !strings.Contains(out, "Sync started") {
		t.Fatalf("second /sync: %s", out)
	}
	release <- errors.New("bucket <gone>")
	waitFor(t, func() bool { return strings.Contains(ch.last(), "Sync failed") })
	if !strings.Contains(ch.last(), "bucket <gone>") {
		t.Fatalf("error not reported: %q", ch.last())
	}
}

func TestDefaultRunnerIsStable(t *testing.T) {
	b, _, _, cfg := newBridge(t, 5)
	cfg.Runners = map[string][]string{"zeta": {"z"}, "codex": {"c"}, "aider": {"a"}}
	for range 20 {
		if got := b.defaultRunner(); got != "aider" {
			t.Fatalf("defaultRunner = %q, want the alphabetically first", got)
		}
	}
	cfg.Runners["claude"] = []string{"claude"}
	if got := b.defaultRunner(); got != "claude" {
		t.Fatalf("claude must win when configured, got %q", got)
	}
	cfg.Runners = nil
	if got := b.defaultRunner(); got != "" {
		t.Fatalf("no runners: %q", got)
	}
}
