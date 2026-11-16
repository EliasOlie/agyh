package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/store"
	"github.com/EliasOlie/agyh/internal/sync"
)

// withVault loads the config, opens the index (re-synced first when reindex
// is set) and hands a vault engine to fn. Every sync command goes through it.
func (a *app) withVault(cmd *cobra.Command, reindex bool, fn func(e *sync.Engine, cfg *config.Config) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var st *store.Store
	if reindex {
		st, err = a.openSynced(cmd.Context(), false)
	} else {
		st, err = a.open()
	}
	if err != nil {
		return err
	}
	defer st.Close()
	engine, err := sync.NewEngine(cfg.Sync, st)
	if err != nil {
		return err
	}
	return fn(engine, cfg)
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) syncCmd() *cobra.Command {
	var (
		asJSON     bool
		statusOnly bool
		pushOnly   bool
		pullOnly   bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize sessions and projects with your self-hosted cloud vault (S3 / VPSQL)",
		Long: `Synchronizes your coding-agent sessions (Antigravity, Claude Code, Codex)
and projects with an encrypted S3-compatible cloud vault.

All sessions are compressed and encrypted client-side using AES-256-GCM.
Configure your endpoint, bucket and credentials in ~/.config/agyh/config.json
or via AGYH_SYNC_* environment variables.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withVault(cmd, true, func(engine *sync.Engine, cfg *config.Config) error {
				w, ctx := cmd.OutOrStdout(), cmd.Context()
				if statusOnly {
					return runSyncStatus(cmd, engine, asJSON)
				}
				var (
					res    sync.SyncResult
					err    error
					action = "Synced with cloud vault"
				)
				switch {
				case pushOnly:
					action = "Pushed to cloud vault"
					res, err = engine.Push(ctx)
				case pullOnly:
					action = "Pulled from cloud vault"
					res, err = engine.Pull(ctx)
				default:
					res, err = engine.Sync(ctx)
				}
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(w, res)
				}
				fmt.Fprintf(w, "✓ %s (%s/%s)\n", action, cfg.Sync.Endpoint, cfg.Sync.Bucket)
				fmt.Fprintf(w, "  Uploaded:   %d sessions\n", res.Uploaded)
				fmt.Fprintf(w, "  Downloaded: %d sessions\n", res.Downloaded)
				fmt.Fprintf(w, "  Total:      %d sessions in vault\n", res.Total)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&statusOnly, "status", false, "show sync status without transferring data")
	cmd.Flags().BoolVar(&pushOnly, "push", false, "only upload local sessions to vault")
	cmd.Flags().BoolVar(&pullOnly, "pull", false, "only download remote sessions from vault")

	cmd.AddCommand(
		a.syncSub("status", "Show differences between local store and cloud vault", true,
			func(cmd *cobra.Command, e *sync.Engine, asJSON bool) error { return runSyncStatus(cmd, e, asJSON) }),
		a.syncSub("push", "Force upload of all local sessions to cloud vault", true,
			transfer((*sync.Engine).Push, func(r sync.SyncResult) string {
				return fmt.Sprintf("✓ Pushed %d sessions to vault (%d total)", r.Uploaded, r.Total)
			})),
		a.syncSub("pull", "Download any missing or updated sessions from cloud vault", true,
			transfer((*sync.Engine).Pull, func(r sync.SyncResult) string {
				return fmt.Sprintf("✓ Pulled %d sessions from vault (%d total)", r.Downloaded, r.Total)
			})),
		// restore rebuilds the index from the vault, so it must not index first.
		a.syncSub("restore", "Reconstruct local database by restoring all sessions from cloud vault", false,
			transfer((*sync.Engine).Restore, func(r sync.SyncResult) string {
				return fmt.Sprintf("✓ Restored %d sessions from cloud vault into local index", r.Downloaded)
			})),
	)
	return cmd
}

// syncSub builds one `agyh sync <name>` subcommand with its own --json flag.
func (a *app) syncSub(name, short string, reindex bool, run func(cmd *cobra.Command, e *sync.Engine, asJSON bool) error) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withVault(cmd, reindex, func(e *sync.Engine, _ *config.Config) error { return run(cmd, e, asJSON) })
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

// transfer adapts an engine operation to a subcommand: JSON or a one-line summary.
func transfer(op func(*sync.Engine, context.Context) (sync.SyncResult, error), summary func(sync.SyncResult) string) func(*cobra.Command, *sync.Engine, bool) error {
	return func(cmd *cobra.Command, e *sync.Engine, asJSON bool) error {
		res, err := op(e, cmd.Context())
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(cmd.OutOrStdout(), res)
		}
		fmt.Fprintln(cmd.OutOrStdout(), summary(res))
		return nil
	}
}

func runSyncStatus(cmd *cobra.Command, e *sync.Engine, asJSON bool) error {
	report, err := e.Status(cmd.Context())
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(cmd.OutOrStdout(), report)
	}
	return printSyncStatus(cmd.OutOrStdout(), report)
}

func printSyncStatus(w io.Writer, r sync.StatusReport) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Endpoint:\t%s\n", r.Endpoint)
	fmt.Fprintf(tw, "Bucket:\t%s\n", r.Bucket)
	fmt.Fprintf(tw, "Local sessions:\t%d\n", r.LocalSessions)
	fmt.Fprintf(tw, "Remote vault:\t%d\n", r.RemoteSessions)
	fmt.Fprintf(tw, "To upload:\t%d\n", r.ToUpload)
	fmt.Fprintf(tw, "To download:\t%d\n", r.ToDownload)
	if !r.LastSynced.IsZero() {
		fmt.Fprintf(tw, "Last vault update:\t%s\n", bridge.Ago(r.LastSynced))
	}
	return tw.Flush()
}
