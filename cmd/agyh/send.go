package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
)

func sendCmd() *cobra.Command {
	var caption, agent, session string
	cmd := &cobra.Command{
		Use:   "send <file>... [-m message]",
		Short: "Send files to your phone (Telegram/WhatsApp) through the bridge",
		Long: `Sends files to every connected chat. Agents use it to hand you what they
made (a PDF, a screenshot, a report).

  - Files inside the current workspace, a configured workspace, the temp dir
    or files.allow are sent right away.
  - Anything else waits for your approval on the phone.
  - Secrets are never sent: keys, .env files, credential stores, files under
    .ssh/.aws/.git and agyh's own config and data.

Telegram takes files up to 50 MB, WhatsApp up to 100 MB (files.max_mb caps both).`,
		Example: `  agyh send report.pdf
  agyh send out/*.png -m "telas novas do checkout"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.Bridge.Secret == "" {
				return errors.New("no bridge configured: run `agyh connect telegram` (or whatsapp), then `agyh bridge`")
			}
			cwd, _ := os.Getwd()
			if agent == "" {
				agent = callingAgent()
			}
			results, err := bridge.NewClient(cfg).SendFiles(cmd.Context(), bridge.FileRequest{
				Paths: args, Caption: caption, Cwd: cwd, Agent: agent, SessionID: session})
			if err != nil {
				return err
			}
			failed := 0
			for _, r := range results {
				if r.Error != "" {
					failed++
					fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s: %s\n", r.Path, r.Error)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s → %v\n", r.Path, r.Sent)
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d file(s) not sent", failed, len(results))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&caption, "message", "m", "", "caption sent with the files")
	cmd.Flags().StringVar(&agent, "agent", "", "who is sending, shown on approvals (default: detected)")
	cmd.Flags().StringVar(&session, "session", "", "agent session id (an approval's ⚡ then covers that session)")
	return cmd
}

// callingAgent guesses which agent runs this command, for the approval
// message, from variables the agents set for their tools.
func callingAgent() string {
	switch {
	case os.Getenv("CLAUDECODE") != "":
		return "claude"
	case os.Getenv("CODEX_SANDBOX") != "" || os.Getenv("CODEX_SANDBOX_NETWORK_DISABLED") != "":
		return "codex"
	}
	return ""
}
