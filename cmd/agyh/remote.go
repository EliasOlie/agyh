package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
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
				StatePath: filepath.Join(store.DataDir(), "bridge-state.json")})

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
			_ = ch.Send(ctx, "🤖 <b>agyh connected.</b> Only this chat can control your agents. Send /help.")
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
	return &cobra.Command{
		Use:       "install-hooks <claude|agy>",
		Short:     "Print the hook configuration to paste into your agent's settings",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"claude", "agy"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg any
			where := ""
			switch args[0] {
			case "claude":
				where = "~/.claude/settings.json (merge into the \"hooks\" key)"
				cfg = map[string]any{"hooks": map[string]any{
					"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{
						"type": "command", "command": "agyh hook pre-tool-use --agent claude", "timeout": 180}}}},
					"Stop": []any{map[string]any{"hooks": []any{map[string]any{
						"type": "command", "command": "agyh hook stop --agent claude", "timeout": 15}}}},
				}}
			case "agy":
				where = "your Antigravity hooks file (e.g. ~/.agents/hooks.json)"
				cfg = map[string]any{
					"agyh-permission": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "run_command",
						"hooks": []any{map[string]any{"type": "command", "command": "agyh hook pre-tool-use --agent agy", "timeout": 180}}}}},
					"agyh-stop": map[string]any{"Stop": []any{map[string]any{
						"type": "command", "command": "agyh hook stop --agent agy", "timeout": 15}}},
				}
			default:
				return errors.New("supported agents: claude, agy")
			}
			b, _ := json.MarshalIndent(cfg, "", "  ")
			fmt.Fprintf(cmd.OutOrStdout(), "// Add to %s\n%s\n", where, b)
			return nil
		},
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
			return nil
		},
	}
}
