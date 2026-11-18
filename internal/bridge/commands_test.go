package bridge

import (
	"context"
	"errors"
	"github.com/EliasOlie/agyh/internal/model"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
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

func TestNewWithEveryAgentAndProjectColors(t *testing.T) {
	b, st, _, cfg := newBridge(t, 5)
	ch := newFake("telegram")
	b.AddChannel(ch)
	cfg.Runners = config.Default().Runners
	r := &fakeRunner{done: make(chan struct{})}
	b.runner = r
	m := waMarkup{}
	ctx := context.Background()

	if out := b.Execute(ctx, m, "telegram", "/new agy @api fix the build"); !strings.Contains(out, "Started") {
		t.Fatalf("/new agy: %s", out)
	}
	<-r.done
	r.mu.Lock()
	if strings.Join(r.argv, "|") != "agy|-p|fix the build" || r.dir != "/w/api" {
		t.Fatalf("agy runner: %q in %s", r.argv, r.dir)
	}
	r.mu.Unlock()

	cfg.Runners["codex"] = nil // an empty runner disables the agent
	r2 := &fakeRunner{done: make(chan struct{})}
	b.runner = r2
	b.Execute(ctx, m, "telegram", "/new codex hello")
	<-r2.done
	r2.mu.Lock()
	if r2.argv[0] != "claude" || r2.argv[len(r2.argv)-1] != "codex hello" {
		t.Fatalf("a disabled runner is just a word of the prompt: %q", r2.argv)
	}
	r2.mu.Unlock()

	st.EnsureProject(ctx, "pix")
	st.SetProjectColor(ctx, "pix", "red")
	if out := b.Execute(ctx, m, "telegram", "/projects"); !strings.Contains(out, "🟥") {
		t.Fatalf("/projects should show the project's color: %s", out)
	}
}

func TestSaveSteersAwayFromDuplicates(t *testing.T) {
	b, st, _, _ := newBridge(t, 5)
	ctx := context.Background()
	now := time.Now()
	st.Upsert(ctx, model.Session{Agent: "claude", ID: "aaaa1111bbbb", Title: "x", Path: "/p", UpdatedAt: now, ModTime: now}, nil)
	st.CreateProject(ctx, "conecta-me", "")
	out := b.Execute(ctx, waMarkup{}, "telegram", "/save aaaa1111 conectame")
	if !strings.Contains(out, "looks like the existing") || !strings.Contains(out, "conecta-me") {
		t.Fatalf("/save similar: %s", out)
	}
	if ps, _ := st.Projects(ctx); len(ps) != 1 {
		t.Fatalf("no duplicate may be created: %+v", ps)
	}
}
