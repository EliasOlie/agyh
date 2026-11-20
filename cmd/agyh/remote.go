package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/hookinstall"
	"github.com/EliasOlie/agyh/internal/index"
	"github.com/EliasOlie/agyh/internal/policy"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

func whatsappDB() string { return filepath.Join(store.DataDir(), "whatsapp.db") }

func (a *app) bridgeCmd() *cobra.Command {
	var reindex time.Duration
	cmd := &cobra.Command{
		Use:   "bridge",
		Short: "Run the daemon that connects agent hooks to Telegram and WhatsApp",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if _, err := cfg.EnsureSecret(); err != nil {
				return err
			}
			pol, err := policy.New(cfg.Policy.Safe, cfg.Policy.Fatal)
			if err != nil {
				return err
			}
			st, err := a.openSynced(ctx, false)
			if err != nil {
				return err
			}
			defer st.Close()
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			b := bridge.New(bridge.Options{Config: cfg, Store: st, Policy: pol, Logger: log,
				StatePath:       filepath.Join(store.DataDir(), "bridge-state.json"),
				LedgerPath:      ledgerPath(),
				PropagationPath: filepath.Join(store.DataDir(), "propagation.json"),
				Inbox:           filepath.Join(store.DataDir(), "inbox"),
				Protected:       []string{config.Dir(), store.DataDir()}})

			if cfg.Telegram.Token != "" && cfg.Telegram.ChatID != 0 {
				b.AddChannel(bridge.NewTelegram(bridge.NewTelegramClient(cfg.Telegram.Token), cfg.Telegram.ChatID))
			}
			if cfg.WhatsApp.Enabled {
				wa, err := bridge.NewWhatsApp(ctx, whatsappDB())
				if err != nil {
					log.Warn("whatsapp disabled", "err", err)
				} else {
					b.AddChannel(wa)
				}
			}
			if len(b.Channels()) == 0 {
				fmt.Fprintln(os.Stderr, "agyh: no chat channel configured. Run `agyh connect telegram` or `agyh connect whatsapp`.")
				fmt.Fprintln(os.Stderr, "agyh: the bridge will still answer hooks (they fall back to the terminal).")
			}

			// Keep the index fresh so /chats and /search see new sessions.
			go func() {
				t := time.NewTicker(reindex)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						if _, err := index.Sync(ctx, st, source.All(), false); err != nil && ctx.Err() == nil {
							log.Warn("reindex failed", "err", err)
						}
					}
				}
			}()
			// Offer new or changed skills to the agents that lack them (opt-in).
			switch {
			case cfg.AutoPropagate && !cfg.ManageAgents:
				log.Warn(`"auto_propagate" needs "manage_agents": true; skill propagation is off`)
			case cfg.AutoPropagate:
				go func() {
					t := time.NewTicker(reindex)
					defer t.Stop()
					for {
						if err := b.PropagateSkills(ctx); err != nil && ctx.Err() == nil {
							log.Warn("skill propagation failed", "err", err)
						}
						select {
						case <-ctx.Done():
							return
						case <-t.C:
						}
					}
				}()
			}
			return b.Serve(ctx)
		},
	}
	cmd.Flags().DurationVar(&reindex, "reindex", 2*time.Minute, "how often to refresh the index")
	return cmd
}

func (a *app) connectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connect", Short: "Pair a chat channel (Telegram bot or WhatsApp linked device)"}

	var token string
	tg := &cobra.Command{
		Use:   "telegram",
		Short: "Connect a Telegram bot (create one with @BotFather first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if token == "" {
				token = os.Getenv("AGYH_TELEGRAM_TOKEN")
			}
			if token == "" {
				fmt.Fprint(out, "Bot token from @BotFather (input hidden): ")
				token, err = readSecret()
				fmt.Fprintln(out)
				if err != nil {
					return err
				}
			}
			client := bridge.NewTelegramClient(strings.TrimSpace(token))
			me, err := client.GetMe(ctx)
			if err != nil {
				return fmt.Errorf("token rejected by Telegram: %w", err)
			}
			fmt.Fprintf(out, "✓ Bot @%s found.\nNow open https://t.me/%s on your phone and send /start (waiting up to 3 minutes)…\n", me.Username, me.Username)
			wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			chatID, from, err := client.FirstChat(wctx)
			if err != nil {
				return fmt.Errorf("no message received: %w", err)
			}
			cfg.Telegram = config.Telegram{Token: client.Token, ChatID: chatID}
			if _, err := cfg.EnsureSecret(); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			ch := bridge.NewTelegram(client, chatID)
			_ = ch.Send(ctx, "🤖 <b>agyh conectado com sucesso! / Connected successfully!</b>\n\nApenas este chat pode controlar seus agentes.\nEnvie /start ou /ajuda para ver o guia de comandos e aprovações.")
			fmt.Fprintf(out, "✓ Paired with %s (chat %d). Saved to %s\nStart the daemon with: agyh bridge\n", displayName(from), chatID, config.Path())
			return nil
		},
	}
	tg.Flags().StringVar(&token, "token", "", "bot token (or set AGYH_TELEGRAM_TOKEN; prompted if empty)")

	wa := &cobra.Command{
		Use:   "whatsapp",
		Short: "Link agyh as a WhatsApp device by scanning a QR code in the terminal",
		Long: `Links agyh to your WhatsApp account as a linked device (like WhatsApp Web).
You then control it from the "Message yourself" chat.

This uses an unofficial client (whatsmeow). WhatsApp may restrict accounts that
use unofficial clients: prefer a secondary number. Telegram is the safer channel.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(store.DataDir(), 0o700); err != nil {
				return err
			}
			jid, err := bridge.PairWhatsApp(cmd.Context(), whatsappDB(), cmd.OutOrStdout())
			if err != nil {
				return err
			}
			_ = os.Chmod(whatsappDB(), 0o600)
			cfg.WhatsApp = config.WhatsApp{Enabled: true, JID: jid}
			if _, err := cfg.EnsureSecret(); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n✓ WhatsApp linked (%s). Open the chat with yourself and send /help.\nStart the daemon with: agyh bridge\n", jid)
			return nil
		},
	}
	cmd.AddCommand(tg, wa)
	return cmd
}

func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func displayName(u bridge.TgUser) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return fmt.Sprintf("user %d", u.ID)
}

func installHooksCmd() *cobra.Command {
	var apply bool
	var bin string
	cmd := &cobra.Command{
		Use:   "install-hooks [claude|agy|codex|all]",
		Short: "Install agyh's hooks into your agents (or print them)",
		Long: `Without --apply, prints the hook configuration for each agent. With
--apply, writes it into the agent's settings: earlier agyh hooks are
replaced, hooks that run a program that no longer exists are removed, every
other setting is kept and the previous file is saved as *.agyh-backup.

With no agent (or "all"), every agent found on this machine.

  claude  ~/.claude/settings.json
  agy     ~/.agents/hooks.json (and broken hooks in ~/.gemini/config/hooks.json)
  codex   ~/.codex/hooks.json  (Codex asks you to trust new hooks: run /hooks in codex)

--apply also installs the agyh-send skill (how to send files to your phone)
for Claude Code and Codex (a copy you edited is left alone). Antigravity has
no folder for user skills: tell it in ~/.gemini/config/AGENTS.md, e.g.
"To send me a file, run: agyh send <file> -m <caption>".`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: append([]string{"all"}, hookinstall.Agents...),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			paths := hookinstall.DefaultPaths()
			var agents []string
			switch {
			case len(args) == 1 && args[0] != "all":
				if !slices.Contains(hookinstall.Agents, args[0]) {
					return fmt.Errorf("supported agents: %s", strings.Join(hookinstall.Agents, ", "))
				}
				agents = []string{args[0]}
			default:
				for _, a := range hookinstall.Agents {
					if paths.Detected(a) {
						agents = append(agents, a)
					}
				}
				if len(agents) == 0 {
					return errors.New("no agent found (claude, agy, codex): name one to install anyway")
				}
			}
			if bin == "" {
				bin = selfPath()
			}
			if !apply {
				for _, a := range agents {
					snippet, _ := hookinstall.Snippet(a, bin)
					b, _ := json.MarshalIndent(snippet, "", "  ")
					fmt.Fprintf(out, "// %s: merge into %s\n%s\n\n", a, hook.ShortenHome(paths.File(a)), b)
				}
				fmt.Fprintln(out, "// Or let agyh write it: agyh install-hooks --apply")
				printStale(out, paths, true)
				return nil
			}
			for _, a := range agents {
				res, err := hookinstall.Install(paths, a, bin)
				if err != nil {
					return fmt.Errorf("%s: %w", a, err)
				}
				fmt.Fprintf(out, "✓ %-6s hooks in %s", a, hook.ShortenHome(res.File))
				if res.Backup != "" {
					fmt.Fprintf(out, " (previous file: %s)", hook.ShortenHome(res.Backup))
				}
				fmt.Fprintln(out)
				for _, s := range res.Removed {
					fmt.Fprintf(out, "  removed a broken hook: %s\n", s.Command)
				}
				if a == "agy" {
					removed, backup, err := hookinstall.RemoveStale(paths, paths.AgyLegacy)
					if err != nil {
						return err
					}
					for _, s := range removed {
						fmt.Fprintf(out, "  removed a broken hook from %s: %s\n", hook.ShortenHome(s.File), s.Command)
					}
					if backup != "" {
						fmt.Fprintf(out, "  (previous file: %s)\n", hook.ShortenHome(backup))
					}
				}
				if a == "codex" {
					fmt.Fprintln(out, "  Codex runs new hooks only after you trust them: open codex and run /hooks.")
				}
				// The skill that teaches the agent `agyh send` (files to your phone).
				if p, status, err := hookinstall.InstallSkill(paths, a); err != nil {
					fmt.Fprintf(out, "  ! skill %s: %v\n", hookinstall.SkillName, err)
				} else if p != "" {
					fmt.Fprintf(out, "  skill %s: %s (%s)\n", hookinstall.SkillName, status, hook.ShortenHome(p))
				}
			}
			printStale(out, paths, false)
			fmt.Fprintln(out, "\nStart a new agent session for the hooks to apply.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write the hooks into the agents' settings")
	cmd.Flags().StringVar(&bin, "bin", "", "agyh binary the hooks call (default: this one)")
	return cmd
}

// selfPath is this binary's absolute path: hooks run with the agent's PATH,
// which often lacks ~/.local/bin or ~/go/bin.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "agyh"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if strings.Contains(p, "go-build") { // `go run`: the binary is temporary
		return "agyh"
	}
	return p
}

// printStale warns about hooks that run a missing program: the agent fails
// every tool call they match.
func printStale(out io.Writer, paths hookinstall.Paths, suggest bool) {
	stale, err := hookinstall.FindStale(paths)
	if err != nil {
		fmt.Fprintf(out, "\n⚠ %v\n", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	fmt.Fprintln(out, "\n⚠ Hooks that run a program that no longer exists (the agent fails the tool calls they match):")
	for _, s := range stale {
		fmt.Fprintf(out, "  %s: %s\n", hook.ShortenHome(s.File), s.Command)
	}
	if suggest {
		fmt.Fprintln(out, "  Remove them with: agyh install-hooks --apply")
	}
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check agent data, index, channels and the bridge",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			ok := func(b bool) string {
				if b {
					return "✓"
				}
				return "✗"
			}
			fmt.Fprintln(out, "Agents")
			for _, s := range source.All() {
				fmt.Fprintf(out, "  %s %s\n", ok(s.Available()), s.Name())
			}
			st, err := a.openSynced(ctx, false)
			if err != nil {
				return err
			}
			counts, _ := st.Count(ctx)
			st.Close()
			fmt.Fprintf(out, "Index  %s\n", a.dbPath)
			for k, v := range counts {
				fmt.Fprintf(out, "  %s: %d sessions\n", k, v)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Config %s\n", config.Path())
			fmt.Fprintf(out, "  %s telegram\n", ok(cfg.Telegram.Token != "" && cfg.Telegram.ChatID != 0))
			_, waErr := os.Stat(whatsappDB())
			fmt.Fprintf(out, "  %s whatsapp\n", ok(cfg.WhatsApp.Enabled && waErr == nil))
			fmt.Fprintf(out, "  %s cloud vault (%s)\n", ok(cfg.Sync.Enabled), cfg.Sync.Endpoint)
			fmt.Fprintf(out, "  %s bridge running on %s\n", ok(cfg.Bridge.Secret != "" && bridge.NewClient(cfg).Available(ctx)), cfg.Bridge.Addr)
			stale, _ := hookinstall.FindStale(hookinstall.DefaultPaths())
			fmt.Fprintf(out, "Hooks\n  %s no broken agent hooks\n", ok(len(stale) == 0))
			printStale(out, hookinstall.DefaultPaths(), true)
			return nil
		},
	}
}
