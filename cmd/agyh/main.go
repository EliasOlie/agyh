// Command agyh: search, organize, resume and remote-control your coding-agent
// sessions (Antigravity, Claude Code, Codex) from the terminal, Telegram or WhatsApp.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/index"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
	"github.com/EliasOlie/agyh/internal/tui"
)

var version = "dev"

type app struct {
	dbPath string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "agyh:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	a := &app{}
	var project string
	root := &cobra.Command{
		Use:   "agyh [search words]",
		Short: "Search, organize, resume and remote-control your coding-agent sessions",
		Long: `agyh indexes the local history of your coding agents (Antigravity, Claude Code,
Codex) into one searchable place, lets you group sessions into projects, resume
any of them, and approve agent actions from Telegram or WhatsApp.

Run without arguments for the interactive browser.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			res, err := tui.Run(cmd.Context(), st, joinArgs(args), project)
			st.Close()
			if err != nil {
				return err
			}
			if res.Resume != nil {
				return resume(*res.Resume)
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&a.dbPath, "db", store.DefaultPath(), "index database path")
	root.Flags().StringVarP(&project, "project", "p", "", "open the browser inside one project")
	root.AddCommand(
		a.lsCmd(), a.searchCmd(), a.showCmd(), a.resumeCmd(), a.indexCmd(), a.projectCmd(),
		a.hookCmd(), a.bridgeCmd(), a.connectCmd(), installHooksCmd(), a.doctorCmd(),
	)
	return root
}

func (a *app) open() (*store.Store, error) { return store.Open(a.dbPath) }

// openSynced opens the index and brings it up to date (fast: only changed files are parsed).
func (a *app) openSynced(ctx context.Context, full bool) (*store.Store, error) {
	st, err := a.open()
	if err != nil {
		return nil, err
	}
	if _, err := index.Sync(ctx, st, source.All(), full); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "agyh: index warning:", err)
	}
	return st, nil
}
